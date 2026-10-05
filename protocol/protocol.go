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

// Input deliveries. An input with no delivery is steer.
const (
	DeliveryQueue = "queue"
	DeliverySteer = "steer"
)

// Input part types.
const (
	PartText = "text"
	PartBlob = "blob"
)

// CreateSession is the request that creates a session.
type CreateSession struct {
	// ID is minted by the client, or by the runtime when it is empty.
	ID string `json:"id,omitempty"`
	// Model is a provider/model ref. Empty selects the configured default.
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

// Context is the context gauge of a session: the prompt size of the newest
// model call that measured one, and the window of its model. Zero means unknown.
type Context struct {
	Tokens int64 `json:"tokens"`
	Window int64 `json:"window"`
}

// LastTurn is the outcome of the newest turn that ended. StopReason is a
// stop reason of the turn.ended record, and Error carries its cause or message.
type LastTurn struct {
	TurnID     string `json:"turn_id"`
	StopReason string `json:"stop_reason"`
	Error      string `json:"error,omitempty"`
}

// SubscriptionUsage is the subscription limit snapshot that a provider
// reported with the newest model call that carried one. Provider is claude
// or codex, and CapturedAt is in Unix seconds.
type SubscriptionUsage struct {
	Provider   string                    `json:"provider"`
	Plan       string                    `json:"plan"`
	Windows    []SubscriptionUsageWindow `json:"windows"`
	Overage    *SubscriptionOverage      `json:"overage,omitempty"`
	CapturedAt int64                     `json:"captured_at"`
}

// SubscriptionUsageWindow is one rate-limit window of a SubscriptionUsage.
type SubscriptionUsageWindow struct {
	Key         string  `json:"key"`
	Label       string  `json:"label"`
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at"`
}

// SubscriptionOverage is the pay-as-you-go state of a subscription.
type SubscriptionOverage struct {
	InUse    bool   `json:"in_use"`
	Status   string `json:"status"`
	ResetsAt int64  `json:"resets_at"`
}

// Session is a view of one session at HeadSeq. Usage sums every model and
// summary call, but no goal evaluator call. SubscriptionUsage is null until a
// call carries a snapshot. Plugins lists each configured plugin as the runtime
// that serves the view knows it; the log does not hold it.
type Session struct {
	ID                string             `json:"id"`
	ParentID          string             `json:"parent_id,omitempty"`
	Origin            string             `json:"origin"`
	Model             string             `json:"model"`
	Effort            string             `json:"effort,omitempty"`
	ServiceTier       string             `json:"service_tier,omitempty"`
	Status            string             `json:"status"`
	TurnID            string             `json:"turn_id,omitempty"`
	Queued            []string           `json:"queued,omitempty"`
	Goal              *GoalView          `json:"goal,omitempty"`
	Usage             Usage              `json:"usage"`
	Context           Context            `json:"context"`
	LastTurn          *LastTurn          `json:"last_turn,omitempty"`
	CompactionCount   int                `json:"compaction_count,omitempty"`
	SubscriptionUsage *SubscriptionUsage `json:"subscription_usage"`
	Plugins           []Plugin           `json:"plugins,omitempty"`
	HeadSeq           uint64             `json:"head_seq"`
	SyncedSeq         uint64             `json:"synced_seq"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
}

// Plugin is a configured plugin. State is not-spawned, running, or errored.
type Plugin struct {
	Name  string   `json:"name"`
	State string   `json:"state"`
	Tools []string `json:"tools"`
	Hooks []string `json:"hooks"`
}

// Goal is a condition that the session works toward, turn after turn,
// until an evaluator judges it met. MaxTurns bounds the goal turns; 0 is
// unlimited.
type Goal struct {
	Condition string `json:"condition"`
	MaxTurns  int    `json:"max_turns,omitempty"`
}

// GoalView is the goal of a session. State is active, paused, achieved,
// failed, exhausted, or cleared. Turns counts the judged turns. A paused
// goal runs again at RetryAt, or at the next input.
type GoalView struct {
	Goal
	State   string    `json:"state"`
	Turns   int       `json:"turns"`
	Reason  string    `json:"reason,omitempty"`
	RetryAt time.Time `json:"retry_at,omitzero"`
}

// Part is one piece of input content. A blob part carries one attachment in Data.
type Part struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Data      []byte `json:"data,omitempty"`
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

// Admitted is the receipt of an input: the seq of its input.admitted
// record. A typed slash command has no input.admitted record: Seq is its
// first command.recorded record, and Command is its newest status. Repeat
// is true when the session had already admitted the input; it is not part of
// the wire body.
type Admitted struct {
	InputID string `json:"input_id"`
	Seq     uint64 `json:"seq"`
	Command string `json:"command,omitempty"`
	Repeat  bool   `json:"-"`
}

// Interrupt stops the running turn, or only the named turn when TurnID is
// set. Tree also stops the turn of each descendant of the session.
type Interrupt struct {
	TurnID string `json:"turn_id,omitempty"`
	Tree   bool   `json:"tree,omitempty"`
}

// Compact folds the turns before the newest KeepTurns, or before the
// newest compaction_keep_turns when KeepTurns is nil. KeepTurns is at least 1.
type Compact struct {
	KeepTurns *int `json:"keep_turns,omitempty"`
}

// Compacted is the result of a compaction. Folded is false when the session
// had too few turns to fold, and then nothing else is set. A compaction by
// the backend of the session has only ByBackend and Folded.
type Compacted struct {
	FromSeq   uint64 `json:"from_seq,omitempty"`
	ToSeq     uint64 `json:"to_seq,omitempty"`
	ByBackend bool   `json:"by_backend,omitempty"`
	Folded    bool   `json:"folded"`
}

// Resolution answers an open request with the answer of the user as JSON, or
// dismisses it. Exactly one is set. A question takes a map of question to choice.
type Resolution struct {
	Answer  json.RawMessage `json:"answer,omitempty"`
	Dismiss bool            `json:"dismiss,omitempty"`
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
// the item_id of the item.completed record that ends the item, unless a
// failed attempt started the item: that item never completes.
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

// SettingsPatch changes each non-nil setting of a session. A running turn
// uses the new settings from its next model call.
type SettingsPatch struct {
	Model       *string `json:"model,omitempty"`
	Effort      *string `json:"effort,omitempty"`
	ServiceTier *string `json:"service_tier,omitempty"`
}

// EventPage is one page of durable events. Next is the after of the next
// page, or 0 when the page reaches the head.
type EventPage struct {
	Events []Event `json:"events"`
	Next   uint64  `json:"next,omitempty"`
}

// Error codes of the HTTP API.
const (
	CodeInvalidRequest    = "invalid_request"
	CodeSessionNotFound   = "session_not_found"
	CodeSessionExists     = "session_exists"
	CodeSessionNotOwned   = "session_not_owned"
	CodeInputConflict     = "input_conflict"
	CodeTurnMismatch      = "turn_mismatch"
	CodeSessionBusy       = "session_busy"
	CodeRequestNotPending = "request_not_pending"
	CodeModelUnavailable  = "model_unavailable"
	CodePayloadTooLarge   = "payload_too_large"
	CodeDraining          = "draining"
	CodeInternal          = "internal"
	CodeNotAGitRepo       = "not_a_git_repo"
	CodeNoBase            = "no_base"
	CodeTooManyChanges    = "too_many_changes"
	CodeProcessNotFound   = "process_not_found"
)

// ErrorBody is the body of every HTTP error response.
type ErrorBody struct {
	Error Error `json:"error"`
}

// Error is a failed HTTP request: a code from the Code constants and a message.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}
