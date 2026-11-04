package eventlog

import (
	"time"

	"github.com/majorcontext/harness/protocol"
)

type checkpointState struct {
	Created                                 bool
	ParentID, Agent, Origin, WorkDir, Model string
	Renamed, Modeled, Derived               string
	Settings                                protocol.Settings
	Allowed                                 []string
	AllowedSet                              bool
	MaxSteps                                int
	CreatedAt, UpdatedAt                    time.Time
	Inputs                                  map[string]checkpointInput
	Prog                                    protocol.Progress
	Turn                                    Turn
	TurnIDs                                 map[string]bool
	LastEnded                               protocol.TurnEnded
	Calls                                   []checkpointCall
	Requests                                map[string]checkpointRequest
	ProgressQueuedSet, ProgressRequestsSet  bool
	TurnInputsSet                           bool
	Evaluated                               string
	Usage                                   protocol.Usage
	Context                                 protocol.ContextMeasured
	MeasuredTurn                            int
	ReadAt                                  time.Time
	ReadTTL                                 time.Duration
	Subscribed                              *protocol.SubscriptionUsage
	SubscriptionWindowsSet                  bool
	SubscriptionCostSet                     bool
	Cost                                    *float64
	CostSet                                 bool
	Compaction                              protocol.CompactionApplied
	CompactedAt                             uint64
	CompactedTime                           time.Time
	Compacted, CompactedLen                 int
	Children                                map[string]protocol.Outcome
	Backends                                map[string]BackendChain
	Retained                                []protocol.ToolResultRetained
	Commands                                map[string]checkpointCommand
	History                                 []checkpointEntry
	Stranded                                []checkpointEntry
	TurnAt                                  int
	TurnBy                                  string
	TurnN, TurnItems                        int
	Unran                                   []int
}

type checkpointInput struct {
	State    inputState
	Seq      uint64
	Event    protocol.InputAdmitted
	PartsSet bool
	TurnID   string
}
type checkpointCall struct {
	CallID, ItemID, Name string
	Answered             bool
}
type checkpointRequest struct {
	Request protocol.RequestOpened
	TurnID  string
}
type checkpointCommand struct {
	Rec   protocol.CommandRecorded
	Seq   uint64
	After string
}
type checkpointProvenance struct{ Source, ID, Label string }
type checkpointEntry struct {
	Seq                         uint64
	Sources                     []uint64
	At                          time.Time
	ID                          string
	Msg                         protocol.LogMessage
	By                          string
	Turn                        int
	Promoted                    []protocol.InputAdmitted
	PromotedSet                 bool
	Last                        uint64
	From                        checkpointProvenance
	Pinned, ReaderOnly, Carried bool
}
type checkpointLightEntry struct {
	Entry   checkpointEntry
	Summary uint64
	At      time.Time
}
type checkpointSnapshot struct {
	State       checkpointState
	Folded      []checkpointLightEntry
	Attachments map[string]attachmentInfo
	Commands    map[string]uint64
}

