package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// enqueue is a small helper wrapping POST /session/{id}/enqueue's JSON body
// shape (parts + seq), used by every test in this file.
func (h *harness) enqueue(id, text string, seq int64) (*http.Response, []byte) {
	h.t.Helper()
	return h.do("POST", "/session/"+id+"/enqueue", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": text}},
		"seq":   seq,
	})
}

// waitIdle blocks (via GET /session/{id}/wait?until=idle) until the session's
// composite state reads idle, returning the final wait snapshot.
func (h *harness) waitIdle(id string) waitJSON {
	h.t.Helper()
	resp, data := h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("wait status %d: %s", resp.StatusCode, data)
	}
	var wr waitJSON
	if err := json.Unmarshal(data, &wr); err != nil {
		h.t.Fatal(err)
	}
	return wr
}

// TestEnqueueIdleDispatchesImmediately is the red-first test for POST
// /session/{id}/enqueue's idle happy path (Task 4 of docs/plans/2026-07-21-
// durable-enqueue.md): an idle session's free run slot is claimed, the
// prompt is durably enqueued (fsynced before any response), and — since it
// is also the queue head — dispatched immediately, reported "started".
func TestEnqueueIdleDispatchesImmediately(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("run this done")}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.enqueue(id, "run this", 1)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("enqueue status %d: %s", resp.StatusCode, data)
	}
	var er enqueueResponse
	if err := json.Unmarshal(data, &er); err != nil {
		t.Fatal(err)
	}
	if er.Status != "started" || er.Watermark != 1 || er.Queued != 0 {
		t.Fatalf("enqueue response = %+v, want status=started watermark=1 queued=0", er)
	}

	wr := h.waitIdle(id)
	if wr.State != "idle" {
		t.Fatalf("state after drain = %q, want idle", wr.State)
	}

	sess := h.getSessionJSON(id)
	if sess.Queued != 0 {
		t.Errorf("queued after drain = %d, want 0", sess.Queued)
	}
}

// TestEnqueueBusyQueuesAndDeduplicates is the red-first test for POST
// /session/{id}/enqueue's busy branch and idempotency contract: while a
// session is busy, enqueue durably queues (202 "queued"), a retry with the
// SAME seq is a clean 200 "duplicate" no-op (not a second queue entry), and
// once the occupant releases, the queue drains and delivers the queued text.
func TestEnqueueBusyQueuesAndDeduplicates(t *testing.T) {
	prov := &queueProv{
		name:    "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
		turns:   [][]provider.Event{asstTurn("queued done")},
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "occupant"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("occupant prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started
	sse.waitFor(t, "session.status") // occupant's own busy

	// First enqueue while busy: durably queued.
	resp, data = h.enqueue(id, "run this", 1)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("enqueue status %d: %s", resp.StatusCode, data)
	}
	var er enqueueResponse
	if err := json.Unmarshal(data, &er); err != nil {
		t.Fatal(err)
	}
	if er.Status != "queued" || er.Watermark != 1 || er.Queued != 1 {
		t.Fatalf("first enqueue response = %+v, want status=queued watermark=1 queued=1", er)
	}

	// Same seq again: clean 200 duplicate no-op, watermark unchanged, no
	// second queue entry.
	resp, data = h.enqueue(id, "run this", 1)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("duplicate enqueue status %d: %s", resp.StatusCode, data)
	}
	var dup enqueueResponse
	if err := json.Unmarshal(data, &dup); err != nil {
		t.Fatal(err)
	}
	if dup.Status != "duplicate" || dup.Watermark != 1 {
		t.Fatalf("duplicate enqueue response = %+v, want status=duplicate watermark=1", dup)
	}

	sess := h.getSessionJSON(id)
	if sess.Queued != 1 {
		t.Fatalf("queued depth = %d, want 1 (duplicate must not add a second entry)", sess.Queued)
	}

	close(prov.release) // let the occupant finish; queue should drain

	occupantIdle := sse.waitFor(t, "session.status")
	if occupantIdle.Status != "idle" {
		t.Fatalf("expected occupant's idle, got status %q", occupantIdle.Status)
	}
	dispatchedBusy := sse.waitFor(t, "session.status")
	if dispatchedBusy.Status != "busy" {
		t.Fatalf("expected dispatched queued turn's busy, got status %q", dispatchedBusy.Status)
	}
	var asst Event
	for {
		asst = sse.waitFor(t, "message")
		if asst.Message != nil && asst.Message.Role == message.RoleAssistant {
			break
		}
	}
	if asst.Message.Parts.Text() != "queued done" {
		t.Fatalf("dispatched turn text = %q, want %q", asst.Message.Parts.Text(), "queued done")
	}

	wr := h.waitIdle(id)
	if wr.State != "idle" {
		t.Fatalf("final state = %q, want idle", wr.State)
	}
	sess = h.getSessionJSON(id)
	if sess.Queued != 0 {
		t.Errorf("queued after full drain = %d, want 0", sess.Queued)
	}
}

