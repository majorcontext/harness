package eventlog

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/majorcontext/harness/protocol"
)

// ErrIllegal reports an event that breaks a state machine or a log invariant.
var ErrIllegal = errors.New("eventlog: illegal transition")

func illegal(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrIllegal}, args...)...)
}

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
	State     protocol.GoalState
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
	WorkDir   string
	Model     string
	Title     string
	Status    protocol.Status
	Goal      protocol.GoalState
	HeadSeq   uint64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type pendingRequest struct {
	protocol.RequestOpened
	turnID string
}

type inputState string

type input struct {
	state inputState
	seq   uint64
	event protocol.InputAdmitted
	// turnID is the turn that took the input, once promoted.
	turnID string
}

const (
	inputAdmitted  inputState = "admitted"
	inputPromoted  inputState = "promoted"
	inputWithdrawn inputState = "withdrawn"
)

// State is the durable session state. Apply is its only mutator.
type State struct {
	created  bool
	parentID string
	agent    string
	origin   string
	workDir  string
	model    string
	// renamed is the newest title of a person, modeled the newest title of a model, and derived the title of the first prompt.
	renamed, modeled, derived string
	settings                  protocol.Settings
	allowed                   []string
	createdAt                 time.Time
	updatedAt                 time.Time
	inputs                    map[string]input
	prog                      protocol.Progress
	turn                      Turn
	turnIDs                   map[string]bool
	lastEnded                 protocol.TurnEnded
	calls                     []OpenToolCall
	requests                  map[string]pendingRequest
	resolved                  map[string]protocol.RequestResolved
	asked                     map[string]json.RawMessage
	// evaluated is the last turn that the goal judged, or the last turn that ended before goal.set.
	evaluated string
	usage     protocol.Usage
	context   protocol.ContextMeasured
	// measuredTurn is the turn count when the newest reading was recorded.
	measuredTurn int
	subscribed   *protocol.SubscriptionUsage
	cost         *float64
	compaction   protocol.CompactionApplied
	// compactedAt is the seq of the record of the newest compaction.
	compactedAt uint64
	// compactedTime is the time of that record.
	compactedTime time.Time
	compacted     int
	// compactedLen is the length of ModelHistory right after that compaction.
	compactedLen int
	children     map[string]protocol.Outcome
	backends     map[string]BackendChain
	retained     []protocol.ToolResultRetained
	commands     map[string]command
	history      []entry
	// folded holds the messages that readers see before the summary of the
	// newest compaction: what each compaction folded, and the summary of each
	// earlier compaction in its place. The model never reads it.
	folded []protocol.Message
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
	c.turnIDs = maps.Clone(s.turnIDs)
	c.calls = slices.Clone(s.calls)
	c.requests = maps.Clone(s.requests)
	c.children = maps.Clone(s.children)
	c.backends = maps.Clone(s.backends)
	c.commands = maps.Clone(s.commands)
	// A full cap makes an append to c copy, so c never writes into s.history.
	c.history = s.history[:len(s.history):len(s.history)]
	c.stranded = slices.Clip(s.stranded)
	c.folded = slices.Clip(s.folded)
	c.retained = slices.Clip(s.retained)
	c.unran = slices.Clip(s.unran)
	return &c
}

// Head returns the seq of the last applied record, or 0.
func (s *State) Head() uint64 { return s.prog.Seq }

// Status returns the session status.
func (s *State) Status() protocol.Status { return s.prog.Status }

// Turn returns the current turn, if any.
func (s *State) Turn() (Turn, bool) {
	t := s.turn
	t.InputIDs = slices.Clone(t.InputIDs)
	return t, t.ID != ""
}

// Queue returns the admitted inputs that no turn has taken, oldest first.
func (s *State) Queue() []protocol.InputAdmitted {
	out := make([]protocol.InputAdmitted, len(s.prog.Queued))
	for i, id := range s.prog.Queued {
		out[i] = cloneInput(s.inputs[id].event)
	}
	return out
}

func cloneInput(in protocol.InputAdmitted) protocol.InputAdmitted {
	in.Parts = cloneParts(in.Parts)
	return in
}

