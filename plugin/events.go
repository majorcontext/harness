package plugin

import "encoding/json"

// Event is one entry of the runtime event stream.
type Event struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"session_id,omitempty"`
	Properties json.RawMessage `json:"properties,omitempty"` // payload; its shape depends on Type
}

// The event types of protocol version 1.
const (
	EventSessionStatus    = "session.status"
	EventQuestionAsked    = "question.asked"
	EventFileEdited       = "file.edited"
	EventToolExecuteStart = "tool.execute.start"
	EventToolExecuteEnd   = "tool.execute.end"
	EventSessionError     = "session.error"
)

// FileEditedProperties is the Properties payload of a file.edited event.
type FileEditedProperties struct {
	Path string `json:"path"` // absolute
}

// ToolExecuteStartProperties is the Properties payload of a tool.execute.start event.
type ToolExecuteStartProperties struct {
	Tool   string `json:"tool"`
	CallID string `json:"call_id"`
}

// ToolExecuteEndProperties is the Properties payload of a tool.execute.end event.
type ToolExecuteEndProperties struct {
	Tool   string `json:"tool"`
	CallID string `json:"call_id"`
	OK     bool   `json:"ok"` // false for an error result
}

// SessionErrorProperties is the Properties payload of a session.error event.
type SessionErrorProperties struct {
	Message string `json:"message"` // shortened, with credentials redacted on a best-effort basis
}

// EventBatch is the parameter of an event hook notification.
type EventBatch struct {
	Events []Event `json:"events"` // in emit order
}
