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
	// StatusRetrying appears only in status frames.
	StatusRetrying = "retrying"
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

// Event is one durable record of a session log, or an ephemeral frame of a
// live subscription. An ephemeral frame is never stored; its Seq is the last
// durable seq when it was sent.
type Event struct {
	Seq       uint64          `json:"seq"`
	Time      time.Time       `json:"t"`
	Kind      string          `json:"k"`
	Data      json.RawMessage `json:"d"`
	Ephemeral bool            `json:"ephemeral,omitempty"`
}

// Ephemeral frame kinds.
const (
	KindItemStarted = "item.started"
	KindItemDelta   = "item.delta"
	KindStatus      = "status"
)

// ItemFrame is the data of an item.started or item.delta frame. ItemID is
// the item_id of the item.completed record that ends the item.
type ItemFrame struct {
	ItemID string `json:"item_id"`
	TurnID string `json:"turn_id"`
	// Type is "text" or "reasoning" in an item.delta frame.
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// StatusFrame is the data of a status frame. A retrying turn waits until
// NextAt before its attempt number Attempt. An item that the failed attempt
// started never completes.
type StatusFrame struct {
	Status  string    `json:"status"`
	TurnID  string    `json:"turn_id"`
	Attempt int       `json:"attempt,omitempty"`
	NextAt  time.Time `json:"next_at,omitzero"`
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

// Model is a model that a configured provider serves.
type Model struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	// ContextWindow is 0 when the backend reports the window during a turn.
	ContextWindow int `json:"context_window"`
}

// SettingsPatch changes each non-nil setting of a session. The next turn
// uses the new settings; a running turn keeps its own.
type SettingsPatch struct {
	Model       *string `json:"model,omitempty"`
	Effort      *string `json:"effort,omitempty"`
	ServiceTier *string `json:"service_tier,omitempty"`
}
