# Harness Plugin Protocol — v1

Plugins are separate processes speaking **JSON-RPC 2.0 over stdio, one
message per line** (NDJSON). Any language works. The TypeScript SDK and the Go package
`github.com/majorcontext/harness/plugin` implement it. Log to stderr — stdout belongs to the protocol.

The channel is **bidirectional**: the harness sends hook dispatches and tool
executions; the plugin sends client API calls back — including while one of
its hooks is in flight. Request IDs are JSON numbers. Each side numbers its
own requests independently.

## Lifecycle

1. Harness spawns the long-lived plugin process lazily on the first hook
   dispatch or tool call. Before host construction, a matching cached manifest
   is reused; a missing or stale entry gets one bounded probe. The cache
   trusts the size and the mtime of the executable: a guard binary that is
   replaced in place with the same size and mtime, and that now subscribes to
   a hook, is not seen until `harness plugin probe` runs or the size or the
   mtime changes.
2. Harness → `initialize` (request) with `InitializeParams`. Plugin responds
   with its `Manifest`. A protocol-version mismatch is an initialize error.
   The harness compares the live manifest with the cached one: name, protocol
   version, hooks, `deny_on_failure`, `hook_timeouts`, and tools. A difference
   is a start failure.
3. Hook dispatches, tool executions, and client API calls flow until:
4. Harness → `shutdown` (notification), then closes stdin. Plugin exits.

`InitializeParams` carries `serve_url` and `run_token` when the host serves
the HTTP API: `harness serve` sets them, and an embedder sets them with
`Options.ServeURL` and `Options.RunToken`. A plugin process in any language
can then call the HTTP API directly (`GET /sessions/{id}/messages`, for
example) instead of the stdio client API. Both are empty when the host serves
no HTTP API, as in `harness run`. See "Trust model" below.

### Trust model

Plugins are **trusted local processes**, not third parties: the harness
spawns them itself over stdio from a probed and validated manifest.
`run_token` is therefore the exact same bearer token
the orchestrator holds for this run — not a separate, narrower-scoped
credential minted per plugin. A plugin that can reach `serve_url` can do
anything the orchestrator can do over the HTTP API for this process (create
sessions, prompt, abort, read any session it owns). This mirrors the
`shell.env` hook, which already hands plugins a seat at env-var injection
for tool commands. Credential scoping happens at the network layer, not inside
the harness. A plugin
that should not have this reach simply should not be installed.

A plugin that fails to start or errors on a dispatch is skipped — **hook
chains fail open**; a plugin can never wedge a session. Every sync dispatch
carries a deadline (default 5s; see Hook timeouts).

### Deny on failure

A plugin can mark `tool.execute.before` as deny-on-failure. The manifest gets
an optional list:

```json
{"name": "guard", "protocol_version": 1,
 "hooks": ["tool.execute.before"],
 "deny_on_failure": ["tool.execute.before"]}
```

- `tool.execute.before` is the only valid value, and `hooks` must list it. Any
  other value makes the manifest invalid: a probed manifest fails the first
  `Create` or `Open`, and an in-process plugin fails `harness.New`.
- A failure is any of: the plugin does not start, the process exits, the
  deadline passes, the response does not decode, or the hook returns an error
  or panics.
- When a flagged hook fails, the chain stops and the tool does not run. The
  model gets an error tool result: `denied: plugin <name> failed: <error>`.
- A hook is flagged when the cached manifest or the live manifest flags it. A
  start failure, a manifest mismatch included, denies each call of a flagged
  hook.
- A canceled context is an abort, not a failure. An interrupt during a flagged
  hook ends the call and records no deny.
- A hook that is not flagged keeps the fail-open rule.
- `Options.OnError` receives each failure, flagged or not.

An older host ignores `deny_on_failure` and fails open. A plugin cannot
change this. A guardrail that must fail closed needs a host that has this
field.

### Hook timeouts

A plugin can set the deadline of a synchronous hook. The manifest gets an
optional map from hook name to milliseconds:

```json
{"name": "guard", "protocol_version": 1,
 "hooks": ["tool.execute.before"],
 "hook_timeouts": {"tool.execute.before": 60000}}
```

- A hook with no entry uses 5s.
- A value must be from 1 to 600000. The hook must be in `hooks` and must not be
  `event`. Any other value makes the manifest invalid: a probed manifest fails
  the first `Create` or `Open`, and an in-process plugin fails `harness.New`.
- The timer does not pause while the hook waits on a client call.
- When the dispatch of that hook starts the plugin, the `initialize` call has
  the longer of the hook timeout and 5s, so a short timeout never shortens the
  start. Only `initialize` has that bound; the spawn or dial does not end with
  the context. The timeout bounds the hook call alone.
