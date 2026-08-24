package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestAmbientGoalParkedStatusAbsentWithNoGoal covers the overwhelming common
// case: no goal has ever been registered, so s.goalParked is false and the
// ambient block must never appear.
func TestAmbientGoalParkedStatusAbsentWithNoGoal(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if last := lastUserText(t, prov.requests[0]); strings.Contains(last, "[goal:") {
		t.Fatalf("last user message = %q, want no ambient parked-goal block", last)
	}
}

// TestAmbientGoalParkedStatusPresentAfterWorkerPark is invariant 6's
// headline test (docs/plans/2026-07-21-goal-worker-park.md): a worker-turn
// exhaustion parks the goal (Task 1) but, before this change, left no
// in-band signal at all for a model prompted mid-outage — the same "nothing
// to go on" gap TestAmbientMCPStatusPresentWhenDegraded closed for degraded
// MCP servers. Red-verified: with goalParkedSegment (and its call site in
// streamTurn) absent, this test fails because the request carries no
// "[goal:" block at all.
func TestAmbientGoalParkedStatusPresentAfterWorkerPark(t *testing.T) {
	dir := t.TempDir()
	var s *Session
	var prov *goalProvider
	var parkErr error
	synctest.Test(t, func(t *testing.T) {
		prov = &goalProvider{
			// Exhausts the deterministic tier in exactly goalWorkerRetries+1
			// attempts (3, matching the plan's own worked example), then
			// recovers so the later plain Prompt call below can succeed.
			workerErrN: goalWorkerRetries + 1,
			workerErr:  errors.New("provider: connection reset by peer, endpoint https://mcp.example.com/v1?token=SECRET"),
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
			},
		}
		s = goalSession(t, prov, dir)
		_, parkErr = s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if !IsGoalWorkerParked(parkErr) {
			t.Fatalf("PursueGoal err = %v, want IsGoalWorkerParked", parkErr)
		}

		// Stands in for the queued prompt that dispatches as a normal turn
		// the instant the exit-park frees the run slot (Task 2, server
		// side) — from the engine's point of view, just an ordinary Prompt
		// call on a session whose goal happens to be parked.
		if _, err := s.Prompt(context.Background(), "status?"); err != nil {
			t.Fatal(err)
		}
	})

	// The last recorded request is the plain Prompt call above (worker
	// calls only; goalProvider.requests also records eval calls, but the
	// evaluator is never invoked once the worker exhausts its budget).
	last := lastUserText(t, prov.requests[len(prov.requests)-1])
	if !strings.Contains(last, "[goal:") {
		t.Fatalf("last user message = %q, want an ambient parked-goal block", last)
	}
	if !strings.Contains(last, "3 failed worker attempts") {
		t.Errorf("ambient block = %q, want it to report 3 failed worker attempts", last)
	}
	if !strings.Contains(last, "resumes automatically") {
		t.Errorf("ambient block = %q, want it to say the goal resumes automatically", last)
	}
	// Classified text only — never the raw provider error (which here even
	// carries a fake secret in a URL, mirroring the MCP leak test) — see
	// classifyGoalWorkerError.
	if strings.Contains(last, "connection reset by peer") {
		t.Errorf("ambient block = %q, leaked the raw provider error text", last)
	}
	if strings.Contains(last, "SECRET") || strings.Contains(last, "mcp.example.com") {
		t.Errorf("ambient block = %q, leaked raw provider error detail", last)
	}

	// Newest-user-message-only: every earlier user message in the same
	// request must be untouched (mirrors
	// TestAmbientProcessStatusPresentAfterStart's prefix check).
	req := prov.requests[len(prov.requests)-1]
	for i, m := range req.Messages {
		if m.Role != message.RoleUser {
			continue
		}
		if i != len(req.Messages)-1 && strings.Contains(renderMsgText(m), "[goal:") {
			t.Fatalf("ambient parked-goal block leaked onto a non-newest message: %+v", m)
		}
	}
}

