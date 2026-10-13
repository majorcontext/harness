// Package plugin implements the harness side of the plugin protocol.
//
// A plugin speaks JSON-RPC 2.0 over a byte stream, one message per line
// (NDJSON). The stream is a process pipe or an in-memory pipe. The channel
// is bidirectional: the harness sends hook dispatches and tool executions to
// the plugin, and the plugin sends client API calls
// (client/session.messages, client/mcp.call, client/generate) back to the
// harness, including while a hook is in flight. The author API is the
// public package harness/plugin.
//
// See PROTOCOL.md for the versioned wire specification.
package plugin
