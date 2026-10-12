package eventlog

import (
	"maps"
	"slices"
	"time"

	"github.com/majorcontext/harness/protocol"
)

func (s *State) runningTurn(id string) error {
	if s.turn.ID != id || s.turn.Suspended || id == "" {
		return illegal("turn %s is not running", id)
	}
	return nil
}

func (s *State) applyStarted(e protocol.TurnStarted) error {
	switch {
	case s.turn.ID != "":
		return illegal("turn %s is current", s.turn.ID)
	case e.TurnID == "":
		return illegal("turn.started has an empty turn_id")
	case s.turnIDs[e.TurnID]:
		return illegal("turn %s was used", e.TurnID)
	}
	for _, c := range s.calls {
		if !c.answered {
			return illegal("turn %s starts with open tool call %s", e.TurnID, c.CallID)
		}
	}
	for _, id := range e.InputIDs {
		if err := s.inputIs(id, inputAdmitted); err != nil {
			return err
		}
	}
	for _, id := range e.InputIDs {
		s.takeInput(id, inputPromoted)
	}
	s.turn = Turn{ID: e.TurnID, InputIDs: e.InputIDs}
	s.turnAt, s.turnBy, s.turnItems = len(s.history), ProviderOf(s.model), 0
	s.turnN++
	s.turnIDs[e.TurnID] = true
	return nil
}

func (s *State) applyItem(e protocol.ItemCompleted) error {
	if err := s.runningTurn(e.TurnID); err != nil {
		return err
	}
	switch e.Message.Role {
	case protocol.RoleUser, protocol.RoleAssistant, protocol.RoleTool:
	default:
		return illegal("item %s has role %q", e.ItemID, e.Message.Role)
	}
	calls := slices.Clone(s.calls)
	for _, p := range e.Message.Parts {
		i := slices.IndexFunc(calls, func(c OpenToolCall) bool { return c.CallID == p.CallID })
		switch {
		case p.Type == protocol.LogPartToolCall && p.CallID == "":
			return illegal("item %s has a tool call with no call_id", e.ItemID)
		case p.Type == protocol.LogPartToolCall && i >= 0:
			return illegal("tool call %s is open", p.CallID)
		case p.Type == protocol.LogPartToolCall:
			calls = append(calls, OpenToolCall{CallID: p.CallID, ItemID: e.ItemID, Name: p.Name})
		case p.Type == protocol.LogPartToolResult && i < 0:
			return illegal("no open tool call %s", p.CallID)
		case p.Type == protocol.LogPartToolResult:
			if j := slices.IndexFunc(p.Blobs, func(b protocol.LogPart) bool { return b.Type != protocol.LogPartBlob || b.BlobKey == "" }); j >= 0 {
				return illegal("tool result %s has a blob %d that is not a blob part with a blob_key", p.CallID, j)
			}
			calls = slices.Delete(calls, i, i+1)
		case p.Type != protocol.LogPartText && p.Type != protocol.LogPartReasoning:
			return illegal("item %s has part type %q", e.ItemID, p.Type)
		}
	}
	s.calls = calls
	s.turnItems++
	return nil
}

func (s *State) unanswered() error {
	for _, c := range s.calls {
		if s.openRequest(c.CallID) < 0 {
			return illegal("tool call %s has no result", c.CallID)
		}
	}
	return nil
}

func (s *State) applySuspended(e protocol.TurnSuspended) error {
	if err := s.runningTurn(e.TurnID); err != nil {
		return err
	}
	if e.Cause != protocol.CauseHandoff {
		return illegal("turn %s suspended with cause %s", e.TurnID, e.Cause)
	}
	if len(s.calls) > 0 {
		return illegal("turn %s suspends with open tool call %s", e.TurnID, s.calls[0].CallID)
	}
	s.turn.Suspended = true
	return nil
}

func (s *State) applyResumed(e protocol.TurnResumed) error {
	if s.turn.ID != e.TurnID || !s.turn.Suspended {
		return illegal("turn %s is not suspended", e.TurnID)
	}
	if e.Count != s.turn.Resumes+1 {
		return illegal("turn %s resume count %d, want %d", e.TurnID, e.Count, s.turn.Resumes+1)
	}
	s.turn.Suspended = false
	s.turn.Resumes = e.Count
	return nil
}

