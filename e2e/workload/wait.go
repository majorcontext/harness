//go:build e2e

package workload

import (
	"context"
	"encoding/json"
	"fmt"
)

// turnEndPayload is the subset of a turn.end event's data this suite reads
// (see server/openapi.yaml's LastTurn.outcome enum).
type turnEndPayload struct {
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// waitForTurnEnd streams a session's events from seq `from` and blocks
// until a turn.end record arrives, returning its outcome. It is the
// event-driven counterpart to polling GetSession — used so a timed phase
// (row 2) or a goal-loop wait (row 3) measures from real completion
// signal, not a poll interval's slop.
func waitForTurnEnd(ctx context.Context, hc *HarnessClient, sessionID string, from int64) (turnEndPayload, error) {
	events, err := hc.Events(ctx, sessionID, from)
	if err != nil {
		return turnEndPayload{}, err
	}
	for ev := range events {
		if ev.Type != "turn.end" {
			continue
		}
		var payload turnEndPayload
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return turnEndPayload{}, fmt.Errorf("parse turn.end payload: %w", err)
		}
		return payload, nil
	}
	if err := ctx.Err(); err != nil {
		return turnEndPayload{}, fmt.Errorf("waiting for turn.end: %w", err)
	}
	return turnEndPayload{}, fmt.Errorf("event stream for session %s ended before a turn.end arrived", sessionID)
}

// waitForGoalOutcome streams events until either a goal.achieved record, a
// turn.end with outcome "max_turns_exceeded" (the goal gave up), or a
// session.error arrives, whichever comes first. It returns a short,
// human-readable description of whichever terminal it saw.
func waitForGoalOutcome(ctx context.Context, hc *HarnessClient, sessionID string, from int64) (string, error) {
	events, err := hc.Events(ctx, sessionID, from)
	if err != nil {
		return "", err
	}
	for ev := range events {
		switch ev.Type {
		case "goal.achieved":
			return "achieved", nil
		case "goal.parked":
			return "parked", nil
		case "session.error":
			return "session.error", nil
		case "turn.end":
			var payload turnEndPayload
			if err := json.Unmarshal(ev.Data, &payload); err == nil && payload.Outcome != "completed" {
				return payload.Outcome, nil
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("waiting for goal outcome: %w", err)
	}
	return "", fmt.Errorf("event stream for session %s ended before a goal outcome arrived", sessionID)
}