// TestEnqueueValidation is the red-first test for POST /session/{id}/enqueue's
// request validation: missing seq, seq 0, empty parts, and a non-text part
// type must all 400, mirroring handlePrompt's own validation.
func TestEnqueueValidation(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	t.Run("missing seq", func(t *testing.T) {
		resp, data := h.do("POST", "/session/"+id+"/enqueue", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": "x"}},
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d: %s", resp.StatusCode, data)
		}
	})

	t.Run("seq 0", func(t *testing.T) {
		resp, data := h.enqueue(id, "x", 0)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d: %s", resp.StatusCode, data)
		}
	})

	t.Run("empty parts", func(t *testing.T) {
		resp, data := h.do("POST", "/session/"+id+"/enqueue", map[string]any{
			"parts": []map[string]string{},
			"seq":   1,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d: %s", resp.StatusCode, data)
		}
	})

	t.Run("non-text part type", func(t *testing.T) {
		resp, data := h.do("POST", "/session/"+id+"/enqueue", map[string]any{
			"parts": []map[string]string{{"type": "image", "text": "x"}},
			"seq":   1,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d: %s", resp.StatusCode, data)
		}
	})
}

// TestEnqueueDuplicateOnIdleWithQueueDrainsHead is the regression test for a
// liveness bug in handleEnqueue's idle branch: a concurrent same-seq retry
// can land in enqueueDurableBusy WHILE this request holds the idle claim,
// durably enqueue there (advancing the watermark this request then sees as
// a duplicate), and then lose its own one-shot claim retry back to this
// request — see enqueueDurableBusy's doc comment for that exact race. The
// old code released the claim on the duplicate (and error) path without
// ever checking the queue again, stranding the concurrent request's
// already-durable prompt on a now-idle session with nothing left to
// dispatch it until unrelated future activity — durability held, but
// delivery was delayed indefinitely.
//
// Reproduced here deterministically, without real concurrency: seed the
// session's durable queue directly on the resident engine.Session (the same
// technique TestQueueRestartRefoldNoAutoDispatch uses to arrange a
// non-empty queue on an idle session), which also advances the watermark to
// 1, then hit POST /session/{id}/enqueue with seq=1 — a clean duplicate
// from the endpoint's point of view — and assert the pre-seeded head still
// gets dispatched and drains.
func TestEnqueueDuplicateOnIdleWithQueueDrainsHead(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("drained")}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	h.srv.mu.Lock()
	st := h.srv.sessions[id]
	h.srv.mu.Unlock()
	if st == nil {
		t.Fatal("session not resident right after creation")
	}
	if _, dup, err := st.sess.EnqueuePromptDurable("queued before duplicate", 1); err != nil || dup {
		t.Fatalf("seed EnqueuePromptDurable: dup=%v err=%v", dup, err)
	}

	resp, data := h.enqueue(id, "run this", 1) // seq == watermark: duplicate
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enqueue status %d: %s", resp.StatusCode, data)
	}
	var er enqueueResponse
	if err := json.Unmarshal(data, &er); err != nil {
		t.Fatal(err)
	}
	if er.Status != "duplicate" || er.Watermark != 1 {
		t.Fatalf("enqueue response = %+v, want status=duplicate watermark=1", er)
	}

	wr := h.waitIdle(id)
	if wr.State != "idle" {
		t.Fatalf("state after drain = %q, want idle", wr.State)
	}
	sess := h.getSessionJSON(id)
	if sess.Queued != 0 {
		t.Fatalf("queued after drain = %d, want 0 (stranded head must be dispatched)", sess.Queued)
	}
	if sess.LastTurn == nil || sess.LastTurn.Outcome != "completed" {
		t.Fatalf("last_turn = %+v, want outcome=completed (the seeded head must actually run)", sess.LastTurn)
	}
}

