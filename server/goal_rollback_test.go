package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/provider"
)

// TestGoalRegisterFailureRollsBackAndSessionStaysUsable exercises the 0-hit
// RegisterGoal-failure branch in handleGoal: a goal is registered and its
// worker turn parks (blockWorker), then POST /abort cancels the loop's
// context WITHOUT clearing the goal (see engine.Session.PursueGoal: a
// context.Canceled worker turn "leaves the goal exactly as it is" so a drain
// is resumable) — the engine session is left with goalActive still true even
// though the server has flipped the session back to idle/not-running. A
// second POST /goal on the same session then reaches RegisterGoal, which
// rejects it because a goal is already active, taking the rollback path in
// handleGoal (undo the claim, s.wg.Done(), 409).
//
// The assertions: (1) the failed registration must not strand the session as
// "running" — a subsequent prompt_async must be accepted (202), never 409;
// (2) Drain must still complete. A missing wg.Done on the rollback path would
// leave the WaitGroup counter permanently off by one, so Drain (which calls
// wg.Wait() unconditionally once its grace context expires) would hang
// forever — the test bounds that wait so a regression fails instead of
// hanging the whole suite.
func TestGoalRegisterFailureRollsBackAndSessionStaysUsable(t *testing.T) {
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

	// Second goal on the same session: claimForPrompt succeeds (not
	// running), but st.sess.RegisterGoal fails because "first" is still
	// active. This is the 0-hit rollback branch.
	resp, data = h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "second"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second POST goal status %d, want 409: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "already active") {
		t.Fatalf("second POST goal body = %s, want it to name the RegisterGoal failure (already active), not a busy conflict", data)
	}

	// Not stranded busy: a plain prompt is accepted immediately (claiming the
	// run slot fresh proves handleGoal's rollback correctly reset
	// running/cancel on the failed second registration).
	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async after failed goal registration = %d, want 202 (session stranded busy): %s", resp.StatusCode, data)
	}

	// Unblock the parked prompt's worker turn (blockWorker still applies —
	// it never consumed the "started" close, sync.Once already fired, so
	// just abort it) and let the session settle before draining.
	resp, data = h.do("POST", "/session/"+id+"/abort", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second abort status %d: %s", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t)

	// Drain must complete — bounded, so a reintroduced missing wg.Done on the
	// rollback path fails this test instead of hanging the suite forever.
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
