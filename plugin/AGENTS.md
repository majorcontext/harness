# Plugin instructions

These rules apply to `plugin/`. Harness does not merge ancestor files. If root
guidance is not active, locate the Git root and read `<repo-root>/AGENTS.md`.
Resolve repository paths from that root.

Read `plugin/PROTOCOL.md` before a wire or hook change. Read
`docs/plugins-and-protocols.md` for extended rationale.

## Process model

A plugin is a separate process that speaks versioned JSON-RPC over stdio.

- `harness plugin probe` runs a bounded manifest probe and caches it with
  executable identity and plugin-spec identity.
- Run and serve startup trust a matching cached manifest. A missing or stale
  entry performs one bounded probe before host construction.
- Spawn a plugin on its first hook dispatch or tool call.
- Keep one warm process for later calls.
- Bound every synchronous dispatch with a deadline.
- A hung plugin must not block unrelated sessions or status reads.

## Manifest and visibility

A configured plugin appears in `Host.Plugins()` before it starts. Report
manifest tools and hooks with live spawn state.

Keep status reads lock-free with respect to a dial or handshake. A plugin that
dies after startup becomes `errored`.

## Hook protocol v1

| Hook | Contract |
|---|---|
| `event` | Asynchronous, batched, fire-and-forget |
| `chat.params` | Synchronous request-parameter mutation |
| `chat.message` | Synchronous message mutation before logging |
| `system.transform` | Synchronous additive system segments |
| `shell.env` | Synchronous command environment mutation |
| `tool.execute.before` | Synchronous argument rewrite or deny |
| `tool.execute.after` | Synchronous result rewrite |

Run synchronous hooks in configured plugin order. Each plugin sees prior
mutations. Keep `system.transform` after provider resolution.

## Plugin tools and client API

Tool definitions come from the cached manifest. Tool execution uses RPC.

Plugins can call `Session.Messages`, `MCP.Call`, `Generate`, and the
configured HTTP client. Plugins must not carry provider API keys.

Do not add message-delta events without a throttling and backpressure design.

## Settled protocol exclusions

Do not add permission hooks or auth hooks. Network-layer deployment controls
own credentials.

Do not add a JavaScript runtime or opencode compatibility shim.

## Tests

Use `net.Pipe` for framing and protocol tests. Use `testing/synctest` for
queue and deadline behavior. Do not spawn a real fixture unless process
lifecycle is the subject of the test.