// TestEnqueueWorkdirBusyRejected mirrors TestPromptSameWorkdirBusyRejected
// (workdir_test.go) for POST /session/{id}/enqueue: session A holds its
// workdir busy (a channel-blocked provider mid-stream), so an enqueue
// against session B — which defaults to the same workdir — is rejected with
// 409 naming the holder, same as prompt_async's own workdir-busy path.
// TestEnqueueWatermarkSurvivesRestart is the primitive's reason to exist: a
// message accepted (2xx) by one serve process must read as a duplicate to
// its successor over the same session dir — the upstream that acked on the
// first 2xx must never cause a double delivery by retrying into the new
// process, and a message never accepted must not read as one.
//
// Mirrors restart_test.go's TestGoalActiveSurvivesRestart two-server-over-
// one-dir pattern: server one is closed WITHOUT registering its
// httptest.Server via t.Cleanup (a manual ts1.Close() below does that),
// since t.Cleanup(ts.Close) on both servers over the same *testing.T would
// leave the first Close racing/duplicating harmlessly at best and masking a
// real double-close bug at worst — restart_test.go avoids it the same way.
// Server two starts fresh (its own *Server, its own scriptedProvider) over
// the SAME on-disk session dir, so nothing but the journal on disk connects
// the two — no shared in-memory state survives the "restart".
func TestEnqueueWatermarkSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	prov1 := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("m1 done")}}
	srv1 := newServer(t, dir, prov1, 0)
	ts1 := httptest.NewServer(srv1)
	h1 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv1, ts: ts1}

	id := h1.createSession("test/m1")
	resp, data := h1.enqueue(id, "m1", 1)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first enqueue status %d: %s", resp.StatusCode, data)
	}
	h1.waitIdle(id)

	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	// Fresh process, fresh scripted provider, same dir: harness 2 has zero
	// in-memory continuity with harness 1 — only the on-disk journal.
	prov2 := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("m2 done")}}
	srv2 := newServer(t, dir, prov2, 0)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	// The message process one already accepted must read as a duplicate to
	// process two — the upstream that got the first 2xx must never trigger
	// a second delivery by retrying into the successor.
	resp, data = h2.enqueue(id, "m1", 1)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("successor duplicate check status %d: %s, want 200", resp.StatusCode, data)
	}
	var dup enqueueResponse
	if err := json.Unmarshal(data, &dup); err != nil {
		t.Fatal(err)
	}
	if dup.Status != "duplicate" || dup.Watermark != 1 {
		t.Fatalf("successor duplicate response = %+v, want status=duplicate watermark=1", dup)
	}

	// A message NEVER accepted by process one must not read as one: a fresh
	// seq is accepted normally by the successor.
	resp, data = h2.enqueue(id, "m2", 2)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("fresh seq after restart status %d: %s", resp.StatusCode, data)
	}
	var fresh enqueueResponse
	if err := json.Unmarshal(data, &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Watermark != 2 {
		t.Fatalf("fresh enqueue response = %+v, want watermark=2", fresh)
	}
}

