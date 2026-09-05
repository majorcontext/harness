// Package mcp implements a dependency-free MCP client.
//
// It supports the 2025-11-25 specification's stdio and Streamable HTTP
// transports. HTTP keeps MCP-Session-Id continuity and sends Options.Headers
// on each request.
//
// The client implements initialization, tools/list, and tools/call. It does
// not implement authorization, client capabilities, non-tool server features,
// resumable SSE streams, or Tasks.
package mcp