// TestAmbientGoalParkedStatusAbsentDuringGoalLoopOwnTurns covers the
// contract's other half: the ambient segment must never appear on a request
// the goal loop itself is driving, including the very turns that lead up to
// (and follow, on resume) a park — only an OTHER, non-goal-loop turn sees
// it. clearGoalParkedAtEntry makes this structural: every PursueGoal call
// clears the flag before its own first worker turn.
func TestAmbientGoalParkedStatusAbsentDuringGoalLoopOwnTurns(t *testing.T) {
	dir := t.TempDir()
	var s *Session
	var prov *goalProvider
	synctest.Test(t, func(t *testing.T) {
		prov = &goalProvider{
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "working"}),
				asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
			},
			eval: [][]provider.Event{
				evalTurn("NOT MET: keep going"),
				evalTurn("MET: all done"),
			},
		}
		s = goalSession(t, prov, dir)
		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Achieved {
			t.Fatalf("result = %+v, want achieved", res)
		}
	})

	for i, req := range prov.requests {
		if last := lastUserText(t, req); strings.Contains(last, "[goal:") {
			t.Errorf("request %d = %q, want no ambient parked-goal block on the goal loop's own turns", i, last)
		}
	}
}

// TestAmbientGoalParkedStatusGoneAfterResume proves the flag is cleared the
// moment a new PursueGoal call resumes the parked goal (clearGoalParkedAtEntry,
// called at entry — before the resumed loop's own first worker turn, and
// therefore also before any later ordinary Prompt call after that loop
// finishes) — mirroring the MCP segment's "self-correcting" contract
// (mcpStatusSegment's doc comment) applied to a resume instead of a
// background retry.
func TestAmbientGoalParkedStatusGoneAfterResume(t *testing.T) {
	dir := t.TempDir()
	var s *Session
	var prov *goalProvider
	synctest.Test(t, func(t *testing.T) {
		prov = &goalProvider{
			workerErrN: goalWorkerRetries + 1,
			workerErr:  errors.New("provider: connection reset by peer"),
			worker: [][]provider.Event{
				// Consumed by the resumed PursueGoal call below, once the
				// provider has "recovered" (workerErrN exhausted).
				asstTurn(provider.StopEndTurn, &message.Text{Text: "recovered"}),
				// Consumed by the plain Prompt call after that.
				asstTurn(provider.StopEndTurn, &message.Text{Text: "sure"}),
			},
			eval: [][]provider.Event{
				evalTurn("MET: all done"),
			},
		}
		s = goalSession(t, prov, dir)
		_, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if !IsGoalWorkerParked(err) {
			t.Fatalf("PursueGoal err = %v, want IsGoalWorkerParked", err)
		}

		// The activity-driven resume (server's maybeAutoArmGoal, upstream of
		// this package): a fresh PursueGoal call against the STILL-ACTIVE
		// goal (Registered: true, matching the real resume path — the goal
		// was never cleared, only parked, so there is nothing to
		// re-register), exactly as the server issues after any ordinary
		// prompt turn completes. This call must clear the parked signal at
		// entry, before its own worker turn runs — asserted below via the
		// request stream.
		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel, Registered: true})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Achieved {
			t.Fatalf("resumed result = %+v, want achieved", res)
		}

		// A plain prompt turn after the resume completed must also see no
		// ambient block: nothing set the flag again.
		if _, err := s.Prompt(context.Background(), "anything else?"); err != nil {
			t.Fatal(err)
		}
	})

	for i, req := range prov.requests {
		if last := lastUserText(t, req); strings.Contains(last, "[goal:") {
			t.Errorf("request %d = %q, want no ambient parked-goal block after resume", i, last)
		}
	}
}

