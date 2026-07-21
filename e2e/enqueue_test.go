package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// enqueueRespJSON mirrors server.enqueueResponse (POST /session/{id}/enqueue's
// success body): the durable-enqueue watermark plus the delivery status. This
// package is black-box (HTTP only), so the wire shape is redeclared locally
// rather than importing the server package — same pattern as apiMessage and
// compaction_test.go's sessionCompactionJSON.
type enqueueRespJSON struct {
	Status    string `json:"status"` // "started" | "queued" | "duplicate"
	Watermark int64  `json:"watermark"`
	Queued    int    `json:"queued,omitempty"`
}

// queuedItemJSON mirrors server.queuedItemJSON: one pending (undelivered)
// entry in GET /session/{id}/queue's FIFO.
type queuedItemJSON struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
	Seq  int64  `json:"seq,omitempty"`
}

// queueGetRespJSON mirrors server.queueGetResponse.
type queueGetRespJSON struct {
	Watermark int64            `json:"watermark"`
	Queued    []queuedItemJSON `json:"queued"`
}

// enqueue posts POST /session/{id}/enqueue and returns the raw status code
// alongside the decoded body, so a test can assert the durability contract's
// status code (202 accepted vs. 200 duplicate) together with its JSON.
func (p *serveProc) enqueue(id, text string, seq int64) (int, enqueueRespJSON) {
	p.t.Helper()
	body := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": text}},
		"seq":   seq,
	}
	resp, data := p.do(http.MethodPost, "/session/"+id+"/enqueue", body)
	var er enqueueRespJSON
	if err := json.Unmarshal(data, &er); err != nil {
		p.t.Fatalf("decode enqueue response: %v (%s)", err, data)
	}
	return resp.StatusCode, er
}

// queueGet fetches GET /session/{id}/queue: the watermark plus every pending
// entry, in FIFO order.
func (p *serveProc) queueGet(id string) queueGetRespJSON {
	p.t.Helper()
	resp, data := p.do(http.MethodGet, "/session/"+id+"/queue", nil)
	if resp.StatusCode != http.StatusOK {
		p.t.Fatalf("get queue: status %d body %s", resp.StatusCode, data)
	}
	var q queueGetRespJSON
	if err := json.Unmarshal(data, &q); err != nil {
		p.t.Fatalf("decode queue: %v (%s)", err, data)
	}
	return q
}

// TestDurableEnqueueSurvivesSIGKILL is the black-box proof of durable
// enqueue's headline guarantee (docs/plans/2026-07-21-durable-enqueue.md): a
// 202 from POST /session/{id}/enqueue means the prompt.queued record is
// already fsynced, so an upstream that acks on that 202 may safely crash and
// retry into a successor process without double delivery — the
// accepted-but-undelivered window across a real process death.
//
// Sequence: occupy the session with a stalled turn, durably enqueue behind
// it (202, watermark 1), SIGKILL mid-turn, boot a fresh process on the same
// session dir, and prove over real HTTP against the successor that (a) the
// pending entry survived and refolded, (b) replaying the same seq is a clean
// 200 duplicate no-op, and (c) the successor still accepts new work.
func TestDurableEnqueueSurvivesSIGKILL(t *testing.T) {
	skipShort(t)

	fake := newFakeAnthropic(1) // stall the very first upstream request
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	t.Cleanup(fake.close)

	sessDir := t.TempDir()
	cfgPath := writeConfig(t, srv.URL)

	// Process 1: create a session and occupy it with a prompt that stalls
	// upstream, so the session is busy when the durable enqueue lands.
	p1 := startServe(t, sessDir, cfgPath)
	id := p1.createSession()
	p1.prompt(id, "occupant turn that stalls mid-stream")

	// Wait until the fake has streamed the first delta, so the enqueue below
	// (and the kill after it) land while the occupying turn is genuinely
	// in-flight, not racing its own start.
	select {
	case <-fake.firstDelta:
	case <-time.After(10 * time.Second):
		t.Fatalf("provider never streamed first delta\nstderr:\n%s", p1.stderr.String())
	}

	// Durable enqueue behind the busy occupant: the 202 attests the
	// prompt.queued record is fsynced BEFORE this response is written.
	status, er := p1.enqueue(id, "queued msg", 1)
	if status != http.StatusAccepted {
		t.Fatalf("enqueue status = %d, want 202", status)
	}
	if er.Status != "queued" || er.Watermark != 1 {
		t.Fatalf("enqueue response = %+v, want status=queued watermark=1", er)
	}

	// SIGKILL — no drain, no graceful shutdown. The occupying turn dies
	// mid-stream; the durable enqueue record must not.
	p1.kill()

	// Process 2: same session dir. Boot must reconcile the journal and
	// refold the pending queue entry.
	p2 := startServe(t, sessDir, cfgPath)

	// (a) The pending entry survived the kill and refolded: watermark 1, one
	// queued item carrying the same text and seq.
	q := p2.queueGet(id)
	if q.Watermark != 1 {
		t.Fatalf("post-restart watermark = %d, want 1", q.Watermark)
	}
	if len(q.Queued) != 1 {
		t.Fatalf("post-restart queued = %+v, want exactly 1 pending entry", q.Queued)
	}
	if q.Queued[0].Text != "queued msg" || q.Queued[0].Seq != 1 {
		t.Fatalf("post-restart queued[0] = %+v, want text=%q seq=1", q.Queued[0], "queued msg")
	}

	// (b) Replaying the same seq into the successor is a clean 200
	// duplicate no-op — the upstream that acked on process 1's 202 can
	// retry here without double delivery.
	status, er = p2.enqueue(id, "queued msg", 1)
	if status != http.StatusOK {
		t.Fatalf("duplicate-retry enqueue status = %d, want 200", status)
	}
	if er.Status != "duplicate" || er.Watermark != 1 {
		t.Fatalf("duplicate-retry enqueue response = %+v, want status=duplicate watermark=1", er)
	}

	// (c) The successor still accepts new work. Depending on whether the
	// duplicate retry's stranded-head drain (above) has already dispatched
	// the surviving entry, the session may be idle or busy at this instant,
	// so the status field can legitimately be "started" or "queued" — only
	// the status code and watermark are asserted.
	status, er = p2.enqueue(id, "m2", 2)
	if status != http.StatusAccepted {
		t.Fatalf("fresh enqueue status = %d, want 202", status)
	}
	if er.Watermark != 2 {
		t.Fatalf("fresh enqueue watermark = %d, want 2 (got status=%q)", er.Watermark, er.Status)
	}
}
