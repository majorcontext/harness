package eventlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// ErrIllegal reports an event that breaks a state machine or a log invariant.
var ErrIllegal = errors.New("eventlog: illegal transition")

func illegal(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrIllegal}, args...)...)
}

// Status is the session status that State derives from the turn and requests.
type Status string

// Session statuses.
const (
	StatusIdle    Status = "idle"
	StatusRunning Status = "running"
	StatusWaiting Status = "waiting"
)

// Delivery says when an admitted input joins a turn.
type Delivery string

// Deliveries.
const (
	DeliveryQueue Delivery = "queue"
	DeliverySteer Delivery = "steer"
)

// StopReason says how a turn ended.
type StopReason string

// Stop reasons.
const (
	StopCompleted     StopReason = "completed"
	StopInterrupted   StopReason = "interrupted"
	StopFailed        StopReason = "failed"
	StopAwaitingInput StopReason = "awaiting_input"
)

// Cause says why a turn stopped early.
type Cause string

// Causes. Only CauseHandoff suspends a turn and only CauseProviderExhausted
// fails it; the others interrupt it. CauseEnded marks a turn that the end of
// a session stopped in one of its descendants.
const (
	CauseStopped           Cause = "stopped"
	CauseEnded             Cause = "ended"
	CauseGoalCleared       Cause = "goal_cleared"
	CauseHandoff           Cause = "handoff"
	CauseCrashed           Cause = "crashed"
	CauseProviderExhausted Cause = "provider_exhausted"
)

// ErrorClass types the error of a failed turn, so a reader never parses the
// text of the error. A retryable failure holds the class name of the
// provider, such as overloaded or server_error.
type ErrorClass string

// Error classes that do not name a retryable class of the provider.
const (
	ErrorPermanent   ErrorClass = "permanent"
	ErrorTimedOut    ErrorClass = "timed_out"
	ErrorRateLimited ErrorClass = "rate_limited"
	ErrorUnrecovered ErrorClass = "unrecovered"
)

// Resolution says how a request closed.
type Resolution string

// Resolutions.
const (
	ResolutionAnswered  Resolution = "answered"
	ResolutionDismissed Resolution = "dismissed"
)

// GoalState is the state of the session goal.
type GoalState string

// Goal states.
const (
	GoalActive    GoalState = "active"
	GoalPaused    GoalState = "paused"
	GoalAchieved  GoalState = "achieved"
	GoalFailed    GoalState = "failed"
	GoalExhausted GoalState = "exhausted"
	GoalCleared   GoalState = "cleared"
)

// Verdict is the evaluator result for one goal turn.
type Verdict string

// Verdicts.
const (
	VerdictMet        Verdict = "met"
	VerdictNotMet     Verdict = "not_met"
	VerdictImpossible Verdict = "impossible"
)

// Outcome is how a child session settled.
type Outcome string

// Outcomes.
const (
	OutcomeDone     Outcome = "done"
	OutcomeFailed   Outcome = "failed"
	OutcomeCanceled Outcome = "canceled"
)

// Turn is the current turn: running, or suspended for the next owner.
type Turn struct {
	ID        string
	InputIDs  []string
	Suspended bool
	Resumes   int
}

// Goal is the session goal and the number of turns evaluated against it.
type Goal struct {
	Condition string
	MaxTurns  int
	State     GoalState
	Reason    string
	Turns     int
	// Evaluated is the last turn that the goal judged, or the last turn that ended before goal.set.
	Evaluated string
	// Pauses counts the pauses since the last verdict.
	Pauses  int
	RetryAt time.Time
}

// OpenToolCall is a tool call item with no result yet.
type OpenToolCall struct {
	CallID string
	ItemID string
	Name   string
	// answered is set when the user answered its request: the turn that the
	// answer starts records the result.
	answered bool
}