// TestAmbientGoalParkedStatusClearedByUpdateGoal covers the review followup
// to the worker-park series: UpdateGoal rewriting an active goal's condition
// must also clear the runtime goalParked/goalParkedReason/goalParkedAttempts
// fields (see UpdateGoal's doc comment and the clear site in goal.go), not
// just the next PursueGoal entry's clearGoalParkedAtEntry. Without this, a
// plain Prompt landing in the window between an UpdateGoal call on a
// parked-but-still-active goal and the next PursueGoal resume would render
// goalParkedSegment's ambient block quoting the OLD park episode's
// reason/attempts, paired confusingly against the NEW condition text the
// model was never actually working toward when it parked.
//
// Red-verified: with the clear in UpdateGoal's condition-changed branch
// removed, this test fails because the plain Prompt call's last request
// still carries a "[goal:" ambient block.
func TestAmbientGoalParkedStatusClearedByUpdateGoal(t *testing.T) {
	dir := t.TempDir()
	var s *Session
	var prov *goalProvider
	var parkErr error
	synctest.Test(t, func(t *testing.T) {
		prov = &goalProvider{
			workerErrN: goalWorkerRetries + 1,
			workerErr:  errors.New("provider: connection reset by peer"),
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
			},
		}
		s = goalSession(t, prov, dir)
		_, parkErr = s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if !IsGoalWorkerParked(parkErr) {
			t.Fatalf("PursueGoal err = %v, want IsGoalWorkerParked", parkErr)
		}

		// Adjust the still-active (parked, not cleared) goal's condition —
		// e.g. a self-adjust tool call or an operator's POST /goal landing
		// while the goal sits parked, before any resume has happened.
		if err := s.UpdateGoal("a completely different condition"); err != nil {
			t.Fatalf("UpdateGoal = %v", err)
		}

		// A plain prompt turn in the window before the next PursueGoal
		// resume must see no ambient block: the update already invalidated
		// the stale park presentation.
		if _, err := s.Prompt(context.Background(), "status?"); err != nil {
			t.Fatal(err)
		}
	})

	last := lastUserText(t, prov.requests[len(prov.requests)-1])
	if strings.Contains(last, "[goal:") {
		t.Fatalf("last user message = %q, want no ambient parked-goal block after UpdateGoal", last)
	}
}

// TestAmbientGoalParkedStatusNeverPersisted mirrors
// TestAmbientProcessStatusNeverPersisted: the block must never leak into
// s.History() or a reloaded session's log.
func TestAmbientGoalParkedStatusNeverPersisted(t *testing.T) {
	sesDir := t.TempDir()
	var s *Session
	var prov *goalProvider
	synctest.Test(t, func(t *testing.T) {
		prov = &goalProvider{
			workerErrN: goalWorkerRetries + 1,
			workerErr:  errors.New("provider: connection reset by peer"),
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
			},
		}
		s = NewSession(Config{
			Providers:    provider.Registry{prov.Name(): prov},
			Model:        message.ModelRef{Provider: prov.Name(), Model: "m1"},
			System:       []string{"base"},
			SessionDir:   sesDir,
			Instructions: &InstructionsConfig{Disabled: true},
			SkillsDirs:   []string{},
		})
		_, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if !IsGoalWorkerParked(err) {
			t.Fatalf("PursueGoal err = %v, want IsGoalWorkerParked", err)
		}
		if _, err := s.Prompt(context.Background(), "status?"); err != nil {
			t.Fatal(err)
		}
	})

	// Sanity: the block really was present on the request (otherwise this
	// test would trivially pass for the wrong reason).
	last := lastUserText(t, prov.requests[len(prov.requests)-1])
	if !strings.Contains(last, "[goal:") {
		t.Fatalf("last user message = %q, want an ambient parked-goal block present before checking persistence", last)
	}

	for _, m := range s.History() {
		if strings.Contains(renderMsgText(m), "[goal:") {
			t.Fatalf("ambient parked-goal block leaked into in-memory history: %+v", m)
		}
	}

	loaded, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	for _, m := range loaded.History() {
		if strings.Contains(renderMsgText(m), "[goal:") {
			t.Fatalf("ambient parked-goal block leaked into persisted history: %+v", m)
		}
	}
}
