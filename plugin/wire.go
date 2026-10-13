package plugin

import (
	"encoding/json"
	"time"
)

// ProtocolVersion is the version of the plugin protocol that this package speaks.
const ProtocolVersion = 1

// Hook names a dispatch point in the runtime.
type Hook string

// The hooks that a plugin can subscribe to.
const (
	// HookEvent receives the event stream in batches. It is asynchronous.
	HookEvent Hook = "event"
	// HookChatParams changes the model request parameters of each model call.
	HookChatParams Hook = "chat.params"
	// HookChatMessage changes a message before it is logged; the runtime does not dispatch it.
	HookChatMessage Hook = "chat.message"
	// HookSystemTransform appends segments to the system prompt.
	HookSystemTransform Hook = "system.transform"
	// HookShellEnv adds environment variables to shell commands.
	HookShellEnv Hook = "shell.env"
	// HookToolExecuteBefore rewrites the arguments of a tool call or blocks it.
	HookToolExecuteBefore Hook = "tool.execute.before"
	// HookToolExecuteAfter rewrites or annotates a tool result.
	HookToolExecuteAfter Hook = "tool.execute.after"
)

// Manifest describes a plugin: its name, its hooks, and its tools.
type Manifest struct {
	Name            string       `json:"name"`
	Version         string       `json:"version,omitempty"`
	ProtocolVersion int          `json:"protocol_version"`
	Hooks           []Hook       `json:"hooks,omitempty"`
	DenyOnFailure   []Hook       `json:"deny_on_failure,omitempty"` // hooks that deny the call when they fail
	HookTimeouts    map[Hook]int `json:"hook_timeouts,omitempty"`   // deadline in ms, 1 to 600000
	Tools           []ToolDef    `json:"tools,omitempty"`
}

// InitializeParams is the first request that the host sends to a plugin.
type InitializeParams struct {
	ProtocolVersion int               `json:"protocol_version"`
	HarnessVersion  string            `json:"harness_version"`
	WorkspaceDir    string            `json:"workspace_dir"` // may be empty
	HTTPHeaders     map[string]string `json:"http_headers,omitempty"`
	Config          json.RawMessage   `json:"config,omitempty"`    // the config block of the plugin, verbatim
	ServeURL        string            `json:"serve_url,omitempty"` // empty when the harness serves no HTTP API
	RunToken        string            `json:"run_token,omitempty"` // bearer token for ServeURL
}

// SessionMessagesRequest asks the host for the messages of a session.
type SessionMessagesRequest struct {
	SessionID string `json:"session_id"`
}

// SessionMessagesResponse holds the messages of a session.
type SessionMessagesResponse struct {
	Messages []Message `json:"messages"` // in log order
}

// Message is one message of a session, with the JSON of the canonical message.
type Message struct {
	ID              string    `json:"id"`
	Role            string    `json:"role"` // user, assistant, or tool
	Parts           []Part    `json:"parts"`
	Model           string    `json:"model,omitzero"`
	CreatedAt       time.Time `json:"created_at,omitzero"`
	Origin          string    `json:"origin,omitempty"`
	ParentToolUseID string    `json:"parent_tool_use_id,omitempty"` // tool call that started the subagent turn
}

// MarshalJSON encodes m with an empty array for nil Parts.
func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	if m.Parts == nil {
		m.Parts = []Part{}
	}
	return json.Marshal(plain(m))
}
