package plugin

import "encoding/json"

// Event is one entry of the runtime event stream. Delivery is best effort.
type Event struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"session_id,omitempty"`
	Properties json.RawMessage `json:"properties,omitempty"` // payload; its shape depends on Type
}

// The event types of protocol version 1. The package documentation says when
// the runtime sends each one.
const (
	EventSessionStatus    = "session.status"     // {"status": "busy"} or {"status": "idle"}
	EventQuestionAsked    = "question.asked"     // reserved; the runtime does not send it
	EventFileEdited       = "file.edited"        // FileEditedProperties
	EventToolExecuteStart = "tool.execute.start" // ToolExecuteStartProperties
	EventToolExecuteEnd   = "tool.execute.end"   // ToolExecuteEndProperties
	EventSessionError     = "session.error"      // SessionErrorProperties
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
	Message string `json:"message"` // at most 256 characters; credential redaction is best effort, so treat it as sensitive
}

// EventBatch is the wire record of an event hook notification.
type EventBatch struct {
	Events []Event `json:"events"` // in emit order
}
