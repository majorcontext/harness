package plugin

import (
	"encoding/json"

	"github.com/majorcontext/harness/message"
)

// Hook names a dispatch point in the engine. Sync hooks chain across plugins
// in config order — each plugin sees the previous plugin's mutations — and
// every sync dispatch carries a deadline.
type Hook string

const (
	// HookEvent is async and fire-and-forget: the full event stream,
	// batched. All other hooks are synchronous.
	HookEvent Hook = "event"
	// HookChatParams mutates model request parameters per request.
	HookChatParams Hook = "chat.params"
	// HookChatMessage mutates a message before it enters the session log.
	HookChatMessage Hook = "chat.message"
	// HookSystemTransform appends segments to the system prompt. It is
	// additive by design and runs after chat.params.
	HookSystemTransform Hook = "system.transform"
	// HookShellEnv injects environment variables into shell commands.
	HookShellEnv Hook = "shell.env"
	// HookToolExecuteBefore rewrites tool arguments or blocks the call.
	HookToolExecuteBefore Hook = "tool.execute.before"
	// HookToolExecuteAfter rewrites or annotates tool results.
	HookToolExecuteAfter Hook = "tool.execute.after"
)

func (h Hook) method() string { return hookMethodPrefix + string(h) }

// Manifest describes a plugin: what it's called, which hooks it subscribes
// to, and which tools it provides. The harness caches manifests at install
// time (keyed by binary hash) so that routing is known at startup without
// spawning anything.
type Manifest struct {
	Name            string    `json:"name"`
	Version         string    `json:"version,omitempty"`
	ProtocolVersion int       `json:"protocol_version"`
	Hooks           []Hook    `json:"hooks,omitempty"`
	Tools           []ToolDef `json:"tools,omitempty"`
}

// ToolDef declares a plugin-provided tool that is added to the model's tool
// list. Execution is dispatched to the plugin via tool/execute.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"` // JSON Schema
}

// InitializeParams is sent by the harness as the first request after
// spawning a plugin process.
type InitializeParams struct {
	ProtocolVersion int    `json:"protocol_version"`
	HarnessVersion  string `json:"harness_version"`
	WorkspaceDir    string `json:"workspace_dir"`
	// HTTPHeaders are headers the harness wants stamped on all plugin
	// outbound HTTP traffic (e.g. workspace attribution). Client.HTTPClient
	// applies them automatically.
	HTTPHeaders map[string]string `json:"http_headers,omitempty"`
	// Config is this plugin's block from the harness config file, verbatim.
	Config json.RawMessage `json:"config,omitempty"`
}

