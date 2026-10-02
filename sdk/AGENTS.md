# SDK

Read the root AGENTS.md. Read `plugin/AGENTS.md` and `plugin/PROTOCOL.md` before a protocol change.

- The TypeScript SDK and the Go plugin host speak the same versioned NDJSON protocol.
- Keep method names, field names, hook behavior, tool results, and shutdown in parity.
- Add no SDK-only wire extension. Change the protocol document and Go host together.
- Keep `sdk/typescript/harness-plugin.mjs` zero-dependency ESM with Node built-ins only.
- Keep stdout exclusive to protocol frames. Log to stderr.
- Preserve snake_case wire fields.
- Derive manifest hooks and tools from the supplied definition.
- Keep Node 18 compatibility unless the README changes the floor.
- Run `node --test sdk/typescript/test/*.test.mjs` and `go test -race ./plugin/...`.
