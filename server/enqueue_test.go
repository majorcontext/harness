package server

import (
	"bytes"
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

// enqueueParts is enqueue's counterpart for a body whose parts are not all
// plain text — a blob attachment mixed in, or a blob-only body — built the
// same way prompt_attachments_test.go's prompt_async bodies are (a raw
// []any of part maps, so attachmentPart's own map shape drops in
// unchanged).
func (h *harness) enqueueParts(id string, parts []any, seq int64) (*http.Response, []byte) {
	h.t.Helper()
	return h.do("POST", "/session/"+id+"/enqueue", map[string]any{
		"parts": parts,
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

	t.Run("empty text", func(t *testing.T) {
		resp, data := h.enqueue(id, "", 1)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d: %s", resp.StatusCode, data)
		}
	})

	t.Run("whitespace-only text", func(t *testing.T) {
		resp, data := h.enqueue(id, "   \n\t", 1)
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

// TestEnqueueAcceptsImageBlobPart is the RED test for the feature this file
// exists to add: POST /session/{id}/enqueue accepts a `blob` part beside its
// text part, exactly like prompt_async's own
// TestPromptAsyncAcceptsImageBlobPart — same wire shape, same validation
// (decodePromptParts, shared verbatim), reused here for the durable,
// idempotent route. Before this change every enqueue body with a non-text
// part 400ed "v1 accepts text parts only"; that rejection is what boxes'
// pending-delivery drain hit for any attachment-bearing message against a
// box whose harness had not woken (or was already running) — the production
// 502 this branch fixes.
func TestEnqueueAcceptsImageBlobPart(t *testing.T) {
	prov := newCapturingProvider(asstTurn("red"))
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	pngBytes := testPNG(t)

	resp, data := h.enqueueParts(id, []any{
		map[string]any{"type": "text", "text": "what color is this?"},
		attachmentPart("image/png", pngBytes),
	}, 1)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("enqueue status %d: %s", resp.StatusCode, data)
	}
	var er enqueueResponse
	if err := json.Unmarshal(data, &er); err != nil {
		t.Fatal(err)
	}
	if er.Status != "started" || er.Watermark != 1 {
		t.Fatalf("enqueue response = %+v, want status=started watermark=1", er)
	}
	h.waitIdle(id)

	users := h.userMessages(id)
	if len(users) != 1 {
		t.Fatalf("user messages = %d, want 1: %+v", len(users), users)
	}
	if got := users[0].Parts.Text(); got != "what color is this?" {
		t.Errorf("transcript user text = %q, want the prompt text", got)
	}
	blobs := blobParts(users[0].Parts)
	if len(blobs) != 1 || blobs[0].MediaType != "image/png" || !bytes.Equal(blobs[0].Data, pngBytes) {
		t.Fatalf("transcript user blobs = %+v, want the uploaded image", blobs)
	}

	sent := blobParts(prov.lastUserParts(t))
	if len(sent) != 1 || !bytes.Equal(sent[0].Data, pngBytes) {
		t.Fatalf("provider request carried %d blob parts, want the uploaded image", len(sent))
	}
}

// TestEnqueueAcceptsBlobOnlyPrompt proves an attachment-only enqueue body (no
// text part) is accepted — an uploaded screenshot with nothing typed beside
// it is a real prompt, not an empty one, on the durable route exactly as it
// already is on prompt_async.
func TestEnqueueAcceptsBlobOnlyPrompt(t *testing.T) {
	prov := newCapturingProvider(asstTurn("ok"))
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	pngBytes := testPNG(t)

	resp, data := h.enqueueParts(id, []any{attachmentPart("image/png", pngBytes)}, 1)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("enqueue status %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	users := h.userMessages(id)
	if len(users) != 1 {
		t.Fatalf("user messages = %d, want 1", len(users))
	}
	if len(blobParts(users[0].Parts)) != 1 {
		t.Fatalf("user parts = %+v, want exactly the image blob", users[0].Parts)
	}
	if got := users[0].Parts.Text(); got != "" {
		t.Errorf("user text = %q, want empty for an attachment-only enqueue", got)
	}
}

// TestQueuedEnqueuePromptKeepsItsImage is the durability half of the
// feature, mirroring prompt_async's own TestQueuedPromptKeepsItsImage but
// through the durable/idempotent route: an image enqueued while the session
// is BUSY is durably queued (fsynced before the 202), and the attachment
// still rides along when the queue drains at the turn boundary — proving the
// blob survives the same busy-branch path boxes' pending-delivery drain
// exercises when a box is already running.
func TestQueuedEnqueuePromptKeepsItsImage(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	pngBytes := testPNG(t)

	// First prompt claims the run slot and parks inside the provider.
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]any{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.enqueueParts(id, []any{
		map[string]any{"type": "text", "text": "and this screenshot"},
		attachmentPart("image/png", pngBytes),
	}, 1)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("queued enqueue status %d: %s", resp.StatusCode, data)
	}
	var er enqueueResponse
	if err := json.Unmarshal(data, &er); err != nil {
		t.Fatal(err)
	}
	if er.Status != "queued" || er.Watermark != 1 {
		t.Fatalf("enqueue response = %+v, want status=queued watermark=1", er)
	}

	prov.releaseAll()
	h.waitIdle(id)

	users := h.userMessages(id)
	var withBlob int
	for _, m := range users {
		for _, b := range blobParts(m.Parts) {
			if bytes.Equal(b.Data, pngBytes) {
				withBlob++
			}
		}
	}
	if withBlob != 1 {
		t.Fatalf("user messages carrying the queued image = %d, want 1: %+v", withBlob, users)
	}
}

// TestEnqueueDuplicateSeqWithBlobIsNoOp proves the durability contract's seq
// idempotency extends to a blob-bearing prompt: a retried enqueue carrying
// the SAME seq (and the same attachment) must be a clean 200 duplicate
// no-op — not a second queue entry and not a second delivered attachment —
// exactly like a retried text-only enqueue already is
// (TestEnqueueBusyQueuesAndDeduplicates). The blob rides on its prompt's
// seq, not on a seq of its own.
func TestEnqueueDuplicateSeqWithBlobIsNoOp(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	pngBytes := testPNG(t)

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]any{{"type": "text", "text": "occupant"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("occupant prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	body := []any{
		map[string]any{"type": "text", "text": "with a picture"},
		attachmentPart("image/png", pngBytes),
	}
	resp, data = h.enqueueParts(id, body, 1)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first enqueue status %d: %s", resp.StatusCode, data)
	}

	// Same seq, same attachment: a clean duplicate no-op.
	resp, data = h.enqueueParts(id, body, 1)
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

	prov.releaseAll()
	h.waitIdle(id)

	users := h.userMessages(id)
	var withBlob int
	for _, m := range users {
		for _, b := range blobParts(m.Parts) {
			if bytes.Equal(b.Data, pngBytes) {
				withBlob++
			}
		}
	}
	if withBlob != 1 {
		t.Fatalf("user messages carrying the image = %d, want exactly 1 (a duplicate seq must not double-deliver)", withBlob)
	}
}

// TestEnqueueRejectsUnusableBlobPart proves the same validation caps
// prompt_async enforces (decodePromptParts, shared verbatim) apply to
// enqueue: an unsupported media type 400s, and — because rejection happens
// BEFORE any run slot is claimed or anything is durably accepted — the seq
// is NOT consumed, so a caller can retry the same seq with a fixed
// attachment.
func TestEnqueueRejectsUnusableBlobPart(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	resp, data := h.enqueueParts(id, []any{attachmentPart("text/plain", []byte("not an image"))}, 1)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", resp.StatusCode, data)
	}

	// The rejected attempt must not have consumed seq=1: GET /queue reports
	// watermark 0 (nothing durably accepted yet).
	resp, data = h.do("GET", "/session/"+id+"/queue", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET queue status %d: %s", resp.StatusCode, data)
	}
	var q queueGetResponse
	if err := json.Unmarshal(data, &q); err != nil {
		t.Fatal(err)
	}
	if q.Watermark != 0 {
		t.Fatalf("watermark = %d, want 0 (a rejected attachment must not advance the durable watermark)", q.Watermark)
	}

	// The same seq now succeeds with a usable attachment.
	resp, data = h.enqueueParts(id, []any{attachmentPart("image/png", testPNG(t))}, 1)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("retry with fixed attachment status %d, want 202: %s", resp.StatusCode, data)
	}
}

// TestEnqueueOversizeBlobRejected proves the single-attachment cap
// (promptAttachmentMaxBytes, the SAME constant prompt_async enforces) is
// reused rather than reimplemented for enqueue.
func TestEnqueueOversizeBlobRejected(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	oversized := bytes.Repeat([]byte("x"), promptAttachmentMaxBytes+1)
	resp, data := h.enqueueParts(id, []any{attachmentPart("image/png", oversized)}, 1)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", resp.StatusCode, data)
	}
	if !bytes.Contains(data, []byte("limit")) {
		t.Errorf("error = %s, want it to name the per-attachment limit", data)
	}
}

// TestEnqueueOversizeBodyRejectedBeforeDecode proves enqueue's body is
// bounded by the SAME promptRequestMaxBytes MaxBytesReader guard
// prompt_async uses (TestPromptAsyncOversizeBodyRejectedBeforeDecode) — two
// individually-legal attachments whose base64 encoding together exceeds the
// whole-body cap are rejected with 413 before decode even runs, not 400 from
// the per-attachment check.
func TestEnqueueOversizeBodyRejectedBeforeDecode(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	each := append(testPNG(t), bytes.Repeat([]byte("x"), promptAttachmentMaxBytes*2/3)...)
	if len(each) >= promptAttachmentMaxBytes {
		t.Fatalf("each attachment is %d bytes, which is not under the %d-byte per-attachment cap",
			len(each), promptAttachmentMaxBytes)
	}
	resp, data := h.enqueueParts(id, []any{
		attachmentPart("image/png", each),
		attachmentPart("image/png", each),
	}, 1)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413: %s", resp.StatusCode, data)
	}
	if !bytes.Contains(data, []byte("limit")) {
		t.Errorf("error = %s, want it to name the request limit", data)
	}
}
