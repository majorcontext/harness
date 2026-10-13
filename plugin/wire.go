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
	HookEvent             Hook = "event"               // receives the event stream in batches; not waited for
	HookChatParams        Hook = "chat.params"         // changes the parameters of each model call
	HookChatMessage       Hook = "chat.message"        // part of the protocol; the runtime never dispatches it
	HookSystemTransform   Hook = "system.transform"    // adds segments to the system prompt of each model call
	HookShellEnv          Hook = "shell.env"           // adds environment variables to each bash command
	HookToolExecuteBefore Hook = "tool.execute.before" // replaces the arguments of a tool call or denies it
	HookToolExecuteAfter  Hook = "tool.execute.after"  // replaces the result text of a tool call that ran
)

// Manifest describes a plugin: its name, its hooks, and its tools. Plugin.Manifest
// builds it.
type Manifest struct {
	Name            string       `json:"name"`
	Version         string       `json:"version,omitempty"`
	ProtocolVersion int          `json:"protocol_version"`
	Hooks           []Hook       `json:"hooks,omitempty"`
	DenyOnFailure   []Hook       `json:"deny_on_failure,omitempty"` // hooks that deny the call when they fail
	HookTimeouts    map[Hook]int `json:"hook_timeouts,omitempty"`   // deadline in ms, 1 to 600000
	Tools           []ToolDef    `json:"tools,omitempty"`
}

// InitializeParams is the wire record of the first request that the host sends
// to a plugin. Client reports its values.
type InitializeParams struct {
	ProtocolVersion int               `json:"protocol_version"`
	HarnessVersion  string            `json:"harness_version"`
	WorkspaceDir    string            `json:"workspace_dir"` // may be empty
	HTTPHeaders     map[string]string `json:"http_headers,omitempty"`
	Config          json.RawMessage   `json:"config,omitempty"`    // the config block of the plugin, verbatim
	ServeURL        string            `json:"serve_url,omitempty"` // empty when the harness serves no HTTP API
	RunToken        string            `json:"run_token,omitempty"` // bearer token for ServeURL
}

// SessionMessagesRequest is the wire record of Client.SessionMessages.
type SessionMessagesRequest struct {
	SessionID string `json:"session_id"`
}

// SessionMessagesResponse is the wire record of the reply to
// Client.SessionMessages.
type SessionMessagesResponse struct {
	Messages []Message `json:"messages"` // in log order
}

// Message is one message of a session, with the JSON of the canonical message.
type Message struct {
	ID              string    `json:"id"`
	Role            string    `json:"role"` // user, assistant, or tool
	Parts           []Part    `json:"parts"`
	Model           string    `json:"model,omitzero"` // provider/model reference of an assistant message
	CreatedAt       time.Time `json:"created_at,omitzero"`
	Origin          string    `json:"origin,omitempty"`             // display label of the source beyond Role; empty for an ordinary message
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