// TestQueueGetReturnsWatermarkAndPending is the red-first test for GET
// /session/{id}/queue (Task 6 of docs/plans/2026-07-21-durable-enqueue.md):
// the reconciliation read surface. While the session is busy (queueProv's
// blocking pattern, same occupant setup as
// TestEnqueueBusyQueuesAndDeduplicates), enqueue durably queues a prompt,
// then GET must report the watermark and exactly the one pending entry
// (id/text/seq), live off the resident instance.
func TestQueueGetReturnsWatermarkAndPending(t *testing.T) {
	prov := &queueProv{
		name:    "test",
		started: make(chan struct{}),
		release: make(chan struct{}),
		turns:   [][]provider.Event{asstTurn("occupant done")},
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "occupant"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("occupant prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.enqueue(id, "pending", 4)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("enqueue status %d: %s", resp.StatusCode, data)
	}

	resp, data = h.do("GET", "/session/"+id+"/queue", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET queue status %d: %s", resp.StatusCode, data)
	}
	var q queueGetResponse
	if err := json.Unmarshal(data, &q); err != nil {
		t.Fatal(err)
	}
	if q.Watermark != 4 || len(q.Queued) != 1 {
		t.Fatalf("queue read = %+v, want watermark=4 and exactly one queued entry", q)
	}
	if q.Queued[0].Text != "pending" || q.Queued[0].Seq != 4 || q.Queued[0].ID <= 0 {
		t.Fatalf("queued[0] = %+v, want text=pending seq=4 id>0", q.Queued[0])
	}

	close(prov.release)
	h.waitIdle(id)
}

// TestQueueGetNonResidentReadsFromDisk is TestQueueGetReturnsWatermarkAndPending's
// cold-session counterpart: seed the durable queue on a resident session
// (same technique TestQueueRestartRefoldNoAutoDispatch and
// TestEnqueueDuplicateOnIdleWithQueueDrainsHead use), restart the process
// over the same dir so the session is NOT resident in the successor, then
// GET /session/{id}/queue there. It must read the same watermark and
// pending entry back from a transient replay — same journal, same fold, so
// resident and non-resident answers can never disagree — and it must NOT
// make the session resident or claim the run slot: this is a pure read.
func TestQueueGetNonResidentReadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	prov1 := &scriptedProvider{name: "test"}
	srv1 := newServer(t, dir, prov1, 0)
	ts1 := httptest.NewServer(srv1)
	h1 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv1, ts: ts1}

	id := h1.createSession("test/m1")
	srv1.mu.Lock()
	st := srv1.sessions[id]
	srv1.mu.Unlock()
	if st == nil {
		t.Fatal("session not resident right after creation")
	}
	if _, dup, err := st.sess.EnqueuePromptDurable("pending", 4); err != nil || dup {
		t.Fatalf("seed EnqueuePromptDurable: dup=%v err=%v", dup, err)
	}

	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}
	ts1.Close()

	prov2 := &scriptedProvider{name: "test"}
	srv2 := newServer(t, dir, prov2, 0)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	srv2.mu.Lock()
	_, resident := srv2.sessions[id]
	srv2.mu.Unlock()
	if resident {
		t.Fatal("test setup invariant broken: session must be non-resident before GET")
	}

	resp, data := h2.do("GET", "/session/"+id+"/queue", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET queue status %d: %s", resp.StatusCode, data)
	}
	var q queueGetResponse
	if err := json.Unmarshal(data, &q); err != nil {
		t.Fatal(err)
	}
	if q.Watermark != 4 || len(q.Queued) != 1 || q.Queued[0].Text != "pending" || q.Queued[0].Seq != 4 {
		t.Fatalf("queue read = %+v, want watermark=4 one queued entry text=pending seq=4", q)
	}

	srv2.mu.Lock()
	_, residentAfter := srv2.sessions[id]
	srv2.mu.Unlock()
	if residentAfter {
		t.Fatal("GET /queue made the session resident; it must be a transient read, no run-slot claim")
	}
}

func TestEnqueueWorkdirBusyRejected(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	t.Cleanup(prov.releaseAll)

	idA := h.createSession("test/m1")
	idB := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+idA+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt A status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // A is now blocked mid-stream, holding its workdir

	resp, data = h.enqueue(idB, "second", 1)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("enqueue B (same workdir) status %d, want 409: %s", resp.StatusCode, data)
	}
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(data, &e)
	if !strings.Contains(e.Error, idA) {
		t.Errorf("409 error = %q, want it to name holder session %s", e.Error, idA)
	}
}
