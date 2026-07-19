package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/majorcontext/harness/provider"
)

// TestGoalReArmAfterAbortWithDifferentConditionResumes replaces
// TestGoalRegisterFailureRollsBackAndSessionStaysUsable: Task 5 changes
// handleGoal's claim-succeeded branch so a goal left active-but-idle by an
// abort (see engine.Session.PursueGoal: a context.Canceled worker turn
// "leaves the goal exactly as it is" so a drain is resumable) is treated
// exactly like a paused/restart goal — a second POST /goal naming a
// DIFFERENT condition now updates and resumes it (202, "started") instead of
// reaching RegisterGoal's "already active" rejection, which is what the
// original version of this test exercised (see
// TestGoalReArmDifferentConditionUpdatesAndResumes for the dedicated
// coverage of that behavior in the restart case).
//
// The original test's other concerns still matter and are preserved here:
// (1) the session must never be left stranded busy in some OTHER way —
// prompt_async correctly still 409s while the re-armed loop runs; (2) Drain
// must still complete cleanly (bounded, so a reintroduced wg.Done leak on
// either the RegisterGoal path or this new update-and-resume path fails fast
// instead of hanging the suite).
func TestGoalReArmAfterAbortWithDifferentConditionResumes(t *testing.T) {
	prov := &goalProv{
		name:        "test",
		blockWorker: true,
		started:     make(chan struct{}),
		eval:        [][]provider.Event{asstTurn("MET: ok")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")

	// First goal: registers fine, worker turn parks immediately.
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "first"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first POST goal status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // the goal occupies the session

	// Abort (not DELETE /goal): cancels the loop's context but does NOT clear
	// the goal — goalActive stays true on the engine session, per
	// PursueGoal's context.Canceled branch ("leave the goal exactly as it
	// is... a drain must be resumable").
	resp, data = h.do("POST", "/session/"+id+"/abort", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("abort status %d: %s", resp.StatusCode, data)
	}

	// Wait for the loop goroutine to unwind and the session to go idle again
	// (running=false), without clearing the still-active goal.
	sse.collectUntilIdle(t)

	// Second goal on the same session, naming a DIFFERENT condition:
	// claimForPrompt succeeds (not running), ActiveGoal() reports "first"
	// still active, and the new condition differs — handleGoal now updates
	// in place and resumes, rather than reaching RegisterGoal's rejection.
	resp, data = h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "second"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("re-arm with a different condition after abort = %d, want 202: %s", resp.StatusCode, data)
	}
	var posted struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &posted); err != nil {
		t.Fatal(err)
	}
	if posted.Status != "started" {
		t.Errorf("re-arm response status = %q, want %q", posted.Status, "started")
	}

	// Not stranded busy in some OTHER way: the goal is running again
	// (against the new condition), so prompt_async correctly still 409s.
	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi"}},
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("prompt_async while the re-armed goal runs = %d, want 409: %s", resp.StatusCode, data)
	}

	// Unblock the parked worker turn and let the session settle before
	// draining.
	resp, data = h.do("POST", "/session/"+id+"/abort", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second abort status %d: %s", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t)

	// Drain must complete — bounded, so a reintroduced missing wg.Done on
	// either code path fails this test instead of hanging the suite forever.
	assertDrainCompletes(t, h.srv)
}

// assertDrainCompletes calls Drain on a tracked goroutine and requires it to
// return within drainBound. Every prompt/goal in this test has already been
// aborted and observed idle before this is called, so on correct code Drain
// (which itself waits on wg.Wait(), unconditionally, once its own grace
// context expires) returns almost immediately — drainBound is generous for
// that real case but tight enough that a reintroduced missing wg.Done on the
// RegisterGoal-failure rollback path (which leaves the WaitGroup counter
// permanently off by one, so wg.Wait() never returns) manifests as a fast,
// unambiguous test failure instead of hanging the suite until the package
// test timeout.
func assertDrainCompletes(t *testing.T, srv *Server) {
	t.Helper()
	const drainBound = 2 * time.Second

	start := time.Now()
	done := make(chan struct{})
	go func() {
		srv.Drain(context.Background())
		close(done)
	}()
	select {
	case <-done:
		// Confirmed complete, not merely "hasn't failed yet": Drain returned
		// and closed done within the bound.
		if elapsed := time.Since(start); elapsed >= drainBound {
			t.Fatalf("Drain returned but took %s, at/past the %s bound; treat as a hang", elapsed, drainBound)
		}
	case <-time.After(drainBound):
		t.Fatalf("Drain did not return within %s; a missing wg.Done on the RegisterGoal-failure rollback path would leave the WaitGroup counter stuck and Drain hung forever", drainBound)
	}
}
