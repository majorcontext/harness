package eventlog

import "slices"

var goalFrom = map[GoalState][]GoalState{
	GoalActive:    {GoalPaused},
	GoalPaused:    {GoalActive},
	GoalAchieved:  {GoalActive},
	GoalFailed:    {GoalActive},
	GoalExhausted: {GoalActive},
	GoalCleared:   {GoalActive, GoalPaused, GoalAchieved, GoalFailed, GoalExhausted},
}

func (s *State) applyGoalSet(e GoalSet) error {
	if e.Condition == "" || e.MaxTurns < 0 {
		return illegal("goal.set needs a condition and max_turns >= 0")
	}
	s.goal = Goal{Condition: e.Condition, MaxTurns: e.MaxTurns, State: GoalActive, Evaluated: s.lastEnded.TurnID}
	return nil
}

func (s *State) applyGoalEvaluated(e GoalEvaluated) error {
	switch {
	case s.goal.State != GoalActive:
		return illegal("no active goal to evaluate")
	case s.lastEnded.TurnID != e.TurnID || s.turn.ID != "":
		return illegal("turn %s is not the last ended turn", e.TurnID)
	case s.goal.Evaluated == e.TurnID:
		return illegal("turn %s was evaluated", e.TurnID)
	case s.goal.MaxTurns > 0 && s.goal.Turns >= s.goal.MaxTurns:
		return illegal("goal max_turns %d is reached", s.goal.MaxTurns)
	case e.Verdict != VerdictMet && e.Verdict != VerdictNotMet && e.Verdict != VerdictImpossible:
		return illegal("goal verdict %q", e.Verdict)
	}
	s.goal.Turns++
	s.goal.Evaluated, s.goal.Pauses = e.TurnID, 0
	return nil
}

func (s *State) applyGoalChanged(e GoalChanged) error {
	if !slices.Contains(goalFrom[e.State], s.goal.State) {
		return illegal("goal %s cannot become %s", s.goal.State, e.State)
	}
	if e.State == GoalPaused {
		s.goal.Pauses++
	}
	s.goal.State, s.goal.Reason, s.goal.RetryAt = e.State, e.Reason, e.RetryAt
	return nil
}
