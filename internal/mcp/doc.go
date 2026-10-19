// Package mcp implements a dependency-free MCP client and a Streamable HTTP
// MCP server for a fixed in-process tool set.
//
// The client supports the 2025-11-25 specification's stdio and Streamable HTTP
// transports. HTTP keeps MCP-Session-Id continuity and sends Options.Headers
// on each request.
//
// The client implements initialization, tools/list, and tools/call. When
// Options.Root is set it declares the roots capability and answers roots/list
// with that directory, on stdio and on an SSE response of HTTP. It answers every
// other server request with method not found. It does not implement
// authorization, sampling, elicitation, resumable SSE streams, or Tasks.
//
// The server (Registry) implements initialization, tools/list, and tools/call.
// It has no transport session state and does not issue or enforce
// Mcp-Session-Id. Every response is a JSON object. Notifications return HTTP
// 202 with no body.
package mcp
