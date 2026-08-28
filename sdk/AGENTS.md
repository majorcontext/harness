# SDK instructions

These rules apply to `sdk/`. Harness does not merge ancestor files. If root
guidance is not active, locate the Git root and read `<repo-root>/AGENTS.md`.
Resolve repository paths and commands from that root.

Read `plugin/AGENTS.md` and `plugin/PROTOCOL.md` before an SDK protocol change.

## Protocol parity

The TypeScript SDK and Go plugin host must speak the same versioned NDJSON
protocol. Keep method names, field names, hook behavior, tool results, and
shutdown behavior in parity.

Do not add an SDK-only wire extension. Change the protocol document and Go
implementation in the same change.

## TypeScript SDK

- Keep `sdk/typescript/harness-plugin.mjs` zero-dependency ESM.
- Use Node built-ins only.
- Keep stdout exclusive to protocol frames. Send logs to stderr.
- Preserve snake_case wire fields.
- Derive manifest hooks and tools from the supplied definition.
- Keep Node 18 compatibility unless the README changes the support floor.

Run:

```bash
node --test sdk/typescript/test/*.test.mjs
go test -race ./plugin/...
```
