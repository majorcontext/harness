package protocol

import "encoding/json"

// Message part types.
const (
	MessagePartText          = "text"
	MessagePartReasoning     = "reasoning"
	MessagePartToolCall      = "tool_call"
	MessagePartToolResult    = "tool_result"
	MessagePartBlob          = "blob"
	MessagePartTaskReport    = "task_report"
	MessagePartEngineContext = "engine_context"
)

// MessagePart is one part of a Message. A tool result holds its text in Content; a blob part names its media type, size, and Key, which GET /sessions/{id}/blobs/{key} reads.
type MessagePart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	Bytes     int             `json:"bytes,omitempty"`
	Key       string          `json:"key,omitempty"`
}

// Message is one message of the conversation that the model reads. Its ID derives from the log.
type Message struct {
	ID    string        `json:"id"`
	Role  string        `json:"role"`
	Parts []MessagePart `json:"parts"`
	// ParentCallID names the call that started the subagent of the message.
	ParentCallID string `json:"parent_call_id,omitempty"`
	// Source, SourceID, and SourceLabel are the provenance of the input that started a turn with this user message.
	Source      string `json:"source,omitempty"`
	SourceID    string `json:"source_id,omitempty"`
	SourceLabel string `json:"source_label,omitempty"`
	// OperatorBatch holds one entry for each input that a user message of steer inputs joined, in the order that the message numbers them.
	OperatorBatch []OperatorBatchEntry `json:"operator_batch,omitempty"`
}

// OperatorBatchEntry is one input that joined a running turn, with its provenance.
type OperatorBatchEntry struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	Source      string `json:"source"`
	SourceID    string `json:"source_id,omitempty"`
	SourceLabel string `json:"source_label,omitempty"`
}

// MessageCommand is the newest record of a typed slash command.
type MessageCommand struct {
	InputID         string          `json:"input_id"`
	Line            string          `json:"line"`
	Name            string          `json:"name"`
	Args            map[string]any  `json:"args,omitempty"`
	Status          string          `json:"status"`
	Text            string          `json:"text,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	ResultTruncated bool            `json:"result_truncated,omitempty"`
	AfterMessageID  string          `json:"after_message_id,omitempty"`
	Seq             uint64          `json:"seq"`
}

// MessagePage is one page of the conversation, oldest first, numbered from 1; a compaction renumbers.
type MessagePage struct {
	Messages []Message `json:"messages"`
	// FirstSeq and LastSeq are 0 for an empty page.
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
	// Total is the number of messages of the whole conversation.
	Total uint64 `json:"total"`
	// HasMore is true when a message older than FirstSeq exists.
	HasMore bool `json:"has_more"`
	// Commands holds the commands that follow a message of the page.
	Commands []MessageCommand `json:"commands"`
}

// DefaultMessageLimit is the page size of a request with no limit, and MaxMessageLimit the most that one page holds.
const (
	DefaultMessageLimit = 100
	MaxMessageLimit     = 1000
)
