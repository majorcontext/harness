package server

import (
	"encoding/json"
	"net/http"
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
