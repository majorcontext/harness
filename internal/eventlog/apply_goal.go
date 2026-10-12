package eventlog

import (
	"slices"

	"github.com/majorcontext/harness/protocol"
)

var goalFrom = map[protocol.GoalState][]protocol.GoalState{
	protocol.GoalActive:    {protocol.GoalPaused},
	protocol.GoalPaused:    {protocol.GoalActive},
	protocol.GoalAchieved:  {protocol.GoalActive},
	protocol.GoalFailed:    {protocol.GoalActive},
	protocol.GoalExhausted: {protocol.GoalActive},
	protocol.GoalCleared:   {protocol.GoalActive, protocol.GoalPaused, protocol.GoalAchieved, protocol.GoalFailed, protocol.GoalExhausted},
}

func (s *State) applyGoalSet(e protocol.GoalSet) error {
	if e.Condition == "" || e.MaxTurns < 0 || e.Turns < 0 || e.MaxTurns > 0 && e.Turns >= e.MaxTurns {
		return illegal("goal.set needs a condition, max_turns >= 0, and turns below max_turns")
	}
	s.goal = Goal{Condition: e.Condition, MaxTurns: e.MaxTurns, Turns: e.Turns, State: protocol.GoalActive, Evaluated: s.lastEnded.TurnID}
	return nil
}

func (s *State) applyGoalEvaluated(e protocol.GoalEvaluated) error {
	switch {
	case s.goal.State != protocol.GoalActive:
		return illegal("no active goal to evaluate")
	case s.lastEnded.TurnID != e.TurnID || s.turn.ID != "":
		return illegal("turn %s is not the last ended turn", e.TurnID)
	case s.goal.Evaluated == e.TurnID:
		return illegal("turn %s was evaluated", e.TurnID)
	case s.goal.MaxTurns > 0 && s.goal.Turns >= s.goal.MaxTurns:
		return illegal("goal max_turns %d is reached", s.goal.MaxTurns)
	case e.Verdict != protocol.VerdictMet && e.Verdict != protocol.VerdictNotMet && e.Verdict != protocol.VerdictImpossible:
		return illegal("goal verdict %q", e.Verdict)
	}
	s.goal.Turns++
	s.goal.Evaluated, s.goal.Pauses = e.TurnID, 0
	return nil
}

func (s *State) applyGoalChanged(e protocol.GoalChanged) error {
	if !slices.Contains(goalFrom[e.State], s.goal.State) {
		return illegal("goal %s cannot become %s", s.goal.State, e.State)
	}
	if e.State == protocol.GoalPaused {
		s.goal.Pauses++
	}
	s.goal.State, s.goal.Reason, s.goal.RetryAt = e.State, e.Reason, e.RetryAt
	return nil
}
