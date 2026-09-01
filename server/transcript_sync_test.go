package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// transcriptResponse mirrors transcriptJSON for a test that decodes it as a
// client would, without reaching into the server's own type for anything
// but the field names.
type transcriptResponse struct {
	Messages   []message.Message `json:"messages"`
	StreamFrom int64             `json:"stream_from"`
}

// getTranscript issues GET /session/{id}/message?stream_from=1 and decodes
// the transcriptJSON envelope.
func getTranscript(t *testing.T, h *harness, id string) (transcriptResponse, *responseMeta) {
	t.Helper()
	resp, data := h.do("GET", "/session/"+id+"/message?stream_from=1", nil)
	meta := &responseMeta{status: resp.StatusCode, body: data}
	if resp.StatusCode != 200 {
		return transcriptResponse{}, meta
	}
	var got transcriptResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode transcript: %v (%s)", err, data)
	}
	return got, meta
}

type responseMeta struct {
	status int
	body   []byte
}

// journalSeqByMessageID snapshots every evtMessage record's seq for
// sessionID, keyed by message ID, directly from the server's own durable
// journal. Caller must not hold s.mu.
func journalSeqByMessageID(s *Server, sessionID string) map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64)
	for _, ev := range s.journal {
		if ev.Type == evtMessage && ev.SessionID == sessionID && ev.Message != nil {
			out[ev.Message.ID] = ev.Seq
		}
	}
	return out
}

// TestTranscriptStreamFrom_ConsistentWithSnapshot is the endpoint's core
// promise: every message the snapshot returns is durably journaled with a
// seq no greater than the returned stream_from, and the journaling happens
// IN THIS REQUEST for a session nothing in this process had synced yet
// (mirrors the "spawned child never touched again" shape syncMessages' own
// doc comment describes) — proving transcriptSyncedThrough, not some
// earlier boot-time reconcile pass, is what produced these seqs.
func TestTranscriptStreamFrom_ConsistentWithSnapshot(t *testing.T) {
	dir := t.TempDir()
	// Build the server FIRST, against an empty dir, so its boot-time
	// reconcile() finds nothing. The session below is written to the same
	// dir only afterward — this process has never journaled a single byte
	// of it before the GET below.
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})
	sess := coldMessages(t, dir, 3) // 6 messages

	h.srv.mu.Lock()
	preSeq := h.srv.sessionSeqLocked(sess.ID)
	h.srv.mu.Unlock()
	if preSeq != 0 {
		t.Fatalf("preSeq = %d, want 0 (nothing journaled for this session yet)", preSeq)
	}

	got, meta := getTranscript(t, h, sess.ID)
	if meta.status != 200 {
		t.Fatalf("GET stream_from = %d: %s", meta.status, meta.body)
	}
	if len(got.Messages) != 6 {
		t.Fatalf("got %d messages, want 6", len(got.Messages))
	}

	seqs := journalSeqByMessageID(h.srv, sess.ID)
	for _, m := range got.Messages {
		seq, ok := seqs[m.ID]
		if !ok {
			t.Errorf("message %s: not found in s.journal", m.ID)
			continue
		}
		if seq > got.StreamFrom {
			t.Errorf("message %s: journaled seq %d > stream_from %d", m.ID, seq, got.StreamFrom)
		}
		if seq <= preSeq {
			t.Errorf("message %s: journaled seq %d <= pre-call watermark %d, want strictly greater (proves in-request journaling)", m.ID, seq, preSeq)
		}
	}
	if got.StreamFrom <= preSeq {
		t.Errorf("stream_from = %d, want strictly greater than pre-call watermark %d", got.StreamFrom, preSeq)
	}
}

// TestTranscriptStreamFrom_LaterMessageHasSeqAboveWatermark: a message
// journaled AFTER the snapshot call returns must have a seq strictly
// greater than stream_from — the property that makes GET
// /event?from=<stream_from> a safe, gap-free resume point.
func TestTranscriptStreamFrom_LaterMessageHasSeqAboveWatermark(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("first"), asstTurn("second")}}
	h := newHarness(t, prov)
	id := h.createSession("")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	got, meta := getTranscript(t, h, id)
	if meta.status != 200 {
		t.Fatalf("GET stream_from = %d: %s", meta.status, meta.body)
	}

	sess := h.srv.residentSession(id)
	if sess == nil {
		t.Fatal("session not resident")
	}
	if _, err := sess.Prompt(context.Background(), "second"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	h.srv.mu.Lock()
	newSeq := h.srv.sessionSeqLocked(id)
	h.srv.mu.Unlock()
	if newSeq <= got.StreamFrom {
		t.Errorf("seq after later message = %d, want strictly greater than stream_from %d", newSeq, got.StreamFrom)
	}
}

// TestTranscriptStreamFrom_ConcurrentJournalDuringSnapshot forces a message
// to be fully journaled by a CONCURRENT Publish(EventMessage) call landing
// in the gap between transcriptSyncedThrough's unlocked sess.History() read
// and its s.mu.Lock() — via the transcriptSyncRace seam, the same pattern
// TestHandleEventDeliversEventPublishedBeforeHeadersFlush uses for
// handleEvent's own registration gap.
//
// The racing message is appended (and journaled, via a real second Prompt
// call) strictly AFTER this call's own sess.History() snapshot was taken,
// so it can never appear in the returned history — the gap-safety invariant
// requires it to land on the "NEITHER" side: absent from history AND its
// seq strictly greater than the returned stream_from. A naive watermark
// (the plain highest seq journaled anywhere for the session, unconditional
// on message identity) would instead already count the raced message's
// seq — this is the exact case transcriptWatermarkLocked's restriction to
// message IDs present in history exists to rule out.
func TestTranscriptStreamFrom_ConcurrentJournalDuringSnapshot(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("first"), asstTurn("raced")}}
	h := newHarness(t, prov)
	id := h.createSession("")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	var racedID string
	h.srv.transcriptSyncRace = func() {
		sess := h.srv.residentSession(id)
		if sess == nil {
			t.Error("transcriptSyncRace: session not resident")
			return
		}
		asst, err := sess.Prompt(context.Background(), "trigger raced turn")
		if err != nil {
			t.Errorf("transcriptSyncRace: Prompt: %v", err)
			return
		}
		racedID = asst.ID
	}
	t.Cleanup(func() { h.srv.transcriptSyncRace = nil })

	got, meta := getTranscript(t, h, id)
	if meta.status != 200 {
		t.Fatalf("GET stream_from = %d: %s", meta.status, meta.body)
	}
	if racedID == "" {
		t.Fatal("transcriptSyncRace never ran")
	}

	inHistory := false
	for _, m := range got.Messages {
		if m.ID == racedID {
			inHistory = true
		}
	}

	seqs := journalSeqByMessageID(h.srv, id)
	racedSeq, journaled := seqs[racedID]
	if !journaled {
		t.Fatalf("raced message %s was never journaled", racedID)
	}

	switch {
	case inHistory && racedSeq <= got.StreamFrom:
		// Safe: fully accounted for in the snapshot.
	case !inHistory && racedSeq > got.StreamFrom:
		// Safe: absent from the snapshot, but resuming from stream_from
		// will still deliver it live (sse.go: ev.Seq > from).
	default:
		t.Errorf("gap-safety invariant violated: raced message in history=%v, seq=%d, stream_from=%d",
			inHistory, racedSeq, got.StreamFrom)
	}
}
