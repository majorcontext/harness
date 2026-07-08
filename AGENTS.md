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

### Deliberately absent — do not add

- **No permission system.** Tool calls are never gated. There is no `permission.ask` hook, no approval UI, no pre-flight rule evaluation.
- **No plan mode.** No edit-mode/plan-mode distinction anywhere in the engine.
- **No JS runtime and no opencode plugin compatibility shim.** Plugins are native processes.
- **No auth hooks.** Credential injection happens at the network layer (gatekeeper) in deployed environments.

These are settled decisions. Do not propose or implement them.

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

Events v1: `session.status`, `question.asked`, `file.edited`.

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

## Code Style

- Standard Go conventions, `go fmt`, `go vet` clean.
- Type annotations in exported APIs over cleverness; small interfaces.

## Code Review Protocol

PRs merge only after the latest automated review round has been read **in
full — including the summary comment**. Inline-thread count is not a merge
gate: the reviewer files findings both as inline threads and as items in the
top-level summary, and both must be addressed (or explicitly acknowledged as
deferred) before merge. Iterate until a round produces zero findings.

## Git Commits

- [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): description` (e.g. `feat(plugin): add shell.env hook`).
- Do not include `Co-Authored-By` lines for AI agents in commit messages.