// Input returns an admitted input and the seq of its input.admitted record.
func (s *State) Input(id string) (protocol.InputAdmitted, uint64, bool) {
	in, ok := s.inputs[id]
	return cloneInput(in.event), in.seq, ok
}

// InputFate returns the turn that took input id, and whether the input was withdrawn. Both are
// zero while the input is queued, and for an unknown input.
func (s *State) InputFate(id string) (turnID string, withdrawn bool) {
	in := s.inputs[id]
	return in.turnID, in.state == inputWithdrawn
}

// Requests returns the open requests, oldest first.
func (s *State) Requests() []protocol.RequestOpened {
	out := make([]protocol.RequestOpened, len(s.prog.Requests))
	for i, id := range s.prog.Requests {
		out[i] = s.requests[id].RequestOpened
		out[i].Payload = slices.Clone(out[i].Payload)
	}
	return out
}

// Resolution returns the record that closed request id, if it ever opened and closed.
func (s *State) Resolution(id string) (protocol.RequestResolved, bool) {
	r, ok := s.resolved[id]
	r.Answer = slices.Clone(r.Answer)
	return r, ok
}

// Asked returns the payload of request id as it opened, whether the request is open or closed.
func (s *State) Asked(id string) (json.RawMessage, bool) {
	p, ok := s.asked[id]
	return slices.Clone(p), ok
}

// OpenToolCalls returns the tool calls with no result, oldest first.
func (s *State) OpenToolCalls() []OpenToolCall { return slices.Clone(s.calls) }

// Goal returns the session goal, if one was set.
func (s *State) Goal() (Goal, bool) {
	g := s.prog.Goal
	if g == nil {
		return Goal{}, false
	}
	return Goal{Condition: g.Condition, MaxTurns: g.MaxTurns, State: g.State, Reason: g.Reason, Turns: g.Turns,
		Evaluated: s.evaluated, Pauses: g.Pauses, RetryAt: g.RetryAt}, true
}

func (s *State) goalState() protocol.GoalState {
	if s.prog.Goal == nil {
		return ""
	}
	return s.prog.Goal.State
}

// LastEnded returns the turn.ended record of the newest ended turn.
func (s *State) LastEnded() protocol.TurnEnded { return s.lastEnded }

// Model returns the session model.
func (s *State) Model() string { return s.model }

// Settings returns the session settings other than the model.
func (s *State) Settings() protocol.Settings { return s.settings }

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
// turn, which holds the queue), a goal that is active or paused (not blocked), a command
// that no owner finished, or a child that has not settled.
func (s *State) Resumable() bool {
	held := s.lastEnded.Cause == protocol.CauseProviderExhausted
	return s.turn.ID != "" || (len(s.prog.Queued) > 0 && !held) || s.goalState() == protocol.GoalActive || s.goalState() == protocol.GoalPaused ||
		len(s.Unfinished()) > 0 || len(s.Unsettled()) > 0
}

// Children returns every spawned child, settled or not, sorted.
func (s *State) Children() []string { return slices.Sorted(maps.Keys(s.children)) }

// Usage returns the token usage summed over every recorded model call and
// every summary call. A goal evaluator call records none.
func (s *State) Usage() protocol.Usage { return s.usage }

// Context returns the newest context measurement, with no usage.
func (s *State) Context() protocol.ContextMeasured { return s.context }

// Measured reports whether the newest reading was recorded in the newest turn.
func (s *State) Measured() bool {
	return s.context != (protocol.ContextMeasured{}) && s.measuredTurn == s.turnN
}

// SubscriptionUsage returns the newest snapshot with the summed cost, or nil.
// A cost with no snapshot gets an empty snapshot of the claude lane, with no capture time.
func (s *State) SubscriptionUsage() *protocol.SubscriptionUsage {
	if s.cost == nil {
		return s.subscribed
	}
	var u protocol.SubscriptionUsage
	if s.subscribed != nil {
		u = *s.subscribed
	} else {
		u = protocol.SubscriptionUsage{Provider: "claude", Windows: []protocol.SubscriptionUsageWindow{}}
	}
	cost := *s.cost
	u.SessionCostUSD = &cost
	return &u
}

// CreatedAt returns the time of the record that created the session.
func (s *State) CreatedAt() time.Time { return s.createdAt }

