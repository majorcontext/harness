# MCP transport instructions

These rules apply to `mcp/`. Harness does not merge ancestor files. If root
guidance is not active, locate the Git root and read `<repo-root>/AGENTS.md`.
Resolve repository paths from that root.
Read `engine/AGENTS.md` for connection policy and lazy schema loading.

## Package boundary

Keep this package independent from engine, server, and command integration. It
implements the MCP client protocol and transports only.

Keep JSON-RPC framing dependency-free. Preserve request ID correlation and
notification handling.

## Transports

- Stdio uses one JSON-RPC message per line over the child process streams.
- Streamable HTTP accepts a JSON response or SSE response.
- Preserve `MCP-Session-Id` continuity.
- Preserve paginated `tools/list` cursors.
- Keep static request headers on every HTTP call.

Do not add OAuth, client-served capabilities, legacy HTTP+SSE fallback, or
other MCP feature families without an explicit scope change.

## Content

Preserve text, image, audio, resource-link, embedded-resource, and `isError`
tool-result fields. Do not collapse structured content into text inside this
package.

## Tests

Use `net.Pipe` for protocol framing and `httptest` for HTTP. Test split frames,
multiple SSE events, pagination, cancellation, and malformed responses. Do not
call a remote MCP server in the unit suite.
