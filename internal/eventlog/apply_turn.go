package eventlog

import "slices"

func (s *State) runningTurn(id string) error {
	if s.turn.ID != id || s.turn.Suspended || id == "" {
		return illegal("turn %s is not running", id)
	}
	return nil
}

func (s *State) applyStarted(e TurnStarted) error {
	switch {
	case s.turn.ID != "":
		return illegal("turn %s is current", s.turn.ID)
	case e.TurnID == "":
		return illegal("turn.started has an empty turn_id")
	case s.turnIDs[e.TurnID]:
		return illegal("turn %s was used", e.TurnID)
	}
	for _, id := range e.InputIDs {
		if err := s.inputIs(id, inputAdmitted); err != nil {
			return err
		}
	}
	s.turn = Turn{ID: e.TurnID, InputIDs: e.InputIDs}
	s.turnIDs[e.TurnID] = true
	return nil
}

func (s *State) applyItem(e ItemCompleted) error {
	if err := s.runningTurn(e.TurnID); err != nil {
		return err
	}
	switch e.Message.Role {
	case RoleUser, RoleAssistant, RoleTool:
	default:
		return illegal("item %s has role %q", e.ItemID, e.Message.Role)
	}
	calls := slices.Clone(s.calls)
	for _, p := range e.Message.Parts {
		i := slices.IndexFunc(calls, func(c OpenToolCall) bool { return c.CallID == p.CallID })
		switch {
		case p.Type == PartToolCall && p.CallID == "":
			return illegal("item %s has a tool call with no call_id", e.ItemID)
		case p.Type == PartToolCall && i >= 0:
			return illegal("tool call %s is open", p.CallID)
		case p.Type == PartToolCall:
			calls = append(calls, OpenToolCall{CallID: p.CallID, ItemID: e.ItemID, Name: p.Name})
		case p.Type == PartToolResult && i < 0:
			return illegal("no open tool call %s", p.CallID)
		case p.Type == PartToolResult:
			calls = slices.Delete(calls, i, i+1)
		case p.Type != PartText && p.Type != PartReasoning:
			return illegal("item %s has part type %q", e.ItemID, p.Type)
		}
	}
	s.calls = calls
	return nil
}

func (s *State) unanswered() error {
	for _, c := range s.calls {
		if !slices.ContainsFunc(s.requests, func(r RequestOpened) bool { return r.ItemID == c.ItemID }) {
			return illegal("tool call %s has no result", c.CallID)
		}
	}
	return nil
}

func (s *State) applySuspended(e TurnSuspended) error {
	if err := s.runningTurn(e.TurnID); err != nil {
		return err
	}
	if e.Cause != CauseHandoff {
		return illegal("turn %s suspended with cause %s", e.TurnID, e.Cause)
	}
	if len(s.calls) > 0 {
		return illegal("turn %s suspends with open tool call %s", e.TurnID, s.calls[0].CallID)
	}
	s.turn.Suspended = true
	return nil
}

func (s *State) applyResumed(e TurnResumed) error {
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

func (s *State) applyEnded(e TurnEnded) error {
	if s.turn.ID == e.TurnID && s.turn.Suspended {
		return illegal("turn %s is suspended", e.TurnID)
	}
	if err := s.runningTurn(e.TurnID); err != nil {
		return err
	}
	switch e.StopReason {
	case StopCompleted, StopFailed:
	case StopInterrupted:
		if c := Cause(e.Error); c != CauseStopped && c != CauseGoalCleared && c != CauseCrashed {
			return illegal("turn %s has interrupt cause %q", e.TurnID, e.Error)
		}
	case StopAwaitingInput:
		if len(s.requests) == 0 {
			return illegal("turn %s awaits input with no open request", e.TurnID)
		}
	default:
		return illegal("turn %s has stop reason %q", e.TurnID, e.StopReason)
	}
	if err := s.unanswered(); err != nil {
		return err
	}
	s.turn = Turn{}
	s.lastEnded = e
	s.usage.InputTokens += e.Usage.InputTokens
	s.usage.OutputTokens += e.Usage.OutputTokens
	s.usage.CacheReadTokens += e.Usage.CacheReadTokens
	s.usage.CacheWriteTokens += e.Usage.CacheWriteTokens
	return nil
}

func (s *State) applyRequestOpened(e RequestOpened) error {
	if s.turn.ID == "" || s.turn.Suspended {
		return illegal("request %s opened with no running turn", e.RequestID)
	}
	if e.RequestID == "" || e.ItemID == "" || s.openRequest(e.RequestID) >= 0 {
		return illegal("request %q on item %q is already open or unnamed", e.RequestID, e.ItemID)
	}
	s.requests = append(s.requests, e)
	return nil
}

func (s *State) applyRequestResolved(e RequestResolved) error {
	i := s.openRequest(e.RequestID)
	if i < 0 {
		return illegal("request %s is not open", e.RequestID)
	}
	if e.Resolution != ResolutionAnswered && e.Resolution != ResolutionDismissed {
		return illegal("request %s has resolution %q", e.RequestID, e.Resolution)
	}
	s.requests = slices.Delete(s.requests, i, i+1)
	return nil
}

func (s *State) openRequest(id string) int {
	return slices.IndexFunc(s.requests, func(r RequestOpened) bool { return r.RequestID == id })
}
