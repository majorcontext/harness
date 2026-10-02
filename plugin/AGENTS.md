# Plugin

Read the root AGENTS.md. Read `plugin/PROTOCOL.md` before a wire or hook change.

- A plugin is a separate process speaking versioned JSON-RPC over stdio.
- `harness plugin probe` caches the manifest with executable and spec identity.
- Startup trusts a matching cached manifest. A missing or stale entry gets one bounded probe.
- Spawn a plugin on its first hook or tool call. Keep one warm process.
- Bound every synchronous dispatch with a deadline. A hung plugin never blocks other sessions.
- List a configured plugin in `Host.Plugins()` before it starts.
- Keep status reads lock-free with respect to dial and handshake.
- A plugin that dies after startup becomes `errored`.
- `event` hooks are asynchronous and batched. All other v1 hooks are synchronous.
- Run synchronous hooks in configured order. Each sees prior mutations.
- Run `system.transform` after provider resolution.
- Tool definitions come from the cached manifest. Execution uses RPC.
- Plugins never carry provider API keys.
- Add no message-delta events without a throttling and backpressure design.
- Use `net.Pipe` for protocol tests and `testing/synctest` for deadlines.
