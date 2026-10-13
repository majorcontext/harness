# SDK

Read the root AGENTS.md. Each SDK has its own rules: read the AGENTS.md of the directory you change.

## sdk/typescript

The rules below bind `sdk/typescript` only. Read `internal/plugin/AGENTS.md` and `internal/plugin/PROTOCOL.md` before a protocol change.

- The TypeScript SDK and the Go plugin host speak the same versioned NDJSON protocol.
- Keep method names, field names, hook behavior, tool results, and shutdown in parity.
- Add no SDK-only wire extension. Change the protocol document and Go host together.
- Keep `sdk/typescript/harness-plugin.mjs` zero-dependency ESM with Node built-ins only.
- Keep stdout exclusive to protocol frames. Log to stderr.
- Preserve snake_case wire fields.
- Derive manifest hooks and tools from the supplied definition.
- Keep Node 18 compatibility unless the README changes the floor.
- Run `node --test sdk/typescript/test/*.test.mjs` and `go test -race ./internal/plugin/...`.
