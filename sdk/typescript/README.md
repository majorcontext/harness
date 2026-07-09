# harness-plugin (TypeScript / JavaScript SDK)

A zero-dependency Node.js ESM SDK for writing [harness](../../README.md)
plugins. It is the JavaScript counterpart of the Go SDK
(`github.com/majorcontext/harness/plugin`); both speak exactly the same wire
protocol, documented in [`plugin/PROTOCOL.md`](../../plugin/PROTOCOL.md).

- **Zero npm dependencies.** `harness-plugin.mjs` uses only Node.js
  built-ins (`node:readline`). Ship it as a single file next to your plugin.
- **Zero Go dependencies.** Nothing in this SDK touches the Go module; the
  harness spawns your plugin as a subprocess and speaks NDJSON to it over
  stdio, same as any other language.
- **Node ≥ 18** (uses the global `fetch` and ESM). Tested against Node 20.

## Install

There is no package to install — copy `harness-plugin.mjs` into your plugin's
directory (or vendor it via git submodule / subtree) and import it as a
relative ESM module:

```js
import { createPlugin } from './harness-plugin.mjs';
```

## Quick start

A complete plugin that redacts secrets from every tool's output and adds one
custom tool:

```js
// my-plugin.mjs
import { createPlugin } from './harness-plugin.mjs';

createPlugin({
  name: 'my-plugin',
  version: '0.1.0',

  hooks: {
    // Additive: segments from every subscribed plugin are appended to the
    // system prompt, in config order.
    systemTransform: async () => ({
      segments: ['Secrets in tool output are redacted automatically.'],
    }),

    // Runs after every tool call in the session. Returning `undefined`
    // means "no changes" and lets the chain continue untouched.
    toolExecuteAfter: async (ctx, req) => {
      const out = req.output.map((part) =>
        part.type === 'text'
          ? { ...part, text: part.text.replace(/sk-[A-Za-z0-9]{16,}/g, '[REDACTED]') }
          : part,
      );
      return { output: out };
    },
  },

  // Tools declared here are added to the model's tool list; execute() is
  // called via `tool/execute` when the model invokes them.
  tools: [
    {
      def: {
        name: 'echo_reverse',
        description: 'Reverses the given text.',
        inputSchema: {
          type: 'object',
          properties: { text: { type: 'string' } },
          required: ['text'],
        },
      },
      execute: async (ctx, args) => {
        return [{ type: 'text', text: [...args.text].reverse().join('') }];
      },
    },
  ],
}).run();
```

Wire it up in your harness config the same way you would a Go plugin binary,
just pointing `command` at `node`:

```yaml
plugins:
  - name: my-plugin
    command: ["node", "/path/to/my-plugin.mjs"]
```

See [`examples/plugins/redactor.mjs`](../../examples/plugins/redactor.mjs) for
the full reference version of the example above, and
[`plugin/typescript_conformance_test.go`](../../plugin/typescript_conformance_test.go)
for it being driven end-to-end through the real Go `plugin.Host`.

## API

### `createPlugin(definition).run(options?)`

```ts
createPlugin({
  name: string,          // required, must match the harness config
  version?: string,
  hooks?: Hooks,
  tools?: Tool[],
}).run();
```

`run()` reads NDJSON from `process.stdin`, serves the harness's requests, and
writes NDJSON responses to `process.stdout` until the harness sends
`shutdown` and closes stdin (or the stream otherwise ends). It returns a
`Promise<void>` that resolves once the connection closes. **Log to
`stderr`** — stdout belongs to the protocol.

`run({ input, output, exitProcess })` lets you override the streams (used by
this SDK's own unit tests with a scripted fake host) and whether `run()`
calls `process.exit()` when the connection ends (default `true`; set `false`
when embedding).

The manifest's `hooks` list and `tools` list are derived automatically from
which `hooks.*` functions you provide and which `tools` you declare — you
never build the manifest by hand.

### Hooks

Every hook receives `(ctx, req)` (or `(ctx, events)` for `onEvent`) and may
be `async`. Returning `undefined`/`null` means "no changes", matching the Go
SDK's `nil` response convention — the harness sends an empty object on your
behalf. Request/response field names match the wire protocol exactly
(snake_case, e.g. `session_id`, `call_id`) so they map 1:1 onto
`plugin/PROTOCOL.md` and the Go structs in `plugin/hooks.go`.

| Hook | Fires on | Return to mutate |
|---|---|---|
| `onEvent(ctx, events)` | async, fire-and-forget event stream | (nothing; no response) |
| `chatParams(ctx, req)` | before a model request | `{ params }` |
| `chatMessage(ctx, req)` | before a message enters the session log | `{ message }` |
| `systemTransform(ctx, req)` | system prompt assembly (additive) | `{ segments: [...] }` |
| `shellEnv(ctx, req)` | before a shell command runs | `{ env: {...} }` |
| `toolExecuteBefore(ctx, req)` | before any tool call | `{ args }` to rewrite, or `{ deny: "..." }` to block |
| `toolExecuteAfter(ctx, req)` | after any tool call | `{ output }` (canonical `message.Parts`) |

Only hooks you provide are declared in the manifest and dispatched to your
plugin — an unsubscribed hook is simply never sent.

### Tools

```js
tools: [
  {
    def: { name, description, inputSchema }, // inputSchema is JSON Schema
    execute: async (ctx, args) => Part[],     // canonical message.Parts
  },
]
```

Throwing inside `execute` becomes an `is_error` tool result sent back to the
model — never a protocol-level failure, matching the Go SDK.

### `ctx` (the `PluginContext`)

Passed to every hook and tool call; mirrors the Go SDK's `*Client`:

| Member | Equivalent |
|---|---|
| `ctx.workspaceDir` | `Client.WorkspaceDir()` |
| `ctx.config` | `Client.Config()` — this plugin's config block, already JSON-parsed |
| `ctx.httpHeaders` | `InitializeParams.HTTPHeaders` |
| `ctx.fetch(url, init)` | `Client.HTTPClient()` — stamps `httpHeaders` on every request |
| `await ctx.sessionMessages(sessionId)` | `Client.SessionMessages` |
| `await ctx.mcpCall(server, tool, args)` | `Client.MCPCall` |
| `await ctx.generate(req)` | `Client.Generate` |

These calls flow back to the harness over the same connection — including
while your hook is still in flight, exactly like the Go SDK.

### Message content (`Part[]`)

Tool outputs and `generate` results use the canonical `message.Parts`
encoding: a plain JS array of objects with a `type` discriminator, e.g.:

```js
[{ type: 'text', text: 'hello' }]
```

See `github.com/majorcontext/harness/message` for the full set of part
types (`text`, `blob`, `tool_call`, `tool_result`, `reasoning`).

## Protocol version

`PROTOCOL_VERSION` is exported and currently `1`, matching
`plugin.ProtocolVersion` in the Go SDK. A mismatch is rejected as an
`initialize` error, same as the Go side.

## Testing

```sh
node --test sdk/typescript/
```

`sdk/typescript/test/framing.test.mjs` drives the SDK with a scripted fake
host over in-memory streams (no real harness process): handshake, manifest
shape, hook mutation, tool execution/errors, event delivery, and bidirectional
client-API calls mid-hook.

For an end-to-end check against the real Go `plugin.Host` (spawns `node` and
talks the real protocol), see the Go test:

```sh
go test ./plugin/... -run TestTypeScriptSDKConformance -v
```

It skips (not fails) when `node` isn't on `PATH`.