- The `event` hook is asynchronous and has no per-hook deadline.

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
`GET /sessions/{id}/messages` handler reads: it returns the canonical message
list for any session the harness process owns (live or reloaded from disk
in serve mode; the one in-flight session in run mode). An unknown session
id is an RPC error, never an empty/silent success.

`client/mcp.call` calls a tool of any server in the `mcp_servers` config of
the runtime. The host waits for the first connect of the servers and never
dials again: a server that is retrying in the background or parked fails the
call at once with its reason, without a new attempt, and only the connect
action of the `mcp` tool brings a parked server back. The result is
`{content, is_error}`: a tool that reports an error is a result with
`is_error` set. An unknown server, a down server, or a failed transport is
an RPC error, and its text hides the server URL and headers.

`client/generate` makes one model request with no tools. An empty `model` is
the default model, and an alias resolves. A model that no configured provider
serves is an RPC error. A model whose backend runs its own agent loop (claude-code) is an RPC error too, because it would start an agent with every tool of its CLI. A failure message is masked as a session error is. The result is `{message, usage}`: `message` is an
assistant message with the text of the reply, and `usage` is the optional
token count of the call, which this host always sends, with zero counts when the model reports none (`input_tokens`, `output_tokens`, and the cache
counts). No session records the call. On success the host logs one line with the
plugin name, the model, and the usage.

Both calls end when the plugin connection ends or the runtime closes. A client call runs no hook of the chain.

Any language implementing this protocol (the Go package `harness/plugin`, the
TypeScript SDK under `sdk/typescript`, etc.) gets `serve_url`/`run_token` once
it decodes `InitializeParams`. They are additive, optional fields (see
Versioning below).

## Chaining semantics

Sync hooks run across plugins in config order; each plugin sees the previous
plugin's mutations.

- `chat.params`, `chat.message`: a response with a `params` (`message`) key
  replaces the request value for the next plugin; a response without the key
  changes nothing.
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

## Concurrency

The harness MAY keep **several requests in flight on one connection at the
same time**. A plugin must not assume request/response lockstep.

The rules, for a plugin in any language:

- **`id` is the only correlation.** Match a response to its request by the
  request `id`, never by arrival order. Both sides number their own requests
  independently, so an id is unique only within one direction.
- **Answer in any order.** A plugin may reply to a later request first. The
  harness demultiplexes by `id` (`wire.Conn.Call`, `wire.Conn`), so a slow
  handler never blocks a fast one.
- **Write each frame as one whole line, atomically.** A plugin that writes
  from more than one thread MUST serialize its writes. Two frames whose
  bytes interleave are both lost, and the connection carries responses for
  every other in-flight request too. The Go host holds one write mutex for
  the life of the connection (`wire.Conn.write`).
- **Reentrancy is the plugin's choice.** A plugin whose handlers are not
  safe to run at the same time MAY serialize them internally. The harness
  stays correct — it is throttled, not broken. The Go package `harness/plugin` does the opposite
  by default: it serves each incoming request on its own goroutine, so a Go
  plugin's hook handlers must be safe for concurrent use.
- **The `hook/event` FIFO guarantee is unchanged.** Notifications still
  arrive in emit order, one at a time, per plugin (see "Chaining semantics"
  above). Concurrency applies to REQUESTS only.

Concurrency is not new to this version. `plugin.Host` is a box-scoped
singleton shared by every session, so two sessions have always been able to
dispatch a hook to one plugin at the same time. Parallel tool execution in
the engine also creates concurrency WITHIN one session: the tool calls of one
assistant message run as a bounded batch, so their `tool.execute.before` and
`tool.execute.after` dispatches can overlap. A plugin must therefore tolerate
overlap both across sessions and across independent calls in one session.

This section documents an existing wire property. It changes no payload
shape, so `ProtocolVersion` stays 1.

## Message content

Tool outputs and generate results use the canonical `message.Parts` encoding:
a JSON array of objects, each with a `"type"` discriminator (`text`, `blob`,
`tool_call`, `tool_result`, `reasoning`). See package
`github.com/majorcontext/harness/internal/message`.

A `tool/execute` output holds `text` and `blob` parts. The text parts, joined
by a newline, are the result text. A `blob` part with `data` is a result blob
that the model reads after the text. A `blob` part with only `url` becomes the
text line `blob: <url> (<media_type>)`. The host fails the call for a part of
any other type.

## Versioning

`ProtocolVersion` is a single integer, declared in both `InitializeParams`
and `Manifest`. v1 is this document. Additions (new hooks, new event types,
new optional fields) bump the minor behavior but not the version — unknown
hooks are simply never subscribed, and unknown fields are ignored. Breaking
changes to existing payload shapes bump the version.

Deliberately absent from this protocol by design: permission hooks, plan mode,
and auth hooks.
