package eventlog

import (
	"fmt"
	"slices"

	"github.com/majorcontext/harness/protocol"
)

// Advance applies the durable event e to p and returns the new Progress. e must
// follow p: its seq is p.Seq+1. Advance trusts the log to hold legal
// transitions, which Check enforces before an append, so a reader that holds
// only a Progress folds the same value as State does. It never changes p.
func Advance(p protocol.Progress, e protocol.Event) (protocol.Progress, error) {
	ev, err := payloadValue(e)
	if err != nil {
		return p, err
	}
	return advance(p, e.Seq, ev)
}

func advance(p protocol.Progress, seq uint64, ev protocol.Payload) (protocol.Progress, error) {
	if seq != p.Seq+1 {
		return p, fmt.Errorf("%w: got %d, want %d", ErrSeq, seq, p.Seq+1)
	}
	p.Seq = seq
	switch e := ev.(type) {
	case protocol.InputAdmitted:
		p.Queued = append(slices.Clip(p.Queued), e.InputID)
	case protocol.InputPromoted:
		p.Queued = without(p.Queued, e.InputID)
	case protocol.InputWithdrawn:
		p.Queued = without(p.Queued, e.InputID)
	case protocol.TurnStarted:
		p.Queued = without(p.Queued, e.InputIDs...)
		p.TurnID = e.TurnID
	case protocol.TurnSuspended:
		p.TurnID = ""
	case protocol.TurnResumed:
		p.TurnID = e.TurnID
	case protocol.TurnEnded:
		p.TurnID = ""
		p.LastTurn = &protocol.LastTurn{TurnID: e.TurnID, StopReason: e.StopReason, Cause: e.Cause, Error: e.Error}
	case protocol.RequestOpened:
		p.Requests = append(slices.Clip(p.Requests), e.RequestID)
	case protocol.RequestResolved:
		p.Requests = without(p.Requests, e.RequestID)
	case protocol.GoalSet:
		p.Goal = &protocol.GoalView{Goal: protocol.Goal{Condition: e.Condition, MaxTurns: e.MaxTurns}, State: protocol.GoalActive, Turns: e.Turns}
	case protocol.GoalEvaluated:
		if p.Goal != nil {
			g := *p.Goal
			g.Turns++
			g.Pauses = 0
			p.Goal = &g
		}
	case protocol.GoalChanged:
		if p.Goal != nil {
			g := *p.Goal
			g.State, g.Reason, g.RetryAt = e.State, e.Reason, e.RetryAt
			if e.State == protocol.GoalPaused {
				g.Pauses++
			}
			p.Goal = &g
		}
	}
	p.Status = statusOf(p)
	return p, nil
}

// statusOf derives the status of p: a running turn, else waiting while a turn
// that awaited input has an open request, else idle.
func statusOf(p protocol.Progress) protocol.Status {
	switch {
	case p.TurnID != "":
		return protocol.StatusRunning
	case p.LastTurn != nil && p.LastTurn.StopReason == protocol.StopAwaitingInput && len(p.Requests) > 0:
		return protocol.StatusWaiting
	}
	return protocol.StatusIdle
}

// without returns ids with each of drop removed, in a new slice when it removes any.
func without(ids []string, drop ...string) []string {
	if !slices.ContainsFunc(ids, func(id string) bool { return slices.Contains(drop, id) }) {
		return ids
	}
	out := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return slices.Contains(drop, id) })
	if len(out) == 0 {
		return nil
	}
	return out
}

// Progress returns the status, turn, queue, open requests, goal, and last turn
// of s at its head. The caller owns the result.
func (s *State) Progress() protocol.Progress {
	p := s.prog
	p.Queued, p.Requests = slices.Clone(p.Queued), slices.Clone(p.Requests)
	if p.Goal != nil {
		g := *p.Goal
		p.Goal = &g
	}
	if p.LastTurn != nil {
		l := *p.LastTurn
		p.LastTurn = &l
	}
	return p
}
