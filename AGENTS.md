# AGENTS.md

Instructions for AI coding agents working in this repository.

## Project Overview

Harness is a Go agent harness (in the spirit of pi and opencode) built around four priorities, in order:

1. **Speed** — especially startup speed. `harness --version` under ~5ms, TUI first frame under ~30ms. These are CI-enforced budgets, not aspirations.
2. **Extensibility** — a language-agnostic plugin protocol with a first-class Go SDK.
3. **Composability** — headless engine, event streams on stdout, client/server split, MCP in both directions.
4. **Dynamic model choice** — swap providers/models mid-session or per-subagent with zero migration cost.

## Architecture

The engine is a headless Go library; every frontend (CLI, TUI, server API) is a client.

```
cmd/harness        thin CLI: flags → engine or client
engine/            session loop, tool registry, event log
provider/          one adapter per API family (anthropic, openai-responses, gemini, openai-compat, bedrock)
message/           canonical message/part types + per-provider transcoders
plugin/            hook bus, JSON-RPC stdio protocol, plugin SDK
server/            HTTP+SSE / unix socket exposing the engine
tui/               a client, nothing more
```

### Core invariants

- **A session is an append-only log of typed events.** User messages, model deltas, tool calls, results, model switches — all events. UIs, JSON output, and plugins are subscribers to the same stream.
- **The session log stores the canonical message format, never a provider's.** Every request, the provider adapter transcodes canonical history → provider wire format from scratch (stateless transcoding). Mid-session model swap = next request uses a different transcoder. No migration step.
- **Provider-specific opaque data (reasoning/thinking blocks, encrypted reasoning items) is stored as provider-tagged attachments** on canonical messages: replayed verbatim to the same provider, dropped when crossing providers. Tool-call IDs are internal; each transcoder maps deterministically to provider-compliant IDs. Prompt-cache markers are injected at transcode time, never stored.
- **Model refs are `provider/model`** plus user-defined aliases (`fast`, `smart`) from config. The models.dev catalog snapshot is embedded at build time and refreshed async — never on the startup path.

### Project instructions (AGENTS.md)

