# MCP client and server

Read the root AGENTS.md.

- Keep this package independent from runtime, server, and command code.
- Keep JSON-RPC framing dependency-free. Preserve request ID correlation.
- Stdio uses one JSON-RPC message per line.
- Streamable HTTP accepts a JSON or SSE response. Preserve `MCP-Session-Id`.
- Preserve `tools/list` and `resources/list` pagination cursors.
- Send static request headers on every HTTP call.
- Client scope: `tools/list`, `tools/call`, `resources/list`, `resources/read`.
- Add no OAuth, client capabilities, legacy HTTP+SSE, subscriptions, or prompts.
- Preserve text, image, audio, resource-link, embedded-resource, and `isError` fields.
- Never collapse structured content into text here.
- Server (`Registry`): a tool that needs a harness type is registered by its caller.
- Server scope: initialize, notifications/initialized, tools/list, tools/call. Add no prompts, resources, roots, sampling, elicitation, or resumable streams.
- Every server response is one JSON object. Add no `text/event-stream` path. Issue and enforce no `Mcp-Session-Id`.
- `ServeHTTP` validates `Origin` before parsing the body. Absent or loopback passes. Reject cross-origin with 403. Never relax this.
- Return `RPCError` for protocol failures. Return a successful `CallToolResult` with `IsError` for a handler failure.
- Pin framing, HTTP, and registry behavior with an `e2e/` contract row. Make no remote call. Test a transport or `Registry.ServeHTTP` directly only in a file that `testdata/test-exceptions.txt` lists.
