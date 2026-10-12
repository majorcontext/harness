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
	s.evaluated = s.lastEnded.TurnID
	return nil
}

func (s *State) applyGoalEvaluated(e protocol.GoalEvaluated) error {
	switch {
	case s.goalState() != protocol.GoalActive:
		return illegal("no active goal to evaluate")
	case s.lastEnded.TurnID != e.TurnID || s.turn.ID != "":
		return illegal("turn %s is not the last ended turn", e.TurnID)
	case s.evaluated == e.TurnID:
		return illegal("turn %s was evaluated", e.TurnID)
	case s.prog.Goal.MaxTurns > 0 && s.prog.Goal.Turns >= s.prog.Goal.MaxTurns:
		return illegal("goal max_turns %d is reached", s.prog.Goal.MaxTurns)
	case e.Verdict != protocol.VerdictMet && e.Verdict != protocol.VerdictNotMet && e.Verdict != protocol.VerdictImpossible:
		return illegal("goal verdict %q", e.Verdict)
	}
	s.evaluated = e.TurnID
	return nil
}

func (s *State) applyGoalChanged(e protocol.GoalChanged) error {
	if !slices.Contains(goalFrom[e.State], s.goalState()) {
		return illegal("goal %s cannot become %s", s.goalState(), e.State)
	}
	return nil
}
