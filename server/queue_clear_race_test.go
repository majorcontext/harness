package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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
	//
	// One-shot, defensively: in THIS test the seam fires exactly once —
	// the queue is cleared before any dispatch, so dispatchQueueHead
	// returns ok=false, releases the claim, and spawns no runPrompt, and
	// maybeDispatchQueued (the tail call site) is never reached at all
	// (the final assertion below, last_turn == nil, is the proof). The
	// guard is here because the field stays installed for the server's
	// whole life and the body is not safe to run twice; the sibling test
	// TestQueueClearRaceDuringDispatchIsNotAnError is where a real
	// second invocation happens, and it asserts that shape directly. The
	// field is read unsynchronized by production code, so the guard
	// belongs INSIDE the closure; clearing the field from t.Cleanup would
	// be a data race.
	var raceOnce sync.Once
	srv2.queueDispatchRace = func() {
		raceOnce.Do(func() {
			// t.Errorf, not t.Fatalf — see the sibling test's own note:
			// this body runs on the HTTP-handler goroutine, where
			// FailNow is not allowed.
			resp, data := h2.do("DELETE", "/session/"+id+"/queue", nil)
			if resp.StatusCode != http.StatusNoContent {
				t.Errorf("DELETE queue status %d: %s", resp.StatusCode, data)
			}
		})
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
	//
	// One-shot, for the same reason the sibling test above documents: the
	// tail maybeDispatchQueued call re-enters this hook after the request
	// goroutine's own body is already running. calls counts every
	// invocation so the assertion below can prove that late call really
	// happens and is now inert.
	//
	// The guard is a lock-free CAS (atomic.Bool), never sync.Once: this
	// body itself blocks on maybeDispatchQueued's own completion (the
	// wait?until=idle call below) — the exact call the SECOND invocation
	// below IS. maybeDispatchQueued calls this hook BEFORE its own defer
	// clears Server.queueDrainPending, and waitSnapshot (wait.go) refuses
	// to report idle while queueDrainPending is set, so the wait call
	// below cannot return until that second invocation returns from this
	// hook. sync.Once.Do blocks a second, concurrent caller until the
	// first caller's function returns — with Once here, the tail
	// goroutine's second invocation would block waiting for this body to
	// finish, while this body blocks waiting for the wait call, which
	// blocks waiting for that same second invocation to finish: a genuine
	// deadlock, caught live (a 10-minute test-binary timeout panic) under
	// repeated GOMAXPROCS=2 -race hammering. CompareAndSwap never blocks a
	// second caller — it returns false immediately, so the tail
	// invocation falls straight through to maybeDispatchQueued's own
	// defer, clearing queueDrainPending and unblocking the wait call.
	var (
		raced     atomic.Bool
		callsMu   sync.Mutex
		calls     int
		bodyCalls int
	)
	h.srv.queueDispatchRace = func() {
		callsMu.Lock()
		calls++
		callsMu.Unlock()
		if !raced.CompareAndSwap(false, true) {
			return
		}
		callsMu.Lock()
		bodyCalls++
		callsMu.Unlock()
		// t.Errorf, not t.Fatalf: this body runs on the server's
		// HTTP-handler goroutine (the seam fires synchronously from
		// enqueueOrDispatch while the test goroutine is blocked in
		// h.do), and Fatalf's FailNow may only be called from the
		// test goroutine — elsewhere its runtime.Goexit kills the
		// handler mid-request, so a genuine failure here would
		// surface as a broken connection on the outer POST instead
		// of a clean failure. There is no cleanup to abort.
		prov.releaseAll()
		waitResp, waitData := h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
		if waitResp.StatusCode != http.StatusOK {
			t.Errorf("wait for first turn's idle status %d: %s", waitResp.StatusCode, waitData)
		}
		delResp, delData := h.do("DELETE", "/session/"+id+"/queue", nil)
		if delResp.StatusCode != http.StatusNoContent {
			t.Errorf("DELETE queue status %d: %s", delResp.StatusCode, delData)
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

	// The late invocation this test's one-shot guard exists for: the first
	// turn's own runPrompt tail calls maybeDispatchQueued, which fires the
	// hook again. Waiting for the session to go idle makes that call
	// deterministic rather than timing-dependent, so this assertion is a
	// real guard: more than one invocation, exactly one body run.
	waitResp, waitData := h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
	if waitResp.StatusCode != http.StatusOK {
		t.Fatalf("wait until=idle status %d: %s", waitResp.StatusCode, waitData)
	}
	callsMu.Lock()
	gotCalls, gotBody := calls, bodyCalls
	callsMu.Unlock()
	if gotCalls < 2 {
		t.Errorf("queueDispatchRace invocations = %d, want at least 2: the tail dispatch call this guard protects against no longer happens, so the guard needs re-deriving", gotCalls)
	}
	if gotBody != 1 {
		t.Errorf("queueDispatchRace body runs = %d, want exactly 1: a later invocation re-entered the body and can panic the package after the test ends", gotBody)
	}
}
