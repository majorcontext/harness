package eventlog

import "encoding/json"

// Event is the payload of one record. Kind names its registered type.
type Event interface {
	Kind() string
}

// Settings are the session settings other than the model.
type Settings struct {
	Effort      string `json:"effort,omitempty"`
	ServiceTier string `json:"service_tier,omitempty"`
}

// Part types.
const (
	PartText       = "text"
	PartReasoning  = "reasoning"
	PartToolCall   = "tool_call"
	PartToolResult = "tool_result"
)

// Part is one piece of message or input content.
type Part struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// Message roles.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is one completed conversation item.
type Message struct {
	Role  string `json:"role"`
	Parts []Part `json:"parts"`
}

// Usage counts the tokens of one turn.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

// SessionCreated is the first record of every session.
type SessionCreated struct {
	ParentID string   `json:"parent_id,omitempty"`
	Model    string   `json:"model"`
	Settings Settings `json:"settings"`
	Origin   string   `json:"origin"`
}

// OwnerAcquired fences every earlier owner of the session.
type OwnerAcquired struct {
	Epoch uint64 `json:"epoch"`
	Owner string `json:"owner"`
}

// SettingsChanged changes each non-nil setting.
type SettingsChanged struct {
	Model       *string `json:"model,omitempty"`
	Effort      *string `json:"effort,omitempty"`
	ServiceTier *string `json:"service_tier,omitempty"`
}

// InputAdmitted queues an input.
type InputAdmitted struct {
	InputID  string   `json:"input_id"`
	Delivery Delivery `json:"delivery"`
	Source   string   `json:"source"`
	Parts    []Part   `json:"parts"`
}

// InputPromoted moves a queued steer input into the running turn.
type InputPromoted struct {
	InputID string `json:"input_id"`
	TurnID  string `json:"turn_id"`
}

// InputWithdrawn removes a queued input.
type InputWithdrawn struct {
	InputID string `json:"input_id"`
}

// TurnStarted starts a turn and promotes the listed queued inputs.
type TurnStarted struct {
	TurnID   string   `json:"turn_id"`
	InputIDs []string `json:"input_ids"`
}

// ItemCompleted records one finished message of a running turn.
type ItemCompleted struct {
	ItemID  string  `json:"item_id"`
	TurnID  string  `json:"turn_id"`
	Message Message `json:"message"`
}

// TurnSuspended pauses a turn at an item boundary for the next owner.
type TurnSuspended struct {
	TurnID string `json:"turn_id"`
	Cause  Cause  `json:"cause"`
}

// TurnResumed continues a suspended turn.
type TurnResumed struct {
	TurnID string `json:"turn_id"`
	Count  int    `json:"count"`
}

// TurnEnded ends a turn. Error carries the Cause of an interrupted turn.
type TurnEnded struct {
	TurnID     string     `json:"turn_id"`
	StopReason StopReason `json:"stop_reason"`
	Error      string     `json:"error,omitempty"`
	Usage      Usage      `json:"usage"`
}

// RequestOpened asks the client for an answer about an item.
type RequestOpened struct {
	RequestID   string          `json:"request_id"`
	ItemID      string          `json:"item_id"`
	RequestKind string          `json:"kind"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

// RequestResolved closes an open request. It is the result of the tool call of the request item.
type RequestResolved struct {
	RequestID  string          `json:"request_id"`
	Resolution Resolution      `json:"resolution"`
	Answer     json.RawMessage `json:"answer,omitempty"`
}

// GoalSet replaces the goal and resets its turn count.
type GoalSet struct {
	Condition string `json:"condition"`
	MaxTurns  int    `json:"max_turns"`
}

// GoalEvaluated records the verdict on the goal after a turn.
type GoalEvaluated struct {
	TurnID   string  `json:"turn_id"`
	Verdict  Verdict `json:"verdict"`
	Guidance string  `json:"guidance,omitempty"`
}

// GoalChanged moves the goal to another state.
type GoalChanged struct {
	State  GoalState `json:"state"`
	Reason string    `json:"reason,omitempty"`
}

// CompactionApplied replaces the records from FromSeq to ToSeq with Summary.
type CompactionApplied struct {
	FromSeq   uint64 `json:"from_seq"`
	ToSeq     uint64 `json:"to_seq"`
	Summary   string `json:"summary"`
	ByBackend bool   `json:"by_backend"`
}

// ChildSpawned records a child session.
type ChildSpawned struct {
	ChildID string `json:"child_id"`
	Agent   string `json:"agent,omitempty"`
}

// ChildSettled records the outcome of a child session.
type ChildSettled struct {
	ChildID   string  `json:"child_id"`
	Outcome   Outcome `json:"outcome"`
	ResultRef string  `json:"result_ref"`
}

// ContextMeasured records the context size of the session.
type ContextMeasured struct {
	Tokens int64  `json:"tokens"`
	Window int64  `json:"window"`
	Source string `json:"source"`
}

// BackendState points at the newest state blob of a backend.
type BackendState struct {
	Backend string `json:"backend"`
	BlobKey string `json:"blob_key"`
}

// Kind returns "session.created".
func (SessionCreated) Kind() string { return "session.created" }

// Kind returns "owner.acquired".
func (OwnerAcquired) Kind() string { return "owner.acquired" }

// Kind returns "settings.changed".
func (SettingsChanged) Kind() string { return "settings.changed" }

// Kind returns "input.admitted".
func (InputAdmitted) Kind() string { return "input.admitted" }

// Kind returns "input.promoted".
func (InputPromoted) Kind() string { return "input.promoted" }

// Kind returns "input.withdrawn".
func (InputWithdrawn) Kind() string { return "input.withdrawn" }

// Kind returns "turn.started".
func (TurnStarted) Kind() string { return "turn.started" }

// Kind returns "item.completed".
func (ItemCompleted) Kind() string { return "item.completed" }

// Kind returns "turn.suspended".
func (TurnSuspended) Kind() string { return "turn.suspended" }

// Kind returns "turn.resumed".
func (TurnResumed) Kind() string { return "turn.resumed" }

// Kind returns "turn.ended".
func (TurnEnded) Kind() string { return "turn.ended" }

// Kind returns "request.opened".
func (RequestOpened) Kind() string { return "request.opened" }

// Kind returns "request.resolved".
func (RequestResolved) Kind() string { return "request.resolved" }

// Kind returns "goal.set".
func (GoalSet) Kind() string { return "goal.set" }

// Kind returns "goal.evaluated".
func (GoalEvaluated) Kind() string { return "goal.evaluated" }

// Kind returns "goal.changed".
func (GoalChanged) Kind() string { return "goal.changed" }

// Kind returns "compaction.applied".
func (CompactionApplied) Kind() string { return "compaction.applied" }

// Kind returns "child.spawned".
func (ChildSpawned) Kind() string { return "child.spawned" }

// Kind returns "child.settled".
func (ChildSettled) Kind() string { return "child.settled" }

// Kind returns "context.measured".
func (ContextMeasured) Kind() string { return "context.measured" }

// Kind returns "backend.state".
func (BackendState) Kind() string { return "backend.state" }
