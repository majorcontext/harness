package server

import (
	"net/http/httptest"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestDeleteGoalNonResidentClearsAndJournals is issue #78's regression test:
// DELETE /session/{id}/goal on a session that is NOT resident but exists on
// disk with an active goal (the boot-time restart-paused case -- see
// pauseArmedGoalsAtBoot) must actually clear the goal -- journal
// goal.cleared, flip the engine's goal state to inactive -- not merely
// return 204 while leaving everything untouched.
//
// Before the fix, handleGoalDelete's `st != nil` guard skipped ClearGoal
// entirely whenever the session was not already resident: the handler still
// returned 204 (nothing there to reject), but no goal.cleared was
// journaled, engine.Session.goalActive never flipped, and goalState[id]
// .active stayed true -- so a SECOND restart re-paused the "cleared" goal
// right back, the exact operator trap this test guards against.
func TestDeleteGoalNonResidentClearsAndJournals(t *testing.T) {
	dir := t.TempDir()
	prov := &goalProv{
		name:   "test",
		worker: [][]provider.Event{asstTurn("try 1")},
		eval:   [][]provider.Event{asstTurn("NOT MET: nope")},
	}
	mutate := func(o *Options) {
		o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
	}
	srv1 := newServer(t, dir, prov, 0, mutate)
	ts1 := httptest.NewServer(srv1)
	h1 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv1, ts: ts1}

	id := h1.createSession("test/m1")
	sse := h1.openSSE("?from=0", "")
	resp, data := h1.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "impossible", "max_turns": 1})
	if resp.StatusCode != 202 {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t)
	sse.stop()
	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	// Restart: the goal is active (max turns exhausted, never cleared) with
	// no loop attached in the new process -- pauseArmedGoalsAtBoot marks it
	// paused/restart. Nothing has touched the session in srv2 yet, so it is
	// not resident.
	srv2 := newServer(t, dir, prov, 0, mutate)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	before := h2.getPausedGoalView(id)
	if before.Goal == nil || !before.Goal.Active || !before.Goal.Paused {
		t.Fatalf("before delete, goal = %+v, want active+paused/restart", before.Goal)
	}
	srv2.mu.Lock()
	_, resident := srv2.sessions[id]
	srv2.mu.Unlock()
	if resident {
		t.Fatal("test setup invariant broken: session must be non-resident before DELETE")
	}

	resp, data = h2.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("DELETE goal status %d: %s", resp.StatusCode, data)
	}

	srv2.mu.Lock()
	var sawCleared bool
	for _, ev := range srv2.journal {
		if ev.SessionID == id && ev.Type == "goal.cleared" {
			sawCleared = true
		}
	}
	srv2.mu.Unlock()
	if !sawCleared {
		t.Error("no goal.cleared record journaled by DELETE /goal on a non-resident session")
	}

	after := h2.getPausedGoalView(id)
	if after.Goal != nil && after.Goal.Active {
		t.Errorf("goal after DELETE = %+v, want inactive", after.Goal)
	}

	// Restart AGAIN: if goalState[id].active incorrectly stayed true, this
	// second boot would re-pause the (supposedly cleared) goal.
	if err := srv2.Close(); err != nil {
		t.Fatalf("closing second server: %v", err)
	}
	ts2.Close()

	srv3 := newServer(t, dir, prov, 0, mutate)
	ts3 := httptest.NewServer(srv3)
	t.Cleanup(ts3.Close)
	h3 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv3, ts: ts3}

	final := h3.getPausedGoalView(id)
	if final.Goal != nil && final.Goal.Active {
		t.Errorf("goal after second restart = %+v, want inactive (no re-pause)", final.Goal)
	}
}
