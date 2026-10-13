package plugin

import "encoding/json"

// ToolDef declares a tool that a plugin adds to the model's tool list.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`  // text for the model
	InputSchema json.RawMessage `json:"input_schema"` // JSON Schema
}

// ToolExecuteRequest asks a plugin to run one of its tools.
type ToolExecuteRequest struct {
	SessionID string          `json:"session_id"`
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
}

// ToolExecuteResponse is the result of a plugin tool. Output holds text parts
// and blob parts; a part of another type fails the call. A blob part with only
// URL reaches the model as a text line.
type ToolExecuteResponse struct {
	Output  []Part `json:"output"`
	IsError bool   `json:"is_error,omitempty"`
}

// ToolExecuteBeforeRequest is the request of the tool.execute.before hook.
type ToolExecuteBeforeRequest struct {
	SessionID string          `json:"session_id"`
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
}

// ToolExecuteBeforeResponse is the response of the tool.execute.before hook.
type ToolExecuteBeforeResponse struct {
	Args json.RawMessage `json:"args,omitempty"` // non-nil replaces the arguments for the rest of the chain
	Deny string          `json:"deny,omitempty"` // non-empty blocks the call; the model gets it as an error result
}

// ToolExecuteAfterRequest is the request of the tool.execute.after hook.
type ToolExecuteAfterRequest struct {
	SessionID string          `json:"session_id"`
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	Output    []Part          `json:"output"`
}

// ToolExecuteAfterResponse is the response of the tool.execute.after hook.
type ToolExecuteAfterResponse struct {
	Output []Part `json:"output,omitempty"` // non-nil replaces the output for the rest of the chain
}
