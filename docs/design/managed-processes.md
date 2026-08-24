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
      "ports": [3000],
      "ready_regex": "Ready in .*ms",
      "ready_timeout_s": 60
    }
  }
}
```

`command` is required (a non-empty argv, resolved via PATH like any exec —
never run through a shell). `dir`, when set, is resolved against the
engine's working directory. `env` entries are appended to the harness
environment. `ports` (§1a) is optional, purely declarative metadata.
`ready_timeout_s` defaults to 60 when omitted or `<= 0`. Merge rules mirror
`mcp_servers`: keys merge across the user/project config layers, but a
same-name project entry replaces the user entry wholesale (see
`config.ProcessSpec` and `validateProcesses`).

### 1a. Ports: declarative metadata, not enforcement

`ports` is a list of TCP port numbers (each validated to `1-65535` at
config-load time, same "fail loudly, not on first start" philosophy as
`ready_regex`) this process is expected to listen on. Harness does
**nothing** with this beyond carrying it through to every place a caller
might want to know: `process.Status`/`process.Info` (hence `GET /process`
and every lifecycle action's JSON result), the `process` tool's
`list`/`status` output, and the ambient status block (§4) — rendered as a
`:3000` (or `:3000,3001` for more than one) token right after the state,
e.g. `dev ready :3000 14m log=...`. It is never allocated, bound, dialed
(outside of `ready_port`, §1b, which is a separate opt-in field), or
enforced in any way — a process can listen on a port it never declared,
or declare one it never opens, and harness will not notice either way.
The sole purpose is telling an agent (or an operator reading `GET
/process`) where a dev server answers without it having to read the
process's own source or config.

### 1b. Ready check types: `ready_regex` / `ready_port` / `ready_http`

`ready_regex` (unchanged from the original design: a RE2 pattern matched
against a combined stdout+stderr log line) is one of three mutually
exclusive ways to gate `Start`'s block — **at most one** may be set per
definition; setting more than one is a config-load error (`at most one of
ready_regex, ready_port, ready_http may be set`), and `Manager.Declare`
raises the identical error text for a runtime `declare` (both call
`process.ValidateDef` directly — see §2).

- **`ready_port`** (int, `1-65535`): `Start` blocks until a plain TCP dial
  to `127.0.0.1:<ready_port>` succeeds.
- **`ready_http`** (string, a URL): `Start` blocks until a GET to it
  returns any non-5xx status — deliberately looser than "returns 200": a
  404 or a redirect still proves the process is up and serving something,
  which is the only fact this gate cares about.

**Why not just `ready_regex` for everything?** A log-regex gate reads
whatever the process's own stdout+stderr happens to say, and in a
multiplexed runner — Turborepo/pnpm workspace's `dev` script running a
dozen tasks in parallel, each line prefixed with its own task name — nothing
stops a *different* task's line from matching a `ready_regex` written to
watch for one specific task's "compiled successfully" message. A port or
HTTP probe has no such ambiguity: it asks the one question that actually
matters ("can I connect to what I expect to be there"), independent of
which of N processes happened to log something that looked similar.
`ready_regex` remains the right (and only necessary) choice for a process
whose "ready" signal is inherently a log line and nothing else (a CLI
tool, a one-shot migration runner) — this is an additional option, not a
deprecation.

**Mechanics.** Unlike the `ready_regex` watcher, which matches inline as
bytes stream through `cmd.Stdout` (nothing to poll — the process's own
output arrives when it arrives), a TCP dial or HTTP GET has nothing to
subscribe to, so `ready_port`/`ready_http` are driven by `pollReady`: check
immediately, then every `readyPollInterval` (250ms — modest enough not to
meaningfully delay a fast-starting process, without hammering the target)
until the check succeeds or the process exits first (the poller goroutine
is wired to the same `doneCh` the waiter goroutine closes, so it can never
outlive the process it is polling). `ready_timeout_s` semantics are
identical across all three gate types: elapsing it flips `starting` to
`running` with a `Note` explaining the timeout, the process is **never
killed by a timeout**, and the gate (regex watcher or poller alike) keeps
running in the background — a late match/dial/GET success still flips
`running` to `ready`.

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

- **starting** — spawned, a ready gate (`ready_regex`/`ready_port`/
  `ready_http`, §1b) is configured, and it has not yet been satisfied.
  Transient: a client only ever observes this via a concurrent `Status()`
  call while some other caller's blocking `Start` is still waiting (see
  below).
- **ready** — the configured ready gate was satisfied (a `ready_regex`
  match, a successful `ready_port` dial, or a non-5xx `ready_http` GET),
  *or* no ready gate was configured at all (ready immediately on spawn).
- **running** — a ready gate was configured but `ready_timeout_s` elapsed
  before it was satisfied. The process is **never killed by a timeout** —
  it is left running, and the gate being satisfied later (the regex
  watcher keeps scanning; the port/HTTP poller keeps polling) still flips
  this to `ready`.
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
  - no ready gate configured: return `ready` immediately.
  - a ready gate configured (`ready_regex`/`ready_port`/`ready_http`):
    **block** until it is satisfied (`ready`) or `ready_timeout_s` elapses
    (`running`, with a `Note` explaining the timeout).
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
`Declare` validates identically to config parsing — literally the same
function, `process.ValidateDef`, called from both `Manager.Declare` and
`config.validateProcesses` (same error text for an empty argv, an
out-of-range `ports`/`ready_port` entry, an uncompilable `ready_regex`, an
unparseable `ready_http` URL, or more than one ready gate set);
redeclaring a `config`-origin name is always rejected; redeclaring a
`runtime`-origin name that is not currently active replaces it; one that
is active must be stopped first. `Undeclare` mirrors this: a
`runtime`-origin, non-active definition can be removed; a `config`-origin
one, or an active one, cannot.

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
ports?, ready_regex?, ready_port?, ready_http?, ready_timeout_s?)`,
`undeclare(name)`. Results are structured JSON strings: `{name, state,
pid, ready, log, elapsed, exit_code, note, ports}` for the lifecycle
actions (`logs` adds a `logs` field with the tail content; `list` returns
the full roster with `origin`, `command`, `dir`, `env_names` — **never env
values** — `ports`, `ready_regex`, `ready_port`, `ready_http`,
`ready_timeout`, and `status`). `start` blocks on the ready gate exactly
like `Manager.Start`, so one tool call yields a definitive, not-still-
pending answer. `declare` validates identically to config parsing (§1b),
so passing more than one of `ready_regex`/`ready_port`/`ready_http` is
rejected with the same error a config file would get.

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
[processes: dev ready :3000 14m log=.harness/proc/dev.log | db exited(1) 2m ago log=.harness/proc/db.log]
```

One token per process that has *itself* ever been started (a declared but
never-started process is omitted even once the block starts appearing for
others). `ready`/`running`/`starting` report elapsed time since start;
`exited`/`stopped` report elapsed time since finish, suffixed `ago`, and
`exited` additionally carries the exit code. A process with declared
`ports` (§1a) carries a `:3000` (or `:3000,3001`) token right after its
state — `dev`'s in the example above — omitted entirely for a process
with no declared ports (`db`'s). The log path is relativized against the
session's working directory when possible.

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

## 6. End-to-end verification

`e2e.TestManagedProcessesEndToEnd` drives the whole feature through a real
`harness serve` subprocess, a real session, and a real (if short-lived)
child process — not just unit-level calls into `process`/`engine` types.
A fake Anthropic backend records every raw request body it receives and,
on the first call, emits a `tool_use` block calling the `process` tool
itself (`{"action":"start","name":"dev"}`) — the model discovers and
drives the tool, the test never calls `Start` directly. It then asserts,
against the literal wire JSON:

1. The first request (before anything ran) carries no ambient block.
2. The very next request in the same tool loop — same prompt text,
   nothing new said — already carries `[processes: dev ready ...]`.
3. A later, unrelated prompt ("what should I do next?") still carries it.
4. `GET /process` reports the same live process, and the real log file on
   disk contains the real ready line the real child process printed.

This is the literal proof of the design's purpose statement: an agent
starts a dev server without ceremony, and the model always knows what is
running and where the logs are without being told in the prompt.

## 7. Non-goals

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

## 8. Startup config observability

A misnamed config file (a project's `.harness.json` typo'd, or
`HARNESS_CONFIG` pointing somewhere stale) has no declared processes,
mcp servers, or plugins — indistinguishable, from the merged
`*config.Config`'s point of view, from an operator who never intended to
configure anything. This is a real production failure mode (a `dev`
process quietly never gets declared, `process` tool calls 404, and
nothing in the logs says why), so `harness serve` and `harness run` both
emit exactly one `slog` INFO line at boot naming which config file (if
any) was loaded and how much it declares:

```
config: /root/web/.harness.json (2 processes, 1 mcp server, 0 plugins)
```

or, when neither the project override nor the user config file exists:

```
no config file found
```

This is `config.LoadProjectWithInfo` (a thin wrapper around
`LoadProject`, returning the same `*Config` plus a `LoadInfo`): `Path` is
the project override (`<dir>/.harness.json`) when present, else the user
config path (`config.Path()`) when present, else empty; `Processes`/
`MCPServers`/`Plugins` are the **merged** config's counts, so a project
override that only adds one field to an otherwise-empty processes map
still reports the right total. `cmd/harness`'s `loadConfigLogged` (used by
`serveCmd`/`runCmd` only — `sessions`/`plugin probe` keep using the plain
`loadConfig`, since this line belongs at the "engine is booting" moment,
not every CLI invocation) turns that into the single log line above.
