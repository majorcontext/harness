# Harness Plugin Protocol — v1

Plugins are separate processes speaking **JSON-RPC 2.0 over stdio, one
message per line** (NDJSON). Any language works; `github.com/majorcontext/harness/plugin`
is the Go SDK. Log to stderr — stdout belongs to the protocol.

The channel is **bidirectional**: the harness sends hook dispatches and tool
executions; the plugin sends client API calls back — including while one of
its hooks is in flight. Request IDs are JSON numbers. Each side numbers its
own requests independently.

## Lifecycle

1. Harness spawns the plugin process (lazily, on first hook dispatch or tool
   call — never at startup; manifests are cached at install time, keyed by
   binary hash).
2. Harness → `initialize` (request) with `InitializeParams`. Plugin responds
   with its `Manifest`. A protocol-version mismatch is an initialize error.
   The harness verifies the live manifest matches the cached one.
3. Hook dispatches, tool executions, and client API calls flow until:
4. Harness → `shutdown` (notification), then closes stdin. Plugin exits.

`InitializeParams` carries `serve_url` and `run_token` when (and only when)
the harness is running in `harness serve` mode: a plugin process — in any
language, not just the Go SDK — can then also hit the HTTP API directly
(`GET /session/{id}/message`, etc.) instead of going through the stdio
client API. Both are empty in `harness run` mode, where there is no HTTP
API to reach. See "Trust model" below.

### Trust model

Plugins are **trusted local processes**, not third parties: the harness
spawns them itself, over stdio, from a manifest cached at install time
(binary-hash keyed). `run_token` is therefore the exact same bearer token
the orchestrator holds for this run — not a separate, narrower-scoped
credential minted per plugin. A plugin that can reach `serve_url` can do
anything the orchestrator can do over the HTTP API for this process (create
sessions, prompt, abort, read any session it owns). This mirrors the
`shell.env` hook, which already hands plugins a seat at env-var injection
for tool commands, and the "no auth hooks" decision in AGENTS.md (credential
scoping happens at the network layer, not inside the harness). A plugin
that should not have this reach simply should not be installed.

A plugin that fails to start or errors on a dispatch is skipped — **hook
chains fail open**; a plugin can never wedge a session. Every sync dispatch
carries a deadline (default 5s).

## Methods: harness → plugin

| Method | Kind | Params → Result |
|---|---|---|
| `initialize` | request | `InitializeParams` → `Manifest` |
| `shutdown` | notification | — |
| `hook/event` | notification | `EventBatch` |
| `hook/chat.params` | request | `ChatParamsRequest` → `ChatParamsResponse` |
| `hook/chat.message` | request | `ChatMessageRequest` → `ChatMessageResponse` |
| `hook/system.transform` | request | `SystemTransformRequest` → `SystemTransformResponse` |
| `hook/shell.env` | request | `ShellEnvRequest` → `ShellEnvResponse` |
| `hook/tool.execute.before` | request | `ToolExecuteBeforeRequest` → `ToolExecuteBeforeResponse` |
| `hook/tool.execute.after` | request | `ToolExecuteAfterRequest` → `ToolExecuteAfterResponse` |
| `tool/execute` | request | `ToolExecuteRequest` → `ToolExecuteResponse` |

Only hooks named in the plugin's manifest are dispatched to it.

## Methods: plugin → harness (client API)

| Method | Params → Result |
|---|---|
| `client/session.messages` | `SessionMessagesRequest` → `SessionMessagesResponse` |
| `client/mcp.call` | `MCPCallRequest` → `MCPCallResult` |
| `client/generate` | `GenerateRequest` → `GenerateResponse` |

`client/generate` routes through the harness provider layer: plugins inherit
model routing, credentials, and observability, and never carry API keys.

`client/session.messages` is backed by the same session store the HTTP
`GET /session/{id}/message` handler reads: it returns the canonical message
list for any session the harness process owns (live or reloaded from disk
in serve mode; the one in-flight session in run mode). An unknown session
id is an RPC error, never an empty/silent success.

`client/mcp.call` routes to the harness's configured `mcp_servers` (see the
config package doc and engine/mcp.go): `Server` names one of them, `Tool` is
the tool's unnamespaced name on that server (not the
`mcp__<server>__<tool>` form a model-issued tool call uses). It reaches the
exact same connected MCP clients the engine's own namespaced tool calls
use. An unconfigured or connection-failed server is a clear RPC error, not
a panic or a silently empty result. `client/generate` is wired into the
dispatch path and type-checks end to end, but is not implemented yet:
provider-layer routing for plugin-initiated LLM calls is a separate PR, and
it returns a clear RPC error until then.