// Summary is the list entry of a session.
type Summary struct {
	ParentID  string
	Origin    string
	Model     string
	Status    Status
	Goal      GoalState
	HeadSeq   uint64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type pendingRequest struct {
	RequestOpened
	turnID string
}

type inputState string

type input struct {
	state inputState
	seq   uint64
	event InputAdmitted
}

const (
	inputAdmitted  inputState = "admitted"
	inputPromoted  inputState = "promoted"
	inputWithdrawn inputState = "withdrawn"
)

// State is the durable session state. Apply is its only mutator.
type State struct {
	created    bool
	parentID   string
	agent      string
	origin     string
	model      string
	settings   Settings
	allowed    []string
	head       uint64
	createdAt  time.Time
	updatedAt  time.Time
	inputs     map[string]input
	queue      []InputAdmitted
	turn       Turn
	turnIDs    map[string]bool
	lastEnded  TurnEnded
	calls      []OpenToolCall
	requests   []pendingRequest
	resolved   map[string]RequestResolved
	goal       Goal
	usage      Usage
	context    ContextMeasured
	subscribed *SubscriptionUsage
	cost       *float64
	compaction CompactionApplied
	// compactedAt is the seq of the record of the newest compaction.
	compactedAt uint64
	compacted   int
	children    map[string]Outcome
	backends    map[string]BackendChain
	retained    []ToolResultRetained
	commands    map[string]command
	history     []entry
	// stranded holds the pinned segments that a compaction outside a turn
	// folded. The model reads them at the end of the history, and the next
	// turn start settles them there, after its inputs.
	stranded []entry
	// turnAt is the length of history when the current turn started.
	turnAt int
	// turnBy is the provider of the model when the newest turn started, turnN
	// counts the turns that started, and turnItems counts the items of the
	// current turn. unran lists the turns that ended with no item of their own.
	turnBy    string
	turnN     int
	turnItems int
	unran     []int
}

func (s *State) clone() *State {
	c := *s
	c.inputs = maps.Clone(s.inputs)
	c.queue = slices.Clone(s.queue)
	c.turnIDs = maps.Clone(s.turnIDs)
	c.calls = slices.Clone(s.calls)
	c.requests = slices.Clone(s.requests)
	c.children = maps.Clone(s.children)
	c.backends = maps.Clone(s.backends)
	c.commands = maps.Clone(s.commands)
	// A full cap makes an append to c copy, so c never writes into s.history.
	c.history = s.history[:len(s.history):len(s.history)]
	c.stranded = slices.Clip(s.stranded)
	c.retained = slices.Clip(s.retained)
	c.unran = slices.Clip(s.unran)
	return &c
}

// Head returns the seq of the last applied record, or 0.
func (s *State) Head() uint64 { return s.head }

// Status returns the session status.
func (s *State) Status() Status {
	switch {
	case s.turn.ID != "" && !s.turn.Suspended:
		return StatusRunning
	case s.lastEnded.StopReason == StopAwaitingInput && len(s.requests) > 0:
		return StatusWaiting
	}
	return StatusIdle
}

// Turn returns the current turn, if any.
func (s *State) Turn() (Turn, bool) {
	t := s.turn
	t.InputIDs = slices.Clone(t.InputIDs)
	return t, t.ID != ""
}

// Queue returns the admitted inputs that no turn has taken, oldest first.
func (s *State) Queue() []InputAdmitted {
	out := make([]InputAdmitted, len(s.queue))
	for i, in := range s.queue {
		out[i] = cloneInput(in)
	}
	return out
}

func cloneInput(in InputAdmitted) InputAdmitted {
	in.Parts = cloneParts(in.Parts)
	return in
}

// Input returns an admitted input and the seq of its input.admitted record.
func (s *State) Input(id string) (InputAdmitted, uint64, bool) {
	in, ok := s.inputs[id]
	return cloneInput(in.event), in.seq, ok
}

// Requests returns the open requests, oldest first.
func (s *State) Requests() []RequestOpened {
	out := make([]RequestOpened, len(s.requests))
	for i, r := range s.requests {
		out[i] = r.RequestOpened
		out[i].Payload = slices.Clone(r.Payload)
	}
	return out
}

// Resolution returns the record that closed request id, if it ever opened and closed.
func (s *State) Resolution(id string) (RequestResolved, bool) {
	r, ok := s.resolved[id]
	r.Answer = slices.Clone(r.Answer)
	return r, ok
}

// OpenToolCalls returns the tool calls with no result, oldest first.
func (s *State) OpenToolCalls() []OpenToolCall { return slices.Clone(s.calls) }

// Goal returns the session goal, if one was set.
func (s *State) Goal() (Goal, bool) { return s.goal, s.goal.State != "" }

// LastEnded returns the turn.ended record of the newest ended turn.
func (s *State) LastEnded() TurnEnded { return s.lastEnded }

// Model returns the session model.
func (s *State) Model() string { return s.model }

// Settings returns the session settings other than the model.
func (s *State) Settings() Settings { return s.settings }

// AllowedTools returns the tool names of the session: the embedder tools,
// and the built-in tools of a delegated backend. nil allows every tool.
func (s *State) AllowedTools() []string { return slices.Clone(s.allowed) }

// Agent returns the profile of a child session, or "".
func (s *State) Agent() string { return s.agent }

// Unsettled returns the spawned children that have not settled, sorted.
func (s *State) Unsettled() []string {
	var out []string
	for id, o := range s.children {
		if o == "" {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// Resumable reports whether opening the session has work to do: a turn that
// runs or is suspended, an input that waits (not after a provider_exhausted
// turn, which holds the queue), a goal that is active or paused, a command
// that no owner finished, or a child that has not settled.
func (s *State) Resumable() bool {
	held := s.lastEnded.Cause == CauseProviderExhausted
	return s.turn.ID != "" || (len(s.queue) > 0 && !held) || s.goal.State == GoalActive || s.goal.State == GoalPaused ||
		len(s.Unfinished()) > 0 || len(s.Unsettled()) > 0
}

// Children returns every spawned child, settled or not, sorted.
func (s *State) Children() []string { return slices.Sorted(maps.Keys(s.children)) }

// Usage returns the token usage summed over every recorded model call and
// every summary call. A goal evaluator call records none.
func (s *State) Usage() Usage { return s.usage }

// Context returns the newest context measurement, with no usage.
func (s *State) Context() ContextMeasured { return s.context }

// SubscriptionUsage returns the newest snapshot with the summed cost, or nil.
// A cost with no snapshot gets an empty snapshot of the claude lane, with no capture time.
func (s *State) SubscriptionUsage() *SubscriptionUsage {
	if s.cost == nil {
		return s.subscribed
	}
	var u SubscriptionUsage
	if s.subscribed != nil {
		u = *s.subscribed
	} else {
		u = SubscriptionUsage{Provider: "claude", Windows: []SubscriptionUsageWindow{}}
	}
	cost := *s.cost
	u.SessionCostUSD = &cost
	return &u
}

// CompactionCount returns the number of compactions, by the harness or by a backend.
func (s *State) CompactionCount() int { return s.compacted }

// Compaction returns the newest compaction. Replay of history starts there.
func (s *State) Compaction() (CompactionApplied, bool) {
	return s.compaction, s.compaction.ToSeq != 0
}

// BackendChain is the state of a backend that its backend.state records
// fold into: the newest head and the chunk blobs of the current chain in
// order. Legacy is the blob of a record of the older form, which holds the
// head and the entries, and no chunk then.
type BackendChain struct {
	Head    json.RawMessage
	Chunks  []string
	Legacy  string
	Entries int
	Sum     string
}

// BackendState returns the state of backend and whether it saved any.
func (s *State) BackendState(backend string) (BackendChain, bool) {
	c, ok := s.backends[backend]
	c.Chunks = slices.Clone(c.Chunks)
	return c, ok
}

// Retained returns the retained tool results in the order of their records.
func (s *State) Retained() []ToolResultRetained { return slices.Clone(s.retained) }

// Summary returns the list entry of the session.
func (s *State) Summary() Summary {
	return Summary{
		ParentID:  s.parentID,
		Origin:    s.origin,
		Model:     s.model,
		Status:    s.Status(),
		Goal:      s.goal.State,
		HeadSeq:   s.head,
		CreatedAt: s.createdAt,
		UpdatedAt: s.updatedAt,
	}
}

// Apply decodes r and applies it. On error, s is unchanged.
func (s *State) Apply(r Record) error {
	env, err := Decode(r.Data)
	if err != nil {
		return err
	}
	if env.Seq != r.Seq {
		return fmt.Errorf("%w: record %d holds envelope seq %d", ErrSeq, r.Seq, env.Seq)
	}
	return s.apply(env)
}

func (s *State) apply(env Envelope) error {
	if env.Seq != s.head+1 {
		return fmt.Errorf("%w: got %d, want %d", ErrSeq, env.Seq, s.head+1)
	}
	if err := s.step(env); err != nil {
		return err
	}
	s.remember(env)
	s.head = env.Seq
	s.updatedAt = env.Time
	return nil
}

func (s *State) step(env Envelope) error {
	if env.Event == nil {
		return fmt.Errorf("%w: nil event", ErrUnknownKind)
	}
	if c, ok := env.Event.(SessionCreated); ok {
		return s.applyCreated(c, env.Time)
	}
	if !s.created {
		return illegal("%s before session.created", env.Event.Kind())
	}
	switch e := env.Event.(type) {
	case OwnerAcquired:
		return nil
	case SettingsChanged:
		return s.applySettings(e)
	case InputAdmitted:
		return s.applyAdmitted(e, env.Seq)
	case InputPromoted:
		return s.applyPromoted(e)
	case InputWithdrawn:
		return s.applyWithdrawn(e)
	case TurnStarted:
		return s.applyStarted(e)
	case ItemCompleted:
		return s.applyItem(e)
	case TurnSuspended:
		return s.applySuspended(e)
	case TurnResumed:
		return s.applyResumed(e)
	case TurnEnded:
		return s.applyEnded(e)
	case RequestOpened:
		return s.applyRequestOpened(e)
	case RequestResolved:
		return s.applyRequestResolved(e, env.Seq)
	case GoalSet:
		return s.applyGoalSet(e)
	case GoalEvaluated:
		return s.applyGoalEvaluated(e)
	case GoalChanged:
		return s.applyGoalChanged(e)
	case CompactionApplied:
		return s.applyCompaction(e, env.Seq)
	case ChildSpawned:
		return s.applyChildSpawned(e)
	case ChildSettled:
		return s.applyChildSettled(e)
	case CommandRecorded:
		return s.applyCommand(e, env.Seq)
	case ContextMeasured:
		s.applyMeasured(e)
		return nil
	case BackendState:
		return s.applyBackendState(e)
	case ToolResultRetained:
		return s.applyRetained(e)
	}
	return fmt.Errorf("%w: %T", ErrUnknownKind, env.Event)
}
