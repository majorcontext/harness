# MCP server-role instructions

These rules apply to `mcpserver/`. Harness does not merge ancestor files. If
root guidance is not active, locate the Git root and read
`<repo-root>/AGENTS.md`. Resolve repository paths from that root.
Read `mcp/AGENTS.md` for the client-role counterpart this package mirrors.

## Package boundary

Keep this package independent from `engine` and `server`. It implements the
MCP server-role JSON-RPC dispatch and the Streamable HTTP transport only, over
a caller-supplied set of tools. A concrete tool that needs `engine.Session` (or
any other harness type) is registered by its own caller (see
`server/mcp_history.go`), never added to this package.

## Scope

Implement initialize, notifications/initialized, tools/list, and tools/call
only. Do not add prompts, resources, roots, sampling, elicitation, or
resumable SSE streams without an explicit scope change.

Every response is a single JSON object. Do not add a `text/event-stream`
response path unless a registered tool needs to push a server-initiated
message ahead of its own result — none does today.

Do not add `Mcp-Session-Id` issuance or enforcement. This transport's session
identity is stateless by design; a caller that needs identity carries it in
its own URL, one layer above this package.

## Errors

Return a JSON-RPC `RPCError` (`mcp.RPCError`) for a protocol-level failure: an
unknown method, an unknown tool name, or malformed params. Return a successful
`CallToolResult` with `IsError` set for a tool-level failure (a registered
handler's own returned error). Keep this distinction — do not fold one into
the other.

## Tests

Use `httptest` and drive `Registry.ServeHTTP` directly. Cover initialize,
tools/list, tools/call (success, handler error, and unknown tool), unknown
method, and the notification (no-response-body) path. Do not depend on
`engine` or `server` in this package's own test suite.
