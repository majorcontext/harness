package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestQueueClearRaceDuringIdleDispatchIsNotAnError is the regression test for
// handlePrompt's idle-with-queue branch: dispatchQueueHead's ok=false path
// used to be treated as "structurally unreachable" and answered with a 500,
// but it IS reachable via a concurrent DELETE /session/{id}/queue landing in
// the gap between this branch's own EnqueuePrompt call and its
// dispatch-the-head attempt — a benign, already-documented race (DELETE
// /queue is safe to call regardless of run-slot state), not a server bug.
//
// The fix answers 202 {status:"queued", queued:0} instead: the arriving
// prompt WAS durably accepted (enqueued, journaled) even though a concurrent
// clear swept it (and everything ahead of it) away before it could run.
func TestQueueClearRaceDuringIdleDispatchIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"} // must NEVER be called: the queue is cleared before any dispatch
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

	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	// Restart: idle, one prompt already queued (restart refold) -- the same
	// idle-with-queue shape TestIdlePromptWithQueueGoesFIFO exercises.
	srv2 := newServer(t, dir, prov, 0)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	before := h2.getSessionJSON(id)
	if before.Queued != 1 {
		t.Fatalf("before: queued = %d, want 1", before.Queued)
	}

	// Force the race: while handlePrompt's idle-with-queue branch holds no
	// lock (right after its own EnqueuePrompt, right before dispatching the
	// head), clear the entire queue -- q1 AND the prompt this very request
	// just enqueued -- out from under it.
	srv2.queueDispatchRace = func() {
		resp, data := h2.do("DELETE", "/session/"+id+"/queue", nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE queue status %d: %s", resp.StatusCode, data)
		}
	}

	resp, data := h2.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "third"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d, want 202: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" || qr.Queued != 0 {
		t.Fatalf("response = %+v, want status=queued queued=0 (accepted, then cleared before it ran)", qr)
	}

	final := h2.getSessionJSON(id)
	if final.State != "idle" {
		t.Fatalf("final state = %q, want idle (dispatchQueueHead released the claim it took)", final.State)
	}
	if final.Queued != 0 {
		t.Fatalf("final queued = %d, want 0", final.Queued)
	}
	if final.LastTurn != nil {
		t.Errorf("final last_turn = %+v, want nil (nothing ever dispatched)", final.LastTurn)
	}
}

// TestQueueClearRaceDuringDispatchIsNotAnError is
// TestQueueClearRaceDuringIdleDispatchIsNotAnError's counterpart for
// enqueueOrDispatch (handlePrompt's same-session-busy branch): its own
// won-the-retry dispatch attempt can lose the same way, to the same benign
// DELETE /session/{id}/queue race, and must answer 202 rather than 500 too.
func TestQueueClearRaceDuringDispatchIsNotAnError(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	t.Cleanup(prov.releaseAll)

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	// Force the race: while enqueueOrDispatch holds no lock (right after its
	// own EnqueuePrompt, right before its retry claimForPrompt call), let
	// the first turn finish (freeing the run slot so the retry can win it)
	// and then clear the entire queue -- including the "second" prompt this
	// very request just enqueued -- before dispatchQueueHead gets a chance.
	h.srv.queueDispatchRace = func() {
		prov.releaseAll()
		waitResp, waitData := h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
		if waitResp.StatusCode != http.StatusOK {
			t.Fatalf("wait for first turn's idle status %d: %s", waitResp.StatusCode, waitData)
		}
		delResp, delData := h.do("DELETE", "/session/"+id+"/queue", nil)
		if delResp.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE queue status %d: %s", delResp.StatusCode, delData)
		}
	}

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "second"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second prompt status %d, want 202: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" || qr.Queued != 0 {
		t.Fatalf("response = %+v, want status=queued queued=0 (accepted, then cleared before it ran)", qr)
	}

	final := h.getSessionJSON(id)
	if final.State != "idle" {
		t.Fatalf("final state = %q, want idle", final.State)
	}
	if final.Queued != 0 {
		t.Fatalf("final queued = %d, want 0", final.Queued)
	}
}
