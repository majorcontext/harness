// Package mcpserver implements the MCP (Model Context Protocol,
// https://modelcontextprotocol.io) SERVER role, over the Streamable HTTP
// transport, for a fixed in-process set of tools.
//
// This is the mirror image of package mcp, which implements only the
// CLIENT role (mcp/doc.go) — harness had no server-role code at all before
// this package: every existing MCP server harness talks to (mcp.Client)
// runs out-of-process. This package exists so harness itself can host a
// tool a delegated Claude Code CLI turn calls back into — see
// engine/claude_code_backend.go's package doc for why that seam exists
// (get_conversation_history, the fix for a delegated turn otherwise
// starting blind to prior conversation history) and server/mcp_history.go
// for the concrete tool this package serves at POST /session/{id}/mcp.
//
// # Scope
//
// Implemented: the initialize/notifications-initialized lifecycle,
// tools/list, and tools/call — the same subset package mcp's client
// implements, mirrored server-side. Deliberately out of scope, exactly
// like package mcp's own client (see its doc comment for the identical
// list applied to the other role): OAuth 2.1 authorization (this package
// trusts whatever authenticated this HTTP request), prompts, resources,
// completion, logging subscriptions, roots, sampling, elicitation, and
// resumable SSE streams.
//
// # Session identity
//
// This transport's own optional Mcp-Session-Id concept
// (https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#session-management)
// is not used: a Registry issues no session ID and enforces none on
// incoming requests. The spec allows this for a server with no
// transport-level state of its own to track, and this one has none — the
// identity a caller like harness's own /session/{id}/mcp route cares
// about (which harness session a request is for) already lives in the
// URL path one layer up, in server/server.go, not in this package.
//
// # Transport shape
//
// Every response is a single JSON object (Content-Type: application/json),
// never text/event-stream: this server has no server-initiated request or
// notification to push ahead of its own response, which is SSE's only
// advantage over a plain JSON body in this transport. A JSON-RPC
// notification (no "id" — notifications/initialized, or any other
// notification a future client version might send) gets HTTP 202 Accepted
// with an empty body, per the transport spec.
package mcpserver