// BannerAt returns the number of messages of ModelHistory that the engine
// banner follows: the first message of the session, or the whole history that
// the newest compaction left.
func (s *State) BannerAt() int {
	if s.compactedAt != 0 {
		return s.compactedLen
	}
	return 1
}

// CompactionCount returns the number of compactions, by the harness or by a backend.
func (s *State) CompactionCount() int { return s.compacted }

// Compaction returns the newest compaction. Replay of history starts there.
func (s *State) Compaction() (protocol.CompactionApplied, bool) {
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
	c.Head, c.Chunks = slices.Clone(c.Head), slices.Clone(c.Chunks)
	return c, ok
}

// Retained returns the retained tool results in the order of their records.
func (s *State) Retained() []protocol.ToolResultRetained { return slices.Clone(s.retained) }

// Renamed is the newest title of a person, or empty.
func (s *State) Renamed() string { return s.renamed }

// Title is the title of a person, else the title of a model, else the title of the first prompt, else empty.
func (s *State) Title() string { return cmp.Or(s.renamed, s.modeled, s.derived) }

// Summary returns the list entry of the session.
func (s *State) Summary() Summary {
	return Summary{
		ParentID:  s.parentID,
		Origin:    s.origin,
		WorkDir:   s.workDir,
		Model:     s.model,
		Title:     s.Title(),
		Status:    s.Status(),
		Goal:      s.goalState(),
		HeadSeq:   s.prog.Seq,
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
	if env.Seq != s.prog.Seq+1 {
		return fmt.Errorf("%w: got %d, want %d", ErrSeq, env.Seq, s.prog.Seq+1)
	}
	next, err := advance(s.prog, env.Seq, env.Event)
	if err != nil {
		return err
	}
	if err := s.step(env); err != nil {
		return err
	}
	s.remember(env)
	s.prog = next
	s.updatedAt = env.Time
	return nil
}

func (s *State) step(env Envelope) error {
	if env.Event == nil {
		return fmt.Errorf("%w: nil event", ErrUnknownKind)
	}
	if c, ok := env.Event.(protocol.SessionCreated); ok {
		return s.applyCreated(c, env.Time)
	}
	if !s.created {
		return illegal("%s before session.created", env.Event.Kind())
	}
	switch e := env.Event.(type) {
	case protocol.OwnerAcquired:
		return nil
	case protocol.SettingsChanged:
		return s.applySettings(e)
	case protocol.SessionTitled:
		return s.applyTitled(e)
	case protocol.InputAdmitted:
		return s.applyAdmitted(e, env.Seq)
	case protocol.InputPromoted:
		return s.applyPromoted(e)
	case protocol.InputWithdrawn:
		return s.applyWithdrawn(e)
	case protocol.TurnStarted:
		return s.applyStarted(e)
	case protocol.ItemCompleted:
		return s.applyItem(e)
	case protocol.TurnSuspended:
		return s.applySuspended(e)
	case protocol.TurnResumed:
		return s.applyResumed(e)
	case protocol.TurnEnded:
		return s.applyEnded(e)
	case protocol.RequestOpened:
		return s.applyRequestOpened(e)
	case protocol.RequestResolved:
		return s.applyRequestResolved(e, env.Seq, env.Time)
	case protocol.GoalSet:
		return s.applyGoalSet(e)
	case protocol.GoalEvaluated:
		return s.applyGoalEvaluated(e)
	case protocol.GoalChanged:
		return s.applyGoalChanged(e)
	case protocol.CompactionApplied:
		return s.applyCompaction(e, env.Seq, env.Time)
	case protocol.ChildSpawned:
		return s.applyChildSpawned(e)
	case protocol.ChildSettled:
		return s.applyChildSettled(e)
	case protocol.CommandRecorded:
		return s.applyCommand(e, env.Seq)
	case protocol.ContextMeasured:
		s.applyMeasured(e)
		return nil
	case protocol.BackendState:
		return s.applyBackendState(e)
	case protocol.ToolResultRetained:
		return s.applyRetained(e)
	}
	return fmt.Errorf("%w: %T", ErrUnknownKind, env.Event)
}
