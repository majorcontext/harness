// Package mcpserver implements the Streamable HTTP MCP server role for a
// fixed in-process tool set.
//
// It implements initialization, tools/list, and tools/call. It has no
// transport session state and does not issue or enforce Mcp-Session-Id.
// Every response is a JSON object. Notifications return HTTP 202 with no body.
package mcpserver