func mapValues[A, B any](in map[string]A, f func(A) B) map[string]B {
	if in == nil {
		return nil
	}
	out := make(map[string]B, len(in))
	for k, v := range in {
		out[k] = f(v)
	}
	return out
}
func mapSlice[A, B any](in []A, f func(A) B) []B {
	if in == nil {
		return nil
	}
	out := make([]B, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}
func saveEntry(v entry) checkpointEntry {
	return checkpointEntry{v.seq, v.sources, v.at, v.id, v.msg, v.by, v.turn, v.promoted, v.promoted != nil, v.last, checkpointProvenance{v.from.source, v.from.id, v.from.label}, v.pinned, v.readerOnly, v.carried}
}
func loadEntry(v checkpointEntry) entry {
	if v.PromotedSet && v.Promoted == nil {
		v.Promoted = []protocol.InputAdmitted{}
	}
	return entry{seq: v.Seq, sources: v.Sources, at: v.At, id: v.ID, msg: v.Msg, by: v.By, turn: v.Turn, promoted: v.Promoted, last: v.Last, from: provenance{v.From.Source, v.From.ID, v.From.Label}, pinned: v.Pinned, readerOnly: v.ReaderOnly, carried: v.Carried}
}
func saveState(s State) checkpointState {
	return checkpointState{
		Created: s.created, ParentID: s.parentID, Agent: s.agent, Origin: s.origin, WorkDir: s.workDir, Model: s.model,
		Renamed: s.renamed, Modeled: s.modeled, Derived: s.derived, Settings: s.settings, Allowed: s.allowed, AllowedSet: s.allowed != nil, MaxSteps: s.maxSteps,
		CreatedAt: s.createdAt, UpdatedAt: s.updatedAt,
		Inputs: mapValues(s.inputs, func(v input) checkpointInput {
			return checkpointInput{v.state, v.seq, v.event, v.event.Parts != nil, v.turnID}
		}),
		Prog: s.prog, Turn: s.turn, TurnIDs: s.turnIDs, LastEnded: s.lastEnded,
		Calls:             mapSlice(s.calls, func(v OpenToolCall) checkpointCall { return checkpointCall{v.CallID, v.ItemID, v.Name, v.answered} }),
		Requests:          mapValues(s.requests, func(v pendingRequest) checkpointRequest { return checkpointRequest{v.RequestOpened, v.turnID} }),
		ProgressQueuedSet: s.prog.Queued != nil, ProgressRequestsSet: s.prog.Requests != nil, TurnInputsSet: s.turn.InputIDs != nil,
		Evaluated: s.evaluated, Usage: s.usage, Context: s.context,
		MeasuredTurn: s.measuredTurn, ReadAt: s.readAt, ReadTTL: s.readTTL, Subscribed: s.subscribed, Cost: s.cost, CostSet: s.cost != nil,
		SubscriptionWindowsSet: s.subscribed != nil && s.subscribed.Windows != nil,
		SubscriptionCostSet:    s.subscribed != nil && s.subscribed.SessionCostUSD != nil,
		Compaction:             s.compaction, CompactedAt: s.compactedAt, CompactedTime: s.compactedTime, Compacted: s.compacted, CompactedLen: s.compactedLen,
		Children: s.children, Backends: s.backends, Retained: s.retained,
		Commands: mapValues(s.commands, func(v command) checkpointCommand { return checkpointCommand{v.rec, v.seq, v.after} }),
		History:  mapSlice(s.history, saveEntry), Stranded: mapSlice(s.stranded, saveEntry),
		TurnAt: s.turnAt, TurnBy: s.turnBy, TurnN: s.turnN, TurnItems: s.turnItems, Unran: s.unran,
	}
}
func loadState(s checkpointState) State {
	if s.AllowedSet && s.Allowed == nil {
		s.Allowed = []string{}
	}
	if s.ProgressQueuedSet && s.Prog.Queued == nil {
		s.Prog.Queued = []string{}
	}
	if s.ProgressRequestsSet && s.Prog.Requests == nil {
		s.Prog.Requests = []string{}
	}
	if s.TurnInputsSet && s.Turn.InputIDs == nil {
		s.Turn.InputIDs = []string{}
	}
	if s.CostSet && s.Cost == nil {
		zero := 0.0
		s.Cost = &zero
	}
	if s.Subscribed != nil {
		if s.SubscriptionWindowsSet && s.Subscribed.Windows == nil {
			s.Subscribed.Windows = []protocol.SubscriptionUsageWindow{}
		}
		if s.SubscriptionCostSet && s.Subscribed.SessionCostUSD == nil {
			zero := 0.0
			s.Subscribed.SessionCostUSD = &zero
		}
	}
	return State{
		created: s.Created, parentID: s.ParentID, agent: s.Agent, origin: s.Origin, workDir: s.WorkDir, model: s.Model,
		renamed: s.Renamed, modeled: s.Modeled, derived: s.Derived, settings: s.Settings, allowed: s.Allowed, maxSteps: s.MaxSteps,
		createdAt: s.CreatedAt, updatedAt: s.UpdatedAt,
		inputs: mapValues(s.Inputs, func(v checkpointInput) input {
			if v.PartsSet && v.Event.Parts == nil {
				v.Event.Parts = []protocol.LogPart{}
			}
			return input{v.State, v.Seq, v.Event, v.TurnID}
		}),
		prog: s.Prog, turn: s.Turn, turnIDs: s.TurnIDs, lastEnded: s.LastEnded,
		calls:     mapSlice(s.Calls, func(v checkpointCall) OpenToolCall { return OpenToolCall{v.CallID, v.ItemID, v.Name, v.Answered} }),
		requests:  mapValues(s.Requests, func(v checkpointRequest) pendingRequest { return pendingRequest{v.Request, v.TurnID} }),
		evaluated: s.Evaluated, usage: s.Usage, context: s.Context,
		measuredTurn: s.MeasuredTurn, readAt: s.ReadAt, readTTL: s.ReadTTL, subscribed: s.Subscribed, cost: s.Cost,
		compaction: s.Compaction, compactedAt: s.CompactedAt, compactedTime: s.CompactedTime, compacted: s.Compacted, compactedLen: s.CompactedLen,
		children: s.Children, backends: s.Backends, retained: s.Retained,
		commands: mapValues(s.Commands, func(v checkpointCommand) command { return command{v.Rec, v.Seq, v.After} }),
		history:  mapSlice(s.History, loadEntry), stranded: mapSlice(s.Stranded, loadEntry),
		turnAt: s.TurnAt, turnBy: s.TurnBy, turnN: s.TurnN, turnItems: s.TurnItems, unran: s.Unran,
	}
}
func (l *LightState) snapshot() checkpointSnapshot {
	return checkpointSnapshot{saveState(l.state), mapSlice(l.folded, func(v lightEntry) checkpointLightEntry {
		return checkpointLightEntry{saveEntry(v.Entry), v.Summary, v.At}
	}), l.attachments, l.commands}
}
func (s checkpointSnapshot) lightState() *LightState {
	l := &LightState{state: loadState(s.State), folded: mapSlice(s.Folded, func(v checkpointLightEntry) lightEntry { return lightEntry{loadEntry(v.Entry), v.Summary, v.At} }), attachments: s.Attachments, commands: s.Commands}
	l.state.light = l
	return l
}