// Event is one entry in the engine's event stream.
type Event struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"session_id,omitempty"`
	Properties json.RawMessage `json:"properties,omitempty"`
}

// Event types, v1. The vocabulary grows as needed.
//
// Deliberately deferred: message-delta events. Streaming text/reasoning
// deltas are high-frequency and need a throttling design (batching or
// coalescing) before they're added to the fire-and-forget event hook —
// see PROTOCOL.md.
const (
	EventSessionStatus    = "session.status"
	EventQuestionAsked    = "question.asked"
	EventFileEdited       = "file.edited"
	EventToolExecuteStart = "tool.execute.start"
	EventToolExecuteEnd   = "tool.execute.end"
	EventSessionError     = "session.error"
)

// FileEditedProperties is the Event.Properties payload for file.edited: a
// file was created or modified by a built-in tool. Path is absolute.
type FileEditedProperties struct {
	Path string `json:"path"`
}

// ToolExecuteStartProperties is the Event.Properties payload for
// tool.execute.start, emitted immediately before a tool (built-in or
// plugin-provided) runs.
type ToolExecuteStartProperties struct {
	Tool   string `json:"tool"`
	CallID string `json:"call_id"`
}

// ToolExecuteEndProperties is the Event.Properties payload for
// tool.execute.end, emitted immediately after a tool finishes. OK is false
// when the tool result is an error result.
type ToolExecuteEndProperties struct {
	Tool   string `json:"tool"`
	CallID string `json:"call_id"`
	OK     bool   `json:"ok"`
}

// SessionErrorProperties is the Event.Properties payload for session.error,
// emitted when a prompt/turn terminates with an error. Message is the error
// string only — no stack traces, no request/response bodies, no secrets.
type SessionErrorProperties struct {
	Message string `json:"message"`
}

// EventBatch is the params of an event hook notification.
type EventBatch struct {
	Events []Event `json:"events"`
}

// ChatParams are the mutable model request parameters. Nil fields mean "use
// the engine default".
type ChatParams struct {
	Model       message.ModelRef `json:"model,omitzero"`
	Temperature *float64         `json:"temperature,omitempty"`
	TopP        *float64         `json:"top_p,omitempty"`
	MaxTokens   *int             `json:"max_tokens,omitempty"`
}

type ChatParamsRequest struct {
	SessionID string     `json:"session_id"`
	Params    ChatParams `json:"params"`
}

type ChatParamsResponse struct {
	Params ChatParams `json:"params"`
}

type ChatMessageRequest struct {
	SessionID string          `json:"session_id"`
	Message   message.Message `json:"message"`
}

type ChatMessageResponse struct {
	Message message.Message `json:"message"`
}

type SystemTransformRequest struct {
	SessionID string           `json:"session_id"`
	Model     message.ModelRef `json:"model,omitzero"`
}

type SystemTransformResponse struct {
	// Segments are appended to the system prompt.
	Segments []string `json:"segments,omitempty"`
}

type ShellEnvRequest struct {
	SessionID string `json:"session_id"`
	Tool      string `json:"tool"`
	Command   string `json:"command"`
	Dir       string `json:"dir,omitempty"`
}

type ShellEnvResponse struct {
	// Env is merged into the command's environment. Later plugins in the
	// chain override earlier ones on key conflicts.
	Env map[string]string `json:"env,omitempty"`
}

type ToolExecuteBeforeRequest struct {
	SessionID string          `json:"session_id"`
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
}

type ToolExecuteBeforeResponse struct {
	// Args, when non-nil, replaces the tool arguments for the rest of the
	// chain and for execution.
	Args json.RawMessage `json:"args,omitempty"`
	// Deny, when non-empty, blocks the tool call. The message is returned
	// to the model as an error tool result and the chain stops.
	Deny string `json:"deny,omitempty"`
}

type ToolExecuteAfterRequest struct {
	SessionID string          `json:"session_id"`
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	Output    message.Parts   `json:"output"`
}

type ToolExecuteAfterResponse struct {
	// Output, when non-nil, replaces the tool output for the rest of the
	// chain and for the model.
	Output message.Parts `json:"output,omitempty"`
}

// ToolExecuteRequest asks a plugin to run one of its manifest-declared tools.
type ToolExecuteRequest struct {
	SessionID string          `json:"session_id"`
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
}

type ToolExecuteResponse struct {
	Output  message.Parts `json:"output"`
	IsError bool          `json:"is_error,omitempty"`
}

// Client API (plugin → harness).

type SessionMessagesRequest struct {
	SessionID string `json:"session_id"`
}

type SessionMessagesResponse struct {
	Messages []message.Message `json:"messages"`
}

type MCPCallRequest struct {
	Server string          `json:"server"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args,omitempty"`
}

type MCPCallResult struct {
	Content message.Parts `json:"content"`
	IsError bool          `json:"is_error,omitempty"`
}

// GenerateRequest is an LLM call through the harness provider layer, so
// plugins inherit model routing, credentials, and observability — they never
// carry their own API keys.
type GenerateRequest struct {
	// Model is a "provider/model" ref or a config alias like "fast".
	Model     string            `json:"model"`
	System    string            `json:"system,omitempty"`
	Messages  []message.Message `json:"messages"`
	MaxTokens int               `json:"max_tokens,omitempty"`
}

type GenerateResponse struct {
	Message message.Message `json:"message"`
}
