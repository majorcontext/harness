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

// MessagePart is one part of a Message. A tool result holds its text in
// Content, and a result with no output reads "(no output)". A blob part names
// its media type and size, never its bytes.
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
}

// Message is one message of the conversation that the model reads. ID is
// stable: it derives from the input or the item that the log recorded.
type Message struct {
	ID    string        `json:"id"`
	Role  string        `json:"role"`
	Parts []MessagePart `json:"parts"`
	// ParentCallID names the call of the parent that started the subagent
	// that wrote the message.
	ParentCallID string `json:"parent_call_id,omitempty"`
}

// MessageCommand is the newest record of a typed slash command, placed after
// the message that was the newest when its first record was appended.
type MessageCommand struct {
	InputID string          `json:"input_id"`
	Line    string          `json:"line"`
	Name    string          `json:"name"`
	Args    map[string]any  `json:"args,omitempty"`
	Status  string          `json:"status"`
	Text    string          `json:"text,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	// ResultTruncated is true when the result was cut to fit the record.
	ResultTruncated bool `json:"result_truncated,omitempty"`
	// AfterMessageID is empty for a command that no message precedes.
	AfterMessageID string `json:"after_message_id,omitempty"`
	// Seq is the seq of the first record of the command.
	Seq uint64 `json:"seq"`
}

// MessagePage is one page of the conversation, oldest first. A message has
// the seq of its place in the conversation: the summary of the newest
// compaction is 1 and the messages after it count on, so a compaction
// renumbers. The page before this one is the page with before=FirstSeq.
type MessagePage struct {
	Messages []Message `json:"messages"`
	// FirstSeq and LastSeq are the seqs of the first and the last message of
	// the page, both 0 for an empty page.
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
	// Total is the number of messages of the whole conversation.
	Total uint64 `json:"total"`
	// HasMore is true when a message older than FirstSeq exists.
	HasMore bool `json:"has_more"`
	// Commands holds the commands that follow a message of the page, and the
	// commands that no message precedes when the page starts at seq 1.
	Commands []MessageCommand `json:"commands"`
}

// MaxMessageLimit is the most messages that one page holds. DefaultMessageLimit
// is the page size of a request with no limit.
const (
	DefaultMessageLimit = 100
	MaxMessageLimit     = 1000
)
