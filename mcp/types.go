package mcp

import "encoding/json"

// Implementation describes a client or server implementation, exchanged
// during initialize.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// ClientCapabilities describes optional client-served features. This
// package implements none of roots, sampling, or elicitation, so this is
// sent as an empty object (extensible via Experimental).
type ClientCapabilities struct {
	Experimental map[string]json.RawMessage `json:"experimental,omitempty"`
}

// ToolsCapability describes the server's tools capability.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ServerCapabilities describes optional server-served features. Only the
// `tools` capability is modeled in depth; the rest are captured as raw JSON
// so callers can inspect them without this package needing to model every
// server feature it does not implement.
type ServerCapabilities struct {
	Tools        *ToolsCapability           `json:"tools,omitempty"`
	Prompts      map[string]json.RawMessage `json:"prompts,omitempty"`
	Resources    map[string]json.RawMessage `json:"resources,omitempty"`
	Logging      map[string]json.RawMessage `json:"logging,omitempty"`
	Completions  map[string]json.RawMessage `json:"completions,omitempty"`
	Experimental map[string]json.RawMessage `json:"experimental,omitempty"`
}

// initializeParams is the "initialize" request payload.
type initializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// InitializeResult is the "initialize" response payload.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// Tool describes one tool a server exposes, as returned by tools/list.
type Tool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
}

// listToolsParams is the tools/list request payload.
type listToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// ListToolsResult is the tools/list response payload. A non-empty
// NextCursor means more results are available; pass it as the cursor to
// the next call to Client.ListTools.
type ListToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// ContentType values for Content.Type.
const (
	ContentTypeText         = "text"
	ContentTypeImage        = "image"
	ContentTypeAudio        = "audio"
	ContentTypeResourceLink = "resource_link"
	ContentTypeResource     = "resource"
)

// EmbeddedResource is the payload of a Content item with Type
// ContentTypeResource.
type EmbeddedResource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"` // base64
}

// Content is one item of unstructured tool-result content. Which fields
// are populated depends on Type:
//
//   - "text": Text
//   - "image": Data (base64), MimeType
//   - "audio": Data (base64), MimeType
//   - "resource_link": URI, Name, Description, MimeType
//   - "resource": Resource
type Content struct {
	Type        string            `json:"type"`
	Text        string            `json:"text,omitempty"`
	Data        string            `json:"data,omitempty"`
	MimeType    string            `json:"mimeType,omitempty"`
	URI         string            `json:"uri,omitempty"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Resource    *EmbeddedResource `json:"resource,omitempty"`
	Annotations json.RawMessage   `json:"annotations,omitempty"`
}

// callToolParams is the tools/call request payload.
type callToolParams struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments,omitempty"`
}

// CallToolResult is the tools/call response payload. IsError indicates a
// tool execution error (the call reached the tool but the tool failed);
// this is distinct from an RPCError, which indicates a protocol-level
// failure (e.g. unknown tool name).
type CallToolResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// cancelledParams is the notifications/cancelled payload.
type cancelledParams struct {
	RequestID json.RawMessage `json:"requestId"`
	Reason    string          `json:"reason,omitempty"`
}
