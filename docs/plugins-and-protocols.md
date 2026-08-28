# Plugins and external protocols

This document describes plugin lifecycle, hooks, client APIs, and external
protocol boundaries.

## Plugin System

Plugins are separate processes (any language; Go SDK provided) speaking a versioned JSON-RPC protocol over stdio.

- **Manifest cache**: `harness plugin probe` runs a bounded manifest probe and caches the manifest (name, protocol version, hooks subscribed, tool definitions) with executable identity and plugin-spec identity. Run and serve startup trust a matching entry; a missing or stale entry performs one bounded probe before host construction. The long-lived plugin process does not start at boot.
- **Lazy spawn**: a plugin process starts on first hook dispatch or tool call, then stays warm for later calls during the host lifetime (module-level caches in plugins are expected and fine).
- Sync hooks chain across plugins in config order — each sees the previous plugin's mutations — and every sync dispatch carries a deadline so a hung plugin can't wedge a session.
- **Plugin visibility**: `Host.Plugins()` reports every CONFIGURED plugin — name, spawn state (`not-spawned`/`running`/`errored`/`stopped`), registered tools, subscribed hooks — from the cached manifest plus live spawn state. The `session_info` tool (field `plugins`) and `GET /session/{id}` (field `plugins`) both surface it, so a not-yet-spawned plugin still appears. The engine reads it through the `Hooks.Plugins()` interface method, nil-guarded exactly like the other `s.cfg.Hooks` dispatch sites. The state read is lock-free (`instance.liveState`, `plugin/host.go`): `instance.start` holds `inst.mu` for the whole dial-plus-handshake, and `Host` is a box-scoped singleton shared by every session on the box, so a read gated on `inst.mu` would let one session's plugin spawn stall `GET /session`/`session_info` for every other session too — the same "a hung plugin can't wedge a session" rule above, applied to a status read instead of a hook dispatch. `errored` also covers a plugin that died AFTER a successful spawn (its connection closed, detected via the existing `conn.closed` signal), not only a failed start.

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
  standard (Zed, JetBrains, Neovim, Emacs). Harness does not implement an ACP
  adapter today. If one lands, keep it thin: map the event log to
  `session/update` notifications and prefer ACP names where our vocabulary is
  arbitrary. Harness has no permission system, so an adapter must not invent
  `session/request_permission`. This is Zed's Agent *Client* Protocol, not
  IBM's former Agent Communication Protocol.
- **MCP** — client (consume tool servers) and server (expose sessions/tools)
  modes.

  A server's first connect (Initialize+ListAllTools) stays lazy —
  triggered by a session's first `Tools()`/`CallTool()`, bounded by a
  per-server `connect_timeout_s` config field (`MCPServerSpec`, integer
  seconds, <= 0/absent defaults to the engine's own 15s). A server whose
  first attempt fails is never dropped for the process's life: it gets a
  detached background retry on a capped exponential backoff (~1s doubling
  to a 5min cap, jittered) — but bounded to `mcpRetryMaxAttempts` (3)
  further attempts (under ~10s of background effort total). Once those
  are exhausted the entry is marked Parked and the retry goroutine exits
  for good — no further attempt ever fires spontaneously; only an
  explicit re-trigger (the `mcp` tool's `connect` action, below) can move
  it again. A HEALTHY server, by contrast, connects exactly once and is
  never re-probed. `Tools()` always reads live state, so a server that
  recovers mid-session — background retry or explicit reconnect —
  contributes tools on the very next turn automatically, no new session
  required. `CallTool`/`CallServerTool` split the old combined error into
  two: a server name absent from config errors "not configured" (never
  recoverable); a configured-but-unconnected server (still retrying, or
  parked) errors naming that state explicitly (recoverable — retrying may
  still self-heal, parked needs the `mcp` tool). While at least one
  server is degraded, request assembly appends an ambient `[mcp:
  unavailable — <name> (<reason>; retrying), ...]` block to the newest
  user message only — computed fresh every turn, never persisted,
  self-correcting as retries succeed; a Parked server's clause instead
  reads `<name> (<reason>; use the mcp tool action "connect" to retry)` —
  sharing its append-only-to-the-newest-message mechanism
  (`withAmbientStatus`) with the managed-processes status block described in
  `docs/session-storage-and-queue.md`.

  A built-in `mcp` session tool is registered in `newSession` whenever
  the session's MCP registry reports at least one configured server (no
  config flag, unlike `GoalTool`). `status` reports every configured
  server's live state — `{name, connected, attempts, parked, reason}`;
  `connect {server}` makes ONE bounded, synchronous attempt for a named
  server — the only path back for a Parked server, though it works
  against a still-retrying or never-yet-attempted one too. An
  already-connected server is a friendly no-op; an unknown name errors
  listing the configured names. A per-server in-flight guard (under the
  manager's own lock) serializes a tool-triggered connect against both a
  concurrent `connect` call and `retryServer`'s own background attempt
  for the same server — whichever gets there first dials, the other
  reports "attempt already in progress." Every model-visible string on
  this surface — the ambient block, `status`'s `reason`, `connect`'s
  failure result — is `classifyMCPConnectError`'s output, never a raw
  error (which can embed the server's endpoint URL and any secret it
  carries).
- **OpenTelemetry GenAI semantic conventions** — for span/metric naming when
  observability lands. Configuration via standard `OTEL_*` env vars only.
- **A2A** — deliberately not implemented. Cross-org agent meshes are a
  different layer; revisit only if a concrete need appears.
