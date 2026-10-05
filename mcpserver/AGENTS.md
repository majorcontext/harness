# MCP server role

Read the root AGENTS.md. `mcp/AGENTS.md` covers the client role.

- Keep this package independent from `engine` and `server`.
- A tool that needs a harness type is registered by its caller, never added here.
- Implement only initialize, notifications/initialized, tools/list, and tools/call.
- Add no prompts, resources, roots, sampling, elicitation, or resumable streams.
- Every response is one JSON object. Add no `text/event-stream` path.
- Issue and enforce no `Mcp-Session-Id`. Identity lives in the caller's URL.
- `ServeHTTP` validates `Origin` before parsing the body. Absent or loopback passes.
- Reject a cross-origin request with 403. Never relax this check.
- Return `mcp.RPCError` for protocol failures: unknown method, unknown tool, bad params.
- Return a successful `CallToolResult` with `IsError` for a handler failure.
- Pin registry behavior with an `e2e/` contract row that reaches it through the harness. Test `Registry.ServeHTTP` directly only in a file that `testdata/test-exceptions.txt` lists.
- Cover success, handler error, unknown tool, unknown method, and notifications.
