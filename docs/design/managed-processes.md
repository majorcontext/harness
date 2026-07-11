# Managed processes: dev servers without ceremony

## Motivation

An agent working on a web app routinely needs a long-lived process —
`pnpm dev`, a local database, a worker — kept running *across* tool calls
while it edits code and drives a browser or `curl` against it. The `bash`
tool is deliberately turn-scoped (see `engine/bash.go`): a backgrounded
child surviving past the tool call is treated as a leftover, not a managed
resource, and nothing tells the model afterward that it is still there, or
where its output went. This is the write-up for the primitive that fixes
both halves of that gap: a named, supervised process the model can
start/stop/inspect without inventing its own PID-tracking and log-file
conventions turn after turn, and — the more novel half — a model that
**never has to ask** what is running, because request assembly tells it.

It is a build spec in the same register as `docs/design/context-
compaction.md` and `docs/design/fleet-model.md` — states, wire shapes, and
invariants, matched by the implementation landing in the same change.

## 1. Config

```json
{
  "processes": {
    "dev": {
      "command": ["pnpm", "dev"],
      "dir": "apps/app",
      "env": ["K=V"],
      "ready_regex": "Ready in .*ms",
      "ready_timeout_s": 60
    }
  }
}
```

`command` is required (a non-empty argv, resolved via PATH like any exec —
never run through a shell). `dir`, when set, is resolved against the
engine's working directory. `env` entries are appended to the harness
environment. `ready_regex`, when set, must compile (RE2 — Go's `regexp`);
an invalid pattern is a config-load error, not a first-start surprise.
`ready_timeout_s` defaults to 60 when omitted or `<= 0`. Merge rules mirror
`mcp_servers`: keys merge across the user/project config layers, but a
same-name project entry replaces the user entry wholesale (see
`config.ProcessSpec` and `validateProcesses`).

## 2. Engine: `*process.Manager`

`package process` (not `engine` — kept separate so its subprocess/exec
plumbing doesn't bloat the engine package, mirroring how `package mcp`
sits beside `engine/mcp.go`) owns every managed process's definition and
live OS-process handle. `*process.Manager` is a **box-scoped singleton**:
built once per harness process and shared across every session it hosts,
exactly like `engine.MCPManager` — two sessions in the same `harness
serve` process starting the process named `dev` are starting *the same*
process, not two independent ones.

### States

```
starting → ready → exited
   ↓         ↑        ↑
   → running ┘        |
   ↓                   |
   → (killed) ─────→ stopped
```

- **starting** — spawned, a `ready_regex` is configured, and no log line
  has matched it yet. Transient: a client only ever observes this via a
  concurrent `Status()` call while some other caller's blocking `Start` is
  still waiting (see below).
- **ready** — a `ready_regex` matched a combined stdout+stderr log line,
  *or* no `ready_regex` was configured at all (ready immediately on
  spawn).
- **running** — a `ready_regex` was configured but `ready_timeout_s`
  elapsed before any line matched. The process is **never killed by a
  timeout** — it is left running, and a `ready_regex` match observed later
  (the watcher keeps scanning) still flips this to `ready`.
- **exited** — the process terminated on its own, detected asynchronously
  by a waiter goroutine (`cmd.Wait()`), independent of any client asking.
- **stopped** — a client called `Stop`, which killed the process
  intentionally.

`exited` vs `stopped` is the one distinction the state machine insists on:
"did this process die because someone told it to, or on its own" is
exactly the fact an agent debugging a crash needs, and it would be lost if
both collapsed to one terminal state.

### Actions

- **Start(ctx, name)** — idempotent: an already-active (`starting`,
  `ready`, or `running`) process returns its current status unchanged, no
  second process spawned. Otherwise: spawn, stream combined stdout+stderr
  to the log file (see §3), and:
  - no `ready_regex`: return `ready` immediately.
  - a `ready_regex`: **block** until a log line matches (`ready`) or
    `ready_timeout_s` elapses (`running`, with a `Note` explaining the
    timeout).
- **Stop(ctx, name)** — unix: SIGKILL the whole process group (mirroring
  `engine/bash_unix.go`'s `Setpgid`/kill-pgroup/retry-window pattern, so a
  backgrounded grandchild dies with it); non-unix: a plain `Kill`. Either
  way, waits for the waiter goroutine to reap the process (bounded by
  `cmd.WaitDelay`, the same pipe-hang guard `engine/bash.go` uses) and
  records the exit. A no-op, not an error, if the process is not currently
  active.
- **Restart(ctx, name)** — `Stop` then `Start`.
- **Status(name)** — a point-in-time snapshot: state, pid, `started_at`,
  `exit_code`+`finished_at` once reaped, the log path (**always**
  populated, even for a never-started process — it is the path a future
  `Start` will write to), and a `ready` flag.
- **Logs(name, tail)** — the last `tail` lines of the log file (empty, not
  an error, if the process was never started).

### Death detection

A waiter goroutine calls `cmd.Wait()` exactly once per spawn and, the
moment it returns, records the exit and flips the state to `exited` (or
`stopped`, if `Stop` set that intent first) — **without any client
asking**. A subsequent `Status`/`Logs`/the ambient block (§4) all see the
transition the next time they read it; nothing polls the OS.

### Log files

`<workDir>/.harness/proc/<name>.log` — append mode, parent directories
created on first spawn. Every `Status` response carries this path (whether
or not the process has ever run) so a caller never has to reconstruct it.

### Reflection and runtime declaration

A definition's **origin** is `config` (loaded at startup) or `runtime`
(registered via the `process` tool's `declare` action, §3). Runtime
declarations are **server-lifetime only** — never written to
`.harness.json`; the tool's static description says so explicitly.
`Declare` validates identically to config parsing (same error text for an
empty argv or an uncompilable `ready_regex`); redeclaring a `config`-origin
name is always rejected; redeclaring a `runtime`-origin name that is not
currently active replaces it; one that is active must be stopped first.
`Undeclare` mirrors this: a `runtime`-origin, non-active definition can be
removed; a `config`-origin one, or an active one, cannot.

## 3. Session tool: `process`

Exposed whenever a session's `engine.Config.Processes` is non-nil.
`harness run` keeps the zero-cost-when-unconfigured rule from an earlier
draft of this design (nil, hence no tool at all, when the config's
`processes` section is empty). **`harness serve` is a deliberate
exception**: it always builds a `*process.Manager`, even with zero
declared processes, so the tool (and the HTTP endpoints, §5) are present
on every served box. The tradeoff: a box with nothing configured still
spends one tool slot on `process` in serve mode, in exchange for `declare`
being usable from turn one without a restart — a served box is exactly the
long-lived, orchestrator-facing case where an agent registering an ad hoc
process at runtime (a one-off script it wants supervised, not just a
`bash` call) is the common case, unlike a one-shot `harness run`.

Actions: `start(name)`, `stop(name)`, `restart(name)`, `status(name)`,
`logs(name, tail=50)`, `list()`, `declare(name, command, dir?, env?,
ready_regex?, ready_timeout_s?)`, `undeclare(name)`. Results are structured
JSON strings: `{name, state, pid, ready, log, elapsed, exit_code, note}`
for the lifecycle actions (`logs` adds a `logs` field with the tail
content; `list` returns the full roster with `origin`, `command`, `dir`,
`env_names` — **never env values** — `ready_regex`, `ready_timeout`, and
`status`). `start` blocks on the ready gate exactly like `Manager.Start`,
so one tool call yields a definitive, not-still-pending answer.

**The tool's `Description` is computed once, at tool-build time, from the
config-declared roster only** (name, command, dir) and never rewritten
afterward — a runtime `declare` changes what `list` and the ambient block
report, never the tool description itself. This is what keeps the
description safe to sit in a cached system-prompt/tool-list prefix: it is
exactly as stable as the bash tool's.

## 4. Ambient status injection

The point of this design is that the model should not have to call
`status` just to know what it already started. **Once at least one
declared process has EVER been started, for this server process's
lifetime**, every subsequent request-assembly appends an ephemeral status
block to the *newest* user message:

```
[processes: dev ready 14m log=.harness/proc/dev.log | db exited(1) 2m ago log=.harness/proc/db.log]
```

One token per process that has *itself* ever been started (a declared but
never-started process is omitted even once the block starts appearing for
others). `ready`/`running`/`starting` report elapsed time since start;
`exited`/`stopped` report elapsed time since finish, suffixed `ago`, and
`exited` additionally carries the exit code. The log path is relativized
against the session's working directory when possible.

### Where this rides, and why it is safe

Request assembly builds the provider request from the session's durable
history on every turn (`streamTurn`, `engine/engine.go`) — `s.History()`
already returns a **fresh copy** of the message slice (never the same
backing array the durable `s.history` field owns). The injection clones
*only* the last `RoleUser` message in that fresh copy — a new `Parts`
slice with one appended `*message.Text` part — and never touches any
earlier message. Three things fall out of that:

1. **Never persisted.** `s.append` (the sole path into durable history)
   already ran before `streamTurn` ever sees the message; the clone lives
   only in the local `messages` slice handed to `provider.Request`, which
   is discarded after the call. A resumed session (`LoadSession`) replays
   only what was actually appended — the block was never there.
2. **Only the newest message changes.** Every earlier message in the
   request is byte-identical to a request built before any process was
   ever started, which is what keeps a provider's prompt cache warm (the
   same reasoning `provider/anthropic/transcode.go`'s cache-marker
   placement already depends on).
3. **The goal loop needs no special-casing.** `Session.PursueGoal`'s
   worker turns are ordinary `Prompt` calls; the injection point is inside
   `Prompt`'s own `streamTurn`, so a goal-driven worker turn sees the exact
   same ambient block a direct `Prompt` call would.

This is the same "ephemeral suffix rides the request, never the durable
log" shape `docs/design/context-compaction.md` discusses for why a
synthesized summary message is a real, persisted `RoleUser` message (it
must survive reload) while something like this status block must NOT be —
the two designs sit on opposite sides of the same durability boundary on
purpose.

## 5. HTTP API

`GET /process` — every declared process (config- and runtime-origin
alike) with its live status. Never 404s: a server with no processes
configured answers `[]`.

`POST /process/{name}/start`, `/stop`, `/restart` — same semantics as the
`Manager` methods, run-token authenticated like every other endpoint. An
unknown name is 404 (also true when `Processes` is not configured at
all — from a caller's perspective "no such process" and "nothing is
configured" are the same observable fact).

See `server/openapi.yaml` for the full schema (`ProcessStatus`,
`ProcessInfo`).

## 6. Non-goals

- **No process supervision policy** (restart-on-crash, backoff, health
  checks beyond the one-shot ready gate). `exited` is a terminal,
  reported fact; nothing in this design restarts a process that died on
  its own.
- **No stdin.** A managed process's stdin is not wired to anything (the
  null device, like an unattended `exec.Cmd`) — a process that blocks
  waiting for stdin input will simply never produce output past that
  point. Managed processes are servers/workers, not interactive tools.
- **No cross-box process migration.** Like everything else in the fleet
  model, a managed process is box-scoped; it does not survive the box
  dying, and nothing here tries to make it.