The engine auto-injects a project's `AGENTS.md` into the system prompt. On the
first `Prompt` of a session (never at `NewSession` — the startup budget rule)
it walks up from `Config.WorkDir` for `AGENTS.md` (falling back to `AGENT.md`),
stopping at the git root or filesystem root; the closest file wins, per the
[agents.md](https://agents.md/) convention. The file is schema-less Markdown —
no headings are required or parsed. The segment is appended after
`Config.System` and before hook (`system.transform`) segments, cached for the
session, and never written to the session log (loaded fresh on resume).

A present-but-unusable file (invalid UTF-8, or empty/whitespace-only) fails the
first `Prompt` — a project that meant to supply instructions must not run
silently without them. A missing file is fine. Oversize files are truncated at
64 KiB with a marker. Disable with `-no-instructions`, config `instructions:
false`, or point at a specific file with config `instructions_path`.

### Agent Skills

The engine advertises [Agent Skills](https://agentskills.io) in the system
prompt following the spec's progressive-disclosure model. On the first `Prompt`
(alongside instructions loading, same load-once-cache-error pattern) it runs
`skill.Discover` over each configured directory, merges the results sorted by
name, and injects one system segment **after** the instructions segment and
before hook (`system.transform`) segments. That segment is stage 1 only: a
header telling the model it MUST read a skill's `SKILL.md` with the `read_file`
tool before relying on it, then one line per skill — `name — description (path:
<abs SKILL.md>)`. Stage 2 (the body) is deferred to that read.

`Config.SkillsDirs` selects the directories: nil (the default) uses
`<WorkDir>/.agents/skills` when it exists; an explicit empty slice disables
discovery. A malformed `SKILL.md` or a duplicate skill name across dirs fails
the first `Prompt` loudly (same semantics as a malformed AGENTS.md). Skills are
never written to the session log — a resumed session rediscovers them. Config
`skills_dirs` (array; a non-empty project value overrides the user value
entirely) and the repeatable `-skills-dir` run/serve flag drive it.

### Goal loop

`Session.PursueGoal(ctx, condition, GoalOptions)` drives the ordinary `Prompt`
loop toward a natural-language completion condition. Turn 1 prompts the raw
condition; after **every** turn an independent, TOOL-LESS evaluator model
(`GoalOptions.Evaluator`, resolved through the same `Config.Providers` registry,
`MaxTokens` 256) is asked to answer `MET: <reason>` / `NOT MET: <reason>`
(parsed leniently). A NOT MET verdict re-prompts with a fixed-template guidance
message carrying the reason; MET returns `Achieved`. `MaxTurns` (0 = unlimited)
bounds it; two unparseable evaluator replies in a row error rather than spin.
Durable `goal.set` / `goal.eval` / `goal.achieved` / `goal.cleared` records land
in the session log, so `LoadSession` restores an active goal (condition only;
counters reset) via `Session.ActiveGoal()` — resume never auto-runs it, the
caller decides. The loop also emits `goal.*` engine events so the server
journals them. Config `goal_evaluator_model` supplies the evaluator for
`harness run -goal` and `POST /session/{id}/goal`.

The condition itself is adjustable mid-loop. `Session.UpdateGoal` rewrites an
active goal's condition, journals a durable `goal.updated` record, and emits
`EventGoalUpdated` — same lock-and-emit-under-`s.mu` shape as `RegisterGoal`;
a same-condition update is a silent no-op, updating an inactive goal errors.
`PursueGoal` takes a per-turn snapshot (condition, a runtime-only generation
counter, active) instead of closing over the original parameter, so a live
loop picks up new text at its very next turn boundary — both the worker
directive and the evaluator call. The generation counter guards stale
verdicts: if `UpdateGoal` lands while an evaluator call for generation N is
in flight, a MET (or stalled) verdict for N is discarded on return — no
`goal.achieved`, no `goal.eval`, the loop just continues against the new
condition, never a false-positive completion against text the model never
saw. `ClearGoal` is unaffected — it keys on `goalActive`, not condition
equality, so it still stops the loop at every point it does today.

A built-in `goal` session tool (gated on `Config.GoalTool`, on whenever an
evaluator is configured — `harness run -goal` and `harness serve` alike)
lets the model inspect or drive its own goal in-process: no HTTP round-trip,
no run-slot claim. `status` reports `{active, condition}`; `set` arms a new
goal via `RegisterGoal` (it does not start evaluating this turn — see
auto-arm below — and errors telling the model to use `adjust` if a goal is
already active); `adjust` rewrites an active goal's condition via
`UpdateGoal`. There is deliberately **no `clear` action** — see below.

`POST /session/{id}/goal` on a busy session no longer flatly 409s. A running
goal loop updates its condition in place (`status: "updated"`, 200 — no
second loop, no run-slot claim; the loop picks it up at its next turn
boundary). A plain prompt holding the slot with no goal yet active registers
the goal and reports `status: "armed"` (202); once that prompt's `runPrompt`
tail finishes, an auto-arm check (`maybeAutoArmGoal`) claims the freed run
slot itself and spawns the loop — no further client action needed. This is
also how the `goal` tool's own `set` action takes effect: arming a goal
mid-turn, the same auto-arm path starts the loop the instant the current
turn ends. A workdir held by a genuinely different session still 409s,
unchanged.

No self-clear is deliberate: a goal-supervised agent must never be able to
cancel its own supervision from inside a running turn, so the `goal` tool
has no `clear` action — `DELETE /session/{id}/goal` remains the only clear
path, and it is operator-only.

The goal loop is a **plan-artifact-free, gate-free** control loop: it is
`Prompt` plus a read-only evaluator call, with no plan document, no edit/plan
mode, and no permission gate. It does not violate the no-plan-mode decision
below.

### Managed processes

`config.Config.Processes` (`processes` in JSON) declares named long-lived
dev/support processes (`pnpm dev`, a local DB) that a `process` session
tool can start/stop/restart/inspect without an agent reinventing PID
tracking. `*process.Manager` (package `process`, not `engine`) is a
box-scoped singleton — built once per harness process and shared across
every session, exactly like `engine.MCPManager` — with a
starting/ready/running/exited/stopped state machine, unix process-GROUP
kill on stop (mirroring `engine/bash_unix.go`'s Setpgid/kill-pgroup/
WaitDelay pattern), and asynchronous death detection (a waiter goroutine
flips state to `exited` with no client asking). Logs land at
`<workDir>/.harness/proc/<name>.log`.

The tool can also `declare`/`undeclare` NEW process definitions at
runtime (server-lifetime only, never written to `.harness.json`) — see
`docs/design/managed-processes.md` for the full validation and origin
(`config` vs `runtime`) rules. `harness serve` always builds a
`*process.Manager`, even with zero configured processes, so the tool is
present on every served box; `harness run` keeps the zero-cost-when-
unconfigured rule.

Once at least one declared process has EVER been started (this server
process's lifetime), request assembly appends an ephemeral `[processes:
...]` status block to the newest user message ONLY — never persisted into
the durable session log, never touching any earlier message so a
provider's prompt cache prefix stays intact. See
`docs/design/managed-processes.md` §4 for the exact mechanism and why it
is safe.

### Deliberately absent — do not add

- **No permission system.** Tool calls are never gated. There is no `permission.ask` hook, no approval UI, no pre-flight rule evaluation.
- **No plan mode.** No edit-mode/plan-mode distinction anywhere in the engine. (The goal loop above is not plan mode — it produces no plan artifact and gates nothing.)
- **No JS runtime and no opencode plugin compatibility shim.** Plugins are native processes.
- **No auth hooks.** Credential injection happens at the network layer (gatekeeper) in deployed environments.

These are settled decisions. Do not propose or implement them.

## Dispatching goal-supervised sessions

- **Completion conditions must demand world-state evidence, never transcript
  claims.** Require branch-verified-on-origin (`git fetch && git status -sb`
  output shown), pasted test output, etc. — not a model's assertion that it
  did the work. Why: an evaluator once declared files created while the disk
  was empty.
- **Push is the durability mechanism.** Commit as soon as the first test file
  exists; push after every green milestone. Why: lease death and loop death
  have each destroyed unpushed work.

## Plugin System

Plugins are separate processes (any language; Go SDK provided) speaking a versioned JSON-RPC protocol over stdio.

- **Manifest cache**: `harness plugin install` runs the binary once and caches its manifest (name, protocol version, hooks subscribed, tool definitions) keyed by binary hash. Startup reads cached manifests only — nothing spawns at boot.
- **Lazy spawn**: a plugin process starts on first hook dispatch or tool call, then stays warm for the session (module-level caches in plugins are expected and fine).
- Sync hooks chain across plugins in config order — each sees the previous plugin's mutations — and every sync dispatch carries a deadline so a hung plugin can't wedge a session.

### Hook protocol v1

| Hook | Mode | Purpose |
|---|---|---|
| `event` | async, fire-and-forget | full event stream (batched) |
| `chat.params` | sync, mutating | model, temperature, etc. per request |
| `chat.message` | sync, mutating | messages before they enter the log |
| `system.transform` | sync, additive | append segments to the system prompt (runs after `chat.params`) |
| `shell.env` | sync, mutating | inject env vars into shell/tool commands |
| `tool.execute.before` | sync, mutating/blocking | rewrite args or block with `{deny: "message"}` |
| `tool.execute.after` | sync, mutating | rewrite/annotate tool results |

Plugins may also register **custom tools** (defs in manifest, execution via RPC).

### Plugin client API

Plugins are API clients over the same channel: `Session.Messages`, `MCP.Call`, `Generate` (LLM calls through the harness provider layer — plugins never carry their own API keys), and `plugin.HTTPClient()` (outbound HTTP with harness-configured headers, e.g. workspace attribution).

Events v1: `session.status`, `question.asked`, `file.edited`,
`tool.execute.start`, `tool.execute.end`, `session.error`. Message-delta
events are deliberately deferred (see plugin/PROTOCOL.md) pending a
throttling design.

Capability parity bar: the protocol must be able to express the plugin
patterns common in opencode setups — event-driven activity tracking, token
refresh via `shell.env`, tool-call rewriting/vetoing and result guards via
`tool.execute.*`, path-scoped system prompt injection, and custom tools that
call back into the platform.

## External Protocol Surfaces

Standards we conform to at the edges. The internal model (event log, canonical
messages, hook protocol) is ours; these are adapters, never the internal
representation.

- **ACP (Agent Client Protocol, agentclientprotocol.com)** — the editor ↔ agent
  standard (Zed, JetBrains, Neovim, Emacs). Implemented as a thin adapter in
  `server/` mapping the event log to `session/update` notifications. Where our
  event vocabulary has arbitrary naming choices, prefer ACP's names to keep the
  adapter mechanical. We never send `session/request_permission` (no permission
  system) — an agent that never asks is fully conformant. Note: this is Zed's
  Agent *Client* Protocol, not IBM's dead Agent Communication Protocol.
- **MCP** — client (consume tool servers) and server (expose sessions/tools)
  modes. ACP forwards editor MCP config to us, so the two compose.
- **OpenTelemetry GenAI semantic conventions** — for span/metric naming when
  observability lands. Configuration via standard `OTEL_*` env vars only.
- **A2A** — deliberately not implemented. Cross-org agent meshes are a
  different layer; revisit only if a concrete need appears.

## Development hub

`harness hub` is a local, single-operator control surface over a FLEET of
`harness serve` boxes — a fleet dashboard for "what are my agents
doing right now" and for dispatching new goal-supervised sessions, not a
deployed product. It serves one embedded, single-file page
(`tools/hub/index.html`, `go:embed`) on
`localhost:7777` by default (`-addr` to change it).

- **No server-side state.** The hub keeps no registry and reads no config
  file: every box (name, base URL, run token) and the current selection
  live only in that browser tab's URL fragment, base64-encoded JSON
  (`#s=...`), kept in sync via `history.replaceState`. That makes a hub URL
  bookmarkable and shareable between local tabs with zero persistence code
  — and means **run tokens ride the URL by design**; treat a hub link like
  a secret.
- **The page talks to boxes directly** from the browser, over each box's
  normal HTTP+SSE API (`server/openapi.yaml`) — never proxied through the
  hub's own server. Every box must therefore be started with `-cors-origin`
  set to the hub's origin (or `*` for local hacking), e.g. `harness serve
  -cors-origin http://localhost:7777`; a box without it will look
  permanently unreachable from the hub.
- **The Go side is minimal on purpose**, exactly one API: `POST /spawn`.
  It execs the command given by `-spawn-command` (or `$HARNESS_HUB_SPAWN`)
  via `sh -c` and streams its combined stdout+stderr live to the page over
  SSE. The **spawn-command contract** — the only coupling between this repo
  and any deployment-specific provisioning tool — is plain lines anywhere
  in that output: `TUNNEL_URL=<url>` and `RUN_TOKEN=<token>` (required to
  add the box), and any number of `PORT_URL_<port>=<url>` lines (optional —
  one per exposed port's own tunnel/preview URL, collected into a
  `port_urls` map; see the process strip in `tools/hub/index.html`'s header
  comment). Once the command exits, the stream ends with a summary carrying
  those values (if found) and the exit code; the page adds the new box to
  its own URL state itself. Nothing box-provisioning-specific lives in this
  repo.
  - **Box name passthrough.** `POST /spawn`'s JSON body optionally carries
    `{"name": "..."}` — the page's generated (or, on a Respawn/ADOPT, reused)
    box name. The Go handler sets it as `HARNESS_HUB_BOX_NAME` in the spawn
    command's own environment (`tools/hub/spawn.go`'s `runSpawn`), exactly
    the deployment-environment contract `docs/design/fleet-model.md` §8
    specifies: deployment tooling invoked by `-spawn-command` reads this
    variable to derive per-name storage (typically setting
    `HARNESS_SESSION_DIR` from it before `harness serve` starts) — harness's
    own code never reads `HARNESS_HUB_BOX_NAME` at all. A request with no
    body, or no `name` field, spawns exactly as before (no env var set).
- The hub binds loopback-only by default (`resolveAddr` in `tools/hub/hub.go`).
- **Browser-security hardening** (both in `tools/hub/hub.go`, tested in
  `tools/hub/hub_test.go`). `POST /spawn` execs a real, costly provision
  command, so `handleSpawn` rejects a browser cross-origin request before any
  exec: if an `Origin` header is present it must match the request's `Host`
  (OWASP verify-origin). Loopback binding alone does not stop this — any page
  the operator visits can `fetch("http://localhost:7777/spawn",{method:
  "POST"})` as a no-preflight CORS simple request — but the page's own
  same-origin `fetch("/spawn")` (Origin == Host) and non-browser clients (no
  Origin, so not a CSRF vector) pass unchanged. The served page also carries
  a strict `Content-Security-Policy` (`default-src 'none'` + `'unsafe-inline'`
  script/style — the page is a single no-build `go:embed`'d file with no
  external resources and no per-response nonce hook — + `connect-src *`,
  required because it fetches/streams from arbitrary operator-added box
  origins the stateless hub cannot enumerate, + `frame-ancestors`/`base-uri`/
  `form-action` pinned to `'none'`): defense-in-depth for a page holding run
  tokens in its URL fragment.
- **Pure hub logic is unit-tested** by `tools/hub/hub_test.mjs` (run:
  `node --test tools/hub/*_test.mjs`). **End-to-end, against a real backend**
  is `tools/hub/e2e` (see its README): a `go test -race ./...` subtree that
  starts an actual `server.Server` + `hub.NewHandler` and drives the real,
  served `index.html` with Node + jsdom and an unmocked `fetch` — no manual
  setup step; it installs its own `npm` dependency on first run.

### UI design language

The hub is styled as **tactical telemetry** — a committed dark-only
brutalist archetype (derived from the public
[taste-skill](https://github.com/Leonxlnx/taste-skill) brutalist +
anti-slop skills). Any new hub UI — and future passes on the inspector,
which still wears the older soft theme — follows these rules:

- **One substrate, no theme toggle**: `#0a0a0a` background, `#eaeaea`
  phosphor foreground, `#2a2a2a` hairline borders. Never reintroduce a
  light mode here; pick-one-and-commit is the point.
- **Two semantic colors only.** Hazard red (`--accent`, `#ff2a2a`) means
  trouble or destructive action, nothing else. Terminal green (`--ok`,
  `#4af626`) is reserved for exactly one semantic: live or succeeded goal
  execution. Everything else is monochrome.
- **Monospace dominance**: body text is the `ui-monospace` stack;
  headers are heavy uppercase system-ui. Micro-labels are uppercase with
  `.06–.1em` tracking. No webfonts — the page is CSP-self-contained.
- **Geometry**: `border-radius: 0` absolutely everywhere; square status
  markers; 1px compartment borders; inverted-video hover
  (foreground/background swap). No gradients, soft shadows, or
  translucency. The scanline overlay is static — motion requires a
  stated purpose.
- **Copy discipline**: no emoji in UI strings, no em-dashes anywhere, and
  every piece of "telemetry" displayed must be real data (vcs revisions,
  seqs, PIDs, token counts) — never decorative or fabricated metadata.
- **Selectors are load-bearing**: the renderers create elements by class
  name (`.sess`, `.box-card`, `.dot`, `.goalnarr`, …). Restyle classes;
  never rename them in a styling pass.

## Fleet model (the deploy story)

The full build spec lives in `docs/design/fleet-model.md` — read it before
touching anything box-identity, session-lineage, or goal-pause related. The
short version this repo's code assumes: identity is an operator-chosen box
**NAME**; storage is one volume/directory per name (`HARNESS_SESSION_DIR`
points at it), never shared between concurrently-live servers; a box is
ephemeral compute serving one name (cattle), the name and its volume are
durable (pets). Respawning the same name over the same volume is **ADOPT**
— history restores, and any goal that was armed when the box died surfaces
as `paused`/`pause_reason: "restart"` (see the goal loop's paused
presentation, `engine/goal.go` and `server/journal.go`'s `goal.paused`
record) rather than a false "still running" reading. `parent_session`
(`POST /session`, see `engine/store.go`) is the lineage thread connecting a
re-dispatch to the task it continues from, so a fleet UI can group a box's
history by task across boxes.

**Hub spawn contract:** the hub that spawns boxes — `harness hub`, now
implemented in `tools/hub/` (see the Development hub above) — passes the
generated box NAME to the spawn command's environment as
`HARNESS_HUB_BOX_NAME`, so deployment scripts can derive per-name storage
(e.g. mount/create a volume named after it) without the hub and the box
needing any other side channel to agree on identity. Harness itself never
reads this variable — it is a contract between the hub and deployment
tooling, documented in `docs/design/fleet-model.md` §8.

## Startup Speed Rules

- Nothing touches network, subprocesses, or disk beyond one config file before first paint. Provider auth validates on first message send, not at boot.
- No `init()` side effects. No reflection-heavy config frameworks. One flat config parse.
- Pure Go only — no cgo (use modernc SQLite if SQLite is needed) so cross-compilation stays trivial.
- Batch TUI stream rendering (~30–60fps coalescing); never repaint per token delta.

## Development Commands

```bash
go build ./...
go test -race ./...
go test -race -run TestName ./engine/
go vet ./...
```

## Testing

**TDD is mandatory.** Write the failing test first, watch it fail, then
implement until it passes. New behavior lands in the same commit as its test;
a bug fix starts with a test that reproduces the bug.

Rules:

- **Timer-dependent and concurrency-timeout logic is tested inside a
  `testing/synctest` bubble** (Go 1.25+): time is fake and advances only when
  every goroutine in the bubble is durably blocked, so timeouts fire
  deterministically and instantly. `net.Pipe` and channel-based plumbing work
  in bubbles; real network and file I/O do not. Note fake time stops
  advancing once the test function returns — a goroutine parked in
  `time.Sleep` at bubble end is reported as a deadlock, which is the bubble's
  goroutine-leak detection working for you.
- **For concurrency-sensitive code (locks, queues, backpressure), write the
  invariants down in the brief/design before implementation** and test
  against them. Deriving the design from review findings one round at a time
  took four rounds on a recent PR.
- **Exception — cross-process e2e** (`e2e/` only): tests driving a real
  subprocess may observe out-of-process state with deadline-bounded poll
  loops, because no in-process channel can cross an OS process boundary.
  Intervals stay tight, deadlines explicit; anything observable in-process
  still uses channels or synctest.
- **No raw `time.Sleep` for synchronization — ever, bubble or not.** To
  simulate a hung component, block on a channel closed in `t.Cleanup`; in a
  bubble the hang deterministically outlasts any timeout with zero wall-clock
  cost, and the cleanup release lets the goroutine exit before bubble end.
- **No guessed deadlines.** Block directly on channels for expected events
  and let the test binary timeout catch hangs; don't wrap waits in short
  arbitrary `time.After` failsafes that flake under load.
- Always run with `-race`; CI runs `go test -race ./...`.
- `t.Helper()` in every test helper; `t.Cleanup` over `defer` in helpers so
  cleanup composes.
- `httptest` for HTTP surfaces; in-process pipes (`net.Pipe`) for protocol
  tests — never spawn real subprocess fixtures unless the subprocess
  machinery itself is under test.
- Table tests where cases multiply; golden JSON comparisons for transcoders
  (struct field order makes marshaled output deterministic).
- Production timers use `time.NewTimer` + `defer Stop()`, not `time.After`,
  when the surrounding function can return before the timer fires.
- **Regression tests must be red-verified.** Prove the test fails against the
  pre-fix code — revert the fix, observe red, re-apply it — and show that
  evidence. A regression guard that never ran red is unverified.

## Debugging invariants

Rules learned from production incidents (2026-07-09), written so they apply
without knowing the incidents:

- **Cleansing marshals hide poison.** Persisted session logs are scrubbed by
  the guarded marshal paths (`ToolCall.safeArguments` normalizes,
  `ProviderData.MarshalJSON` drops empty entries), so on-disk state can be
  provably clean while resident in-memory state is unmarshalable. When a
  resident session misbehaves but its journal round-trips cleanly through
  `engine.LoadSession` + `json.Marshal`, the defect lives in memory between
  ingest and persist — do not conclude from a clean log that no defect
  exists. (Incident: truncated `ToolCall.Arguments`, fixed in the commit
  titled "fix(message,engine): truncated ToolCall.Arguments must never
  poison history"; see also the tests in `engine/tool_call_poison_test.go`.)
- **Error text names the rejection, not the cause.** Treat error strings as
  the symptom surface — enumerate which layer actually produced the
  credential/config/input being rejected before acting. (Incident: a git 403
  citing SAML SSO was actually a system-level gitconfig credential helper
  serving a rotated-stale token; the SSO re-auth it demanded was
  irrelevant.)
- **Verify binary identity before blaming staleness.** A deployed binary's
  exact commit is embedded — `go version -m <binary>` shows
  `vcs.revision`/`vcs.time` — check that before hypothesizing that a fix is
  missing from a running process.

## Code Style

- Standard Go conventions, `go fmt`, `go vet` clean.
- Type annotations in exported APIs over cleverness; small interfaces.

## Code Review Protocol

PRs merge only after the latest automated review round has been read **in
full — including the summary comment**. Inline-thread count is not a merge
gate: the reviewer files findings both as inline threads and as items in the
top-level summary, and both must be addressed (or explicitly acknowledged as
deferred) before merge. Iterate until a round produces zero findings.

A green check is not a review. The reviewer has failed silently before: an
instant API error produces a placeholder comment and zero findings, which
reads as mergeable. Before merging, verify the review summary is substantive.

Read and act on every review thread individually — never batch-resolve. One
explicit resolve command per thread id. A batch operation once resolved
unread findings.

## Git Commits

- [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): description` (e.g. `feat(plugin): add shell.env hook`).
- Do not include `Co-Authored-By` lines for AI agents in commit messages.