Any language implementing this protocol (the Go SDK in this package, a
future TypeScript SDK, etc.) gets `serve_url`/`run_token` for free once it
decodes `InitializeParams` — no protocol-version bump was needed since they
are additive, optional fields (see Versioning below).

## Chaining semantics

Sync hooks run across plugins in config order; each plugin sees the previous
plugin's mutations.

- `chat.params`, `chat.message`: response replaces the request value for the
  next plugin.
- `system.transform`: additive — segments append; nothing is replaced.
- `shell.env`: maps merge; later plugins win on key conflicts.
- `tool.execute.before`: non-nil `args` replaces; non-empty `deny` blocks the
  tool call, returns the message to the model as an error result, and stops
  the chain.
- `tool.execute.after`: non-nil `output` replaces.
- `event`: async fan-out, batched, fire-and-forget. Delivery to a single
  plugin is FIFO in emit order: the harness gives each plugin instance a
  bounded queue drained by one dedicated sender goroutine (created on first
  emit, exits when the plugin is stopped), so events for the same plugin
  can never be reordered or interleaved by racing writers — e.g. a
  `tool.execute.start` always reaches a plugin before the matching
  `tool.execute.end` for the same call id, provided the harness emitted them
  in that order. There is no ordering guarantee *across* different plugins.
  Emit never blocks the caller: if a plugin's queue is full (the harness's
  default capacity is 256, configurable), the event is dropped and a
  per-plugin counter increments; the first drop for a plugin is also
  reported through the harness's error-observation hook so operators notice
  a wedged or overwhelmed plugin, but delivery itself stays best-effort —
  this is a fire-and-forget hook, not a durable log.

An empty-object response (or the SDK returning `nil`) means "no changes".

## Events

`hook/event` batches carry `Event{type, session_id, properties}`; `properties`
is a JSON object whose shape depends on `type`. Event types, v1:

| Type | Properties | Emitted when |
|---|---|---|
| `session.status` | `{status: "busy"\|"idle"}` | a prompt starts/finishes |
| `question.asked` | (reserved, no emit site yet) | — |
| `file.edited` | `{path}` (absolute) | a built-in file tool (`write_file`, `edit_file`) successfully writes a file |
| `tool.execute.start` | `{tool, call_id}` | immediately before any tool call executes (built-in or plugin-provided) |
| `tool.execute.end` | `{tool, call_id, ok}` | immediately after a tool call finishes; `ok` is `false` when the result is an error result |
| `session.error` | `{message}` | a prompt/turn/goal-loop run terminates with an error; `message` is the error string, capped at 256 characters, with a best-effort redaction pass for obvious credential shapes (bearer tokens, `Authorization` header values, `key=value` secrets such as `api_key=...`). This is best-effort sanitization, not a guarantee — a fixed pattern set cannot catch every credential shape a provider adapter might embed, so plugins should still treat `message` as a potentially-sensitive, untrusted string. The 256-character cap bounds how much of a stack trace or request/response body embedded in an error can leak through, but redaction is pattern-based, not structural, so it cannot promise their absence. Excludes `context.Canceled`: a cancelled context is a deliberate stop (abort, goal clear, server drain), not a failure |

`tool.execute.start`/`tool.execute.end` bracket the actual tool execution
only — a call denied by `tool.execute.before` never runs and so never emits
these.

**Deferred**: message-delta events (`text.delta`, `reasoning.delta`
equivalents for plugins). Streaming deltas are high-frequency; shipping them
on the fire-and-forget `event` hook needs a throttling/coalescing design
first so a slow plugin can't fall arbitrarily far behind or amplify RPC
volume. Not in this vocabulary yet.

## Message content

Tool outputs and generate results use the canonical `message.Parts` encoding:
a JSON array of objects, each with a `"type"` discriminator (`text`, `blob`,
`tool_call`, `tool_result`, `reasoning`). See package
`github.com/majorcontext/harness/message`.

## Versioning

`ProtocolVersion` is a single integer, declared in both `InitializeParams`
and `Manifest`. v1 is this document. Additions (new hooks, new event types,
new optional fields) bump the minor behavior but not the version — unknown
hooks are simply never subscribed, and unknown fields are ignored. Breaking
changes to existing payload shapes bump the version.

Deliberately absent from this protocol, by design (see AGENTS.md): permission
hooks, plan mode, and auth hooks.
