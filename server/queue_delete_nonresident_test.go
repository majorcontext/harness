package server

import (
	"net/http/httptest"
	"testing"
)

// TestDeleteQueueColdSessionSurvivesResidencyRace is the regression test for
// handleQueueDelete's cold-session divergence: DELETE /session/{id}/queue on
// a session that is NOT resident resolves it via a transient LoadSession that
// (before the fix) is never registered into s.sessions. A concurrent request
// that ALSO cold-loads the same session (e.g. a prompt_async arriving in the
// same window) registers its OWN, second *engine.Session instance for the
// same on-disk log and claims the run slot. Before the fix, DequeueAllPrompts
// mutated the transient, unregistered copy: the clear looked like it
// succeeded (204, and even a durable prompt.dequeued(cleared) journal
// record, since both instances share the same OnEvent->Publish wiring) while
// the REGISTERED instance -- the one every future drain actually uses --
// kept its stale, un-cleared queue. The operator's clear was silently
// ignored and the journal contradicted what the session would actually do
// next.
//
// The fix mirrors handleGoalDelete's cold path: load outside s.mu, then
// under s.mu prefer a resident that appeared meanwhile (the race loser's own
// load is discarded), else register the loaded instance into residency --
// so DequeueAllPrompts always mutates the exact instance future drains will
// use.
func TestDeleteQueueColdSessionSurvivesResidencyRace(t *testing.T) {
	dir := t.TempDir()
	prov := newBlockingProvider("test")

	srv1 := newServer(t, dir, prov, 0)
	ts1 := httptest.NewServer(srv1)
	h1 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv1, ts: ts1}

	id := h1.createSession("test/m1")

	srv1.mu.Lock()
	st := srv1.sessions[id]
	srv1.mu.Unlock()
	if st == nil {
		t.Fatal("session not resident right after creation")
	}
	if _, err := st.sess.EnqueuePrompt("q1"); err != nil {
		t.Fatalf("EnqueuePrompt q1: %v", err)
	}
	if _, err := st.sess.EnqueuePrompt("q2"); err != nil {
		t.Fatalf("EnqueuePrompt q2: %v", err)
	}

	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	// Restart: the queue (q1, q2) survives on disk (restart-refold), and
	// nothing has touched the session in srv2 yet, so it is not resident.
	srv2 := newServer(t, dir, prov, 0)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}
	t.Cleanup(prov.releaseAll)

	srv2.mu.Lock()
	_, resident := srv2.sessions[id]
	srv2.mu.Unlock()
	if resident {
		t.Fatal("test setup invariant broken: session must be non-resident before DELETE")
	}

	// Force the race: while DELETE /queue's cold path holds no lock (between
	// its own LoadSession and re-acquiring s.mu), a concurrent prompt_async
	// cold-loads and registers its OWN instance, which -- since the queue it
	// sees is non-empty -- enqueues its own text behind the existing pair and
	// immediately dispatches the queue's head (q1) into the run slot it just
	// claimed (global FIFO, same as TestIdlePromptWithQueueGoesFIFO). q1's
	// dispatched turn blocks (newBlockingProvider) so the test can inspect
	// state deterministically before it ever completes.
	srv2.queueDeleteRace = func() {
		resp, data := h2.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": "racer"}},
		})
		if resp.StatusCode != 202 {
			t.Fatalf("racer prompt_async status %d: %s", resp.StatusCode, data)
		}
	}

	resp, data := h2.do("DELETE", "/session/"+id+"/queue", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("DELETE queue status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // q1's dispatched turn is genuinely in flight

	// The money assertion: the RESIDENT instance -- the one every future
	// drain (maybeDispatchQueued) actually reads -- must be the one the
	// clear mutated. Before the fix, this reads 2 (q2, racer): the clear
	// silently landed on a transient, discarded copy instead.
	srv2.mu.Lock()
	registered := srv2.sessions[id]
	srv2.mu.Unlock()
	if registered == nil {
		t.Fatal("expected the racer's prompt_async to have registered the session")
	}
	if got := len(registered.sess.QueuedPrompts()); got != 0 {
		t.Fatalf("resident queue depth after DELETE = %d, want 0 (clear must land on the instance future drains use)", got)
	}

	// Same story via the wire: GET /session for a resident session reads
	// straight from the resident instance (see buildSession).
	sess := h2.getSessionJSON(id)
	if sess.Queued != 0 {
		t.Fatalf("GET /session queued after DELETE = %d, want 0", sess.Queued)
	}
	if sess.State != "busy" {
		t.Fatalf("state after DELETE = %q, want busy (q1's dispatched turn is untouched)", sess.State)
	}

	// Release q1's turn: its own tail calls maybeDispatchQueued, which must
	// find nothing left to dispatch -- q2 and racer were cleared and must
	// NEVER be delivered.
	prov.releaseAll()
	resp, data = h2.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("wait for idle status %d: %s", resp.StatusCode, data)
	}

	final := h2.getSessionJSON(id)
	if final.State != "idle" {
		t.Fatalf("final state = %q, want idle (no further dispatch)", final.State)
	}
	if final.Queued != 0 {
		t.Fatalf("final queued = %d, want 0", final.Queued)
	}

	// Restart again: the refolded queue on disk must be exactly the
	// post-clear set (empty) -- the clear was genuinely durable against the
	// instance that mattered, not just a journal entry contradicted by what
	// the session actually does next.
	if err := srv2.Close(); err != nil {
		t.Fatalf("closing second server: %v", err)
	}
	ts2.Close()

	srv3 := newServer(t, dir, prov, 0)
	ts3 := httptest.NewServer(srv3)
	t.Cleanup(ts3.Close)
	h3 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv3, ts: ts3}

	refolded := h3.getSessionJSON(id)
	if refolded.Queued != 0 {
		t.Fatalf("refolded queue after second restart = %d, want 0 (exactly the post-clear set)", refolded.Queued)
	}
}
