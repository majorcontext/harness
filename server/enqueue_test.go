package server

import (
	"encoding/json"
	"net/http"
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
