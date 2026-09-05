// Ambient parked-goal status segment. Structurally mirrors
// engine/mcp_status.go's mcpStatusSegment and engine/process.go's
// processStatusSegment (see either's doc comment): computed fresh from live
// Session state on every streamTurn call, appended only to the newest user
// message via the shared withAmbientStatus, and never persisted to the
// session log. It does not survive a process restart.
package engine

import "fmt"

// goalParkedSegment renders the ambient status block request assembly
// appends to the newest user message (see streamTurn) while a worker-turn
// exhaustion has left the session's goal parked. The goal stays active after
// worker retry exhaustion.
//
// Renders "" — absent, the zero happy-path cost the other two ambient
// segments already commit to — in every one of these cases:
//   - s.goalParked is false: the overwhelming common case, and in
//     particular the state for every request PursueGoal's own worker turns
//     make, since clearGoalParkedAtEntry resets the flag before that loop's
//     very first turn of any run (fresh or resumed) — this segment never
//     describes a park to the very loop that would resume it, structurally,
//     not by convention.
//
// When present, the text is deliberately CLASSIFIED
// (s.goalParkedReason, produced by classifyGoalWorkerError at park time) —
// never the raw provider error the sibling goal.parked record/event also
// avoid leaking (see recordGoalParked) — so the model gets an actionable,
// vendor-detail-free signal: the goal is still armed and will resume on its
// own; no action is needed from this turn on its behalf.
func goalParkedSegment(s *Session) string {
	s.mu.Lock()
	parked, reason, attempts := s.goalParked, s.goalParkedReason, s.goalParkedAttempts
	s.mu.Unlock()
	if !parked {
		return ""
	}
	word := "attempts"
	if attempts == 1 {
		word = "attempt"
	}
	return fmt.Sprintf("[goal: parked after %d failed worker %s (%s). It resumes automatically when this turn completes.]", attempts, word, reason)
}