func (s *State) applyEnded(e protocol.TurnEnded) error {
	if s.turn.ID == e.TurnID && s.turn.Suspended {
		return illegal("turn %s is suspended", e.TurnID)
	}
	if err := s.runningTurn(e.TurnID); err != nil {
		return err
	}
	switch e.StopReason {
	case protocol.StopCompleted:
		if e.Cause != "" {
			return illegal("turn %s completes with cause %q", e.TurnID, e.Cause)
		}
	case protocol.StopFailed:
		if e.Cause != "" && e.Cause != protocol.CauseProviderExhausted {
			return illegal("turn %s has failure cause %q", e.TurnID, e.Cause)
		}
	case protocol.StopInterrupted:
		if c := e.Cause; c != protocol.CauseStopped && c != protocol.CauseEnded && c != protocol.CauseGoalCleared && c != protocol.CauseCrashed {
			return illegal("turn %s has interrupt cause %q", e.TurnID, e.Cause)
		}
	case protocol.StopAwaitingInput:
		if e.Cause != "" {
			return illegal("turn %s awaits input with cause %q", e.TurnID, e.Cause)
		}
		if !slices.ContainsFunc(s.requests, func(r pendingRequest) bool { return r.turnID == e.TurnID }) {
			return illegal("turn %s awaits input with no open request from this turn", e.TurnID)
		}
	default:
		return illegal("turn %s has stop reason %q", e.TurnID, e.StopReason)
	}
	if err := s.unanswered(); err != nil {
		return err
	}
	own := s.turnItems
	if e.Cause == protocol.CauseCrashed {
		own--
	}
	if own <= 0 {
		s.unran = append(s.unran, s.turnN)
	}
	s.turn = Turn{}
	s.lastEnded = e
	return nil
}

func (s *State) applyRequestOpened(e protocol.RequestOpened) error {
	if s.turn.ID == "" || s.turn.Suspended {
		return illegal("request %s opened with no running turn", e.RequestID)
	}
	if e.RequestID == "" || e.ItemID == "" || s.openRequest(e.RequestID) >= 0 {
		return illegal("request %q on item %q is already open or unnamed", e.RequestID, e.ItemID)
	}
	if !slices.ContainsFunc(s.calls, func(c OpenToolCall) bool { return c.CallID == e.RequestID && c.ItemID == e.ItemID }) {
		return illegal("no open tool call %s on item %s", e.RequestID, e.ItemID)
	}
	s.requests = append(s.requests, pendingRequest{e, s.turn.ID})
	return nil
}

func (s *State) applyRequestResolved(e protocol.RequestResolved, seq uint64, at time.Time) error {
	i := s.openRequest(e.RequestID)
	if i < 0 {
		return illegal("request %s is not open", e.RequestID)
	}
	if e.Resolution != protocol.ResolutionAnswered && e.Resolution != protocol.ResolutionDismissed {
		return illegal("request %s has resolution %q", e.RequestID, e.Resolution)
	}
	calls := slices.Clone(s.calls)
	if k := slices.IndexFunc(calls, func(c OpenToolCall) bool { return c.CallID == e.RequestID }); k >= 0 {
		if e.Resolution == protocol.ResolutionAnswered {
			calls[k].answered = true
		} else {
			s.say(seq, at, "msg_resolved_"+calls[k].CallID, dismissal(calls[k]))
			calls = slices.Delete(calls, k, k+1)
		}
	}
	s.calls = calls
	s.requests = slices.Delete(s.requests, i, i+1)
	s.resolved = maps.Clone(s.resolved)
	if s.resolved == nil {
		s.resolved = map[string]protocol.RequestResolved{}
	}
	s.resolved[e.RequestID] = e
	return nil
}

func (s *State) openRequest(id string) int {
	return slices.IndexFunc(s.requests, func(r pendingRequest) bool { return r.RequestID == id })
}
