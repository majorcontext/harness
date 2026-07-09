// Package mcp implements a zero-dependency client for the Model Context
// Protocol (MCP, https://modelcontextprotocol.io).
//
// This package implements the MCP specification revision 2025-11-25 (the
// latest stable revision at the time of writing:
// https://modelcontextprotocol.io/specification/2025-11-25). JSON-RPC 2.0 is
// hand-rolled (no external dependency), the same way plugin/ hand-rolls its
// own JSON-RPC protocol.
//
// Both standard transports are supported:
//
//   - stdio: the client spawns the MCP server as a child process and speaks
//     newline-delimited JSON-RPC over its stdin/stdout, per
//     https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#stdio.
//   - Streamable HTTP: the client POSTs JSON-RPC requests to a single MCP
//     endpoint and accepts either a single JSON response or a
//     `text/event-stream` (SSE) response carrying zero or more
//     server-initiated messages followed by the response, per
//     https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#streamable-http.
//     Session continuity uses the `MCP-Session-Id` response/request header;
//     static headers (e.g. `Authorization: Bearer ...`) can be attached to
//     every outgoing request via Options.Headers.
//
// # Scope and deferred features
//
// This package implements the client-side subset needed to consume MCP tool
// servers: the initialize/initialized lifecycle, tools/list (with pagination
// cursors), and tools/call (text, image, audio, resource-link and embedded
// resource content, plus the isError flag). Deliberately out of scope for
// this package (spec features not implemented):
//
//   - OAuth 2.1 authorization (https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization) —
//     only static headers (e.g. pre-obtained Bearer tokens) are supported.
//   - Client-served capabilities: roots, sampling, elicitation.
//   - Server features other than tools: prompts, resources, completion,
//     logging subscriptions (log/notification messages are still delivered
//     to OnNotification, log-and-continue, but are not modeled).
//   - Resumable SSE streams (Last-Event-ID redelivery) and the deprecated
//     2024-11-05 HTTP+SSE transport's backwards-compatibility fallback.
//   - The experimental Tasks utility (basic/utilities/tasks).
//
// Engine, server, and cmd integration is a separate follow-up; this package
// has no dependency on the rest of the harness module.
package mcp
