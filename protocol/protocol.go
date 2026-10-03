// Package protocol holds the data types that the Go API and HTTP share.
package protocol

import (
	"encoding/json"
	"time"
)

// Session statuses.
const (
	StatusIdle    = "idle"
	StatusRunning = "running"
	StatusWaiting = "waiting"
)

// Input deliveries.
const (
	DeliveryQueue = "queue"
	DeliverySteer = "steer"
)

// PartText is the type of a text part.
const PartText = "text"

// CreateSession is the request that creates a session.
type CreateSession struct {
	// ID is minted by the client, or by the runtime when it is empty.
	ID          string `json:"id,omitempty"`
	Model       string `json:"model"`
	Effort      string `json:"effort,omitempty"`
	ServiceTier string `json:"service_tier,omitempty"`
	Origin      string `json:"origin,omitempty"`
	// AllowedTools names the tools that the model sees and may call: the
	// embedder tools, and the built-in tools of a backend that runs its own
	// loop. nil allows every tool; an empty list allows none. For a backend
	// that runs its own loop, any other name fails Create.
	AllowedTools []string `json:"allowed_tools,omitzero"`
}

// ToolSpec describes a tool to the model.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolCall is one call of a tool by the model. ID is the model's call ID.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult is what a tool call returns to the model.
type ToolResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
}

// Usage counts tokens.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

// Session is a view of one session at HeadSeq.
type Session struct {
	ID          string    `json:"id"`
	ParentID    string    `json:"parent_id,omitempty"`
	Origin      string    `json:"origin"`
	Model       string    `json:"model"`
	Effort      string    `json:"effort,omitempty"`
	ServiceTier string    `json:"service_tier,omitempty"`
	Status      string    `json:"status"`
	TurnID      string    `json:"turn_id,omitempty"`
	Queued      []string  `json:"queued,omitempty"`
	Usage       Usage     `json:"usage"`
	HeadSeq     uint64    `json:"head_seq"`
	SyncedSeq   uint64    `json:"synced_seq"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Part is one piece of input content.
type Part struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Input is a client input. The client mints ID; a repeat of the same ID is idempotent.
type Input struct {
	ID       string `json:"id"`
	Parts    []Part `json:"parts"`
	Delivery string `json:"delivery,omitempty"`
	Source   string `json:"source,omitempty"`
	// ExpectedTurnID makes a steer input fail unless that turn is running.
	ExpectedTurnID string `json:"expected_turn_id,omitempty"`
}

// Admitted is the receipt of an input: the seq of its input.admitted record.
type Admitted struct {
	InputID string `json:"input_id"`
	Seq     uint64 `json:"seq"`
}

// Interrupt stops the running turn, or only the named turn when TurnID is set.
type Interrupt struct {
	TurnID string `json:"turn_id,omitempty"`
}

// Event is one durable record of a session log.
type Event struct {
	Seq  uint64          `json:"seq"`
	Time time.Time       `json:"t"`
	Kind string          `json:"k"`
	Data json.RawMessage `json:"d"`
}

// ListSessions selects a page of sessions in ID order.
type ListSessions struct {
	After string `json:"after,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// SessionPage is one page of sessions. Next is the After of the next page, or empty.
type SessionPage struct {
	Sessions []Session `json:"sessions"`
	Next     string    `json:"next,omitempty"`
}

// SyncBatch is a remote append of session records from FromSeq, under the sender's Ownership epoch.
type SyncBatch struct {
	Epoch   uint64            `json:"epoch"`
	Session string            `json:"session"`
	FromSeq uint64            `json:"from_seq"`
	Records [][]byte          `json:"records"`
	Blobs   map[string][]byte `json:"blobs,omitempty"`
}

// SyncAck is the head of the receiver after a SyncBatch, also on a seq mismatch.
type SyncAck struct {
	Head uint64 `json:"head"`
}
