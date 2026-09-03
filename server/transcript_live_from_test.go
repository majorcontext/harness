package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestTranscriptLiveFrom_AtLeastMessageWatermark pins the contract §2 of
// docs/design/live-event-tip-cursor.md states: live_from is never below
// stream_from. It never sits below stream_from even immediately after
// session creation, before any message exists (stream_from is 0, since
// there is nothing to count; live_from can already be above 0, since
// createSession itself durably journals evtSessionCreated — a non-message
// record tipAtStart counts and stream_from never does). Once a turn
// completes, its own trailing session.status/turn.end records are
// journaled ABOVE its last message's seq too, so live_from is strictly
// greater than stream_from for this single-turn session with no
// fabricated backlog at all.
func TestTranscriptLiveFrom_AtLeastMessageWatermark(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("hello")}})
	id := h.createSession("")

	fresh, freshMeta := getTranscript(t, h, id)
	if freshMeta.status != 200 {
		t.Fatalf("GET stream_from (fresh) = %d: %s", freshMeta.status, freshMeta.body)
	}
	if fresh.LiveFrom < fresh.StreamFrom {
		t.Fatalf("brand-new session: live_from = %d, want >= stream_from %d", fresh.LiveFrom, fresh.StreamFrom)
	}
	if fresh.StreamFrom != 0 {
		t.Errorf("brand-new session with no messages: stream_from = %d, want 0", fresh.StreamFrom)
	}

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
	if got.LiveFrom < got.StreamFrom {
		t.Fatalf("live_from = %d, want >= stream_from %d", got.LiveFrom, got.StreamFrom)
	}
	if got.LiveFrom <= got.StreamFrom {
		t.Errorf("live_from = %d, stream_from = %d: want live_from strictly greater once a turn has journaled its own trailing status/turn.end records", got.LiveFrom, got.StreamFrom)
	}
}

// fabricateExcludedBacklog directly journals n durable evtMessage records
// for sessionID, under IDs that never appear in the session's own
// sess.History() — a stand-in for the measured production backlog
// (docs/design/live-event-tip-cursor.md §1): many durable records for one
// session that sit above its message watermark because
// transcriptWatermarkLocked only ever counts a message actually present in
// `messages`. It journals directly via emitDurableLocked (bypassing the
// harness's own Publish wiring, exactly like server/message_page_test.go's
// coldMessages bypasses it for a different reason) because reproducing the
// real claude-code-backend subagent-turn mechanism (harness#217) end to end
// would need a full nested-task-tool integration test; the mechanism this
// test actually exercises — a durable evtMessage for `sessionID` absent
// from `history` — is the exact, and only, shape transcriptWatermarkLocked
// discriminates on, regardless of why a record is absent.
func fabricateExcludedBacklog(t *testing.T, h *harness, sessionID string, n int, idPrefix string) {
	t.Helper()
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	for i := 0; i < n; i++ {
		m := message.Message{
			ID:   fmt.Sprintf("%s_%d", idPrefix, i),
			Role: message.RoleAssistant,
			Parts: message.Parts{
				&message.Text{Text: "nested subagent turn content"},
			},
		}
		h.srv.emitDurableLocked(&Event{Type: evtMessage, SessionID: sessionID, Message: &m})
	}
}

// readUntilSentinel reads events from sse one at a time until it sees an
// evtMessage event whose text contains sentinelText, then returns
// everything read so far (including the sentinel event itself).
//
// This is deliberately NOT sseStream.collectUntilIdle: a resume cursor set
// BELOW a prior turn's own trailing session.status idle (exactly what the
// OLD stream_from cursor is, by construction, whenever a fabricated or
// real backlog sits above it) puts that STALE idle event in the replay
// itself, ahead of anything this test actually wants to wait for —
// collectUntilIdle would stop there, never reaching the backlog between
// that stale idle and the sentinel turn. Reading for a specific message's
// own content has no such collision.
func readUntilSentinel(t *testing.T, sse *sseStream, sentinelText string) []Event {
	t.Helper()
	var out []Event
	for {
		ev := sse.nextEvent(t)
		out = append(out, ev)
		if ev.Type == evtMessage && ev.Message != nil && strings.Contains(ev.Message.Parts.Text(), sentinelText) {
			return out
		}
	}
}

// countMatchingMessageEvents reads via readUntilSentinel and returns how
// many collected evtMessage events carry an ID with the given prefix.
func countMatchingMessageEvents(t *testing.T, sse *sseStream, idPrefix, sentinelText string) int {
	t.Helper()
	evs := readUntilSentinel(t, sse, sentinelText)
	n := 0
	for _, ev := range evs {
		if ev.Type == evtMessage && ev.Message != nil && strings.HasPrefix(ev.Message.ID, idPrefix) {
			n++
		}
	}
	return n
}

// TestTranscriptLiveFrom_SkipsStaleBacklogButOldWatermarkDoesNot is the
// differential test docs/design/live-event-tip-cursor.md §5 describes: it
// proves both that the OLD cursor (stream_from) floods a resumed stream
// with a backlog absent from `messages`, and that the NEW cursor
// (live_from) does not — red-verifying the exact difference the new field
// exists to make, not merely asserting the new behavior in isolation.
func TestTranscriptLiveFrom_SkipsStaleBacklogButOldWatermarkDoesNot(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn("hello"), asstTurn("sentinel-old"), asstTurn("sentinel-new"),
	}})
	id := h.createSession("")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	const backlog = 50
	fabricateExcludedBacklog(t, h, id, backlog, "sub_msg")

	got, meta := getTranscript(t, h, id)
	if meta.status != 200 {
		t.Fatalf("GET stream_from = %d: %s", meta.status, meta.body)
	}
	if got.LiveFrom <= got.StreamFrom {
		t.Fatalf("live_from = %d, want strictly greater than stream_from %d (the fabricated backlog sits above the message watermark)", got.LiveFrom, got.StreamFrom)
	}

	// OLD behavior, red-verified: resuming from stream_from replays the
	// whole backlog.
	oldSSE := h.openSSE(fmt.Sprintf("?from=%d&session=%s", got.StreamFrom, id), "")
	resp2, data2 := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go again"}},
	})
	if resp2.StatusCode != 202 {
		t.Fatalf("prompt_async (sentinel) = %d: %s", resp2.StatusCode, data2)
	}
	h.waitIdle(id)
	if n := countMatchingMessageEvents(t, oldSSE, "sub_msg", "sentinel-old"); n != backlog {
		t.Errorf("resuming from OLD stream_from replayed %d of the %d fabricated backlog messages, want all %d (this pins today's bug)", n, backlog, backlog)
	}

	// NEW behavior: resuming from live_from replays none of it. The
	// session is already idle at this point (the sentinel turn above
	// already completed), so open a SECOND sentinel turn to give this
	// stream its own terminal marker.
	newSSE := h.openSSE(fmt.Sprintf("?from=%d&session=%s", got.LiveFrom, id), "")
	resp3, data3 := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go a third time"}},
	})
	if resp3.StatusCode != 202 {
		t.Fatalf("prompt_async (sentinel 2) = %d: %s", resp3.StatusCode, data3)
	}
	h.waitIdle(id)
	if n := countMatchingMessageEvents(t, newSSE, "sub_msg", "sentinel-new"); n != 0 {
		t.Errorf("resuming from NEW live_from replayed %d of the %d fabricated backlog messages, want 0", n, backlog)
	}
}

// TestTranscriptLiveFrom_RealSessionNoBacklogAfterBootstrap proves the same
// "no backlog, no gap" property against a real, entirely non-fabricated
// event stream: two real turns before the bootstrap read, one real turn
// after it, resuming from live_from. Every event the client sees must
// belong to the AFTER turn; nothing from the two BEFORE turns may reappear.
func TestTranscriptLiveFrom_RealSessionNoBacklogAfterBootstrap(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn("before one"), asstTurn("before two"), asstTurn("after"),
	}})
	id := h.createSession("")

	for _, text := range []string{"go 1", "go 2"} {
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": text}},
		})
		if resp.StatusCode != 202 {
			t.Fatalf("prompt_async(%q) = %d: %s", text, resp.StatusCode, data)
		}
		h.waitIdle(id)
	}

	got, meta := getTranscript(t, h, id)
	if meta.status != 200 {
		t.Fatalf("GET stream_from = %d: %s", meta.status, meta.body)
	}

	sse := h.openSSE(fmt.Sprintf("?from=%d&session=%s", got.LiveFrom, id), "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go 3"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async(after) = %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	evs := sse.collectUntilIdle(t)
	sawAfter := false
	for _, ev := range evs {
		if ev.Type != evtMessage || ev.Message == nil {
			continue
		}
		if strings.Contains(ev.Message.Parts.Text(), "before") {
			t.Fatalf("live stream at live_from replayed a before-bootstrap message: %+v", ev.Message)
		}
		if strings.Contains(ev.Message.Parts.Text(), "after") {
			sawAfter = true
		}
	}
	if !sawAfter {
		t.Fatalf("live stream at live_from never delivered the after-bootstrap message: %+v", evs)
	}
}

// TestTranscriptLiveFrom_NoGapConcurrentRace pins the race-close argument
// of docs/design/live-event-tip-cursor.md §4: a message that races into
// the documented gap between transcriptSyncedThrough's unlocked
// sess.History() read and its s.mu.Lock() — the exact interleaving
// TestTranscriptStreamFrom_ConcurrentJournalDuringSnapshot already forces
// for stream_from — must still be strictly above live_from, and must
// still actually arrive over a real SSE connection resumed at live_from.
//
// Red-verify: a naive live_from := s.seq sampled only at the END of
// transcriptSyncedThrough's locked section (no tipAtStart) fails this
// test, because the raced message's own emitDurableLocked call completes
// (and so is already counted in s.seq) before this call's own lock
// section even begins — see §4's full argument for why tipAtStart, read
// BEFORE sess.History(), is what keeps live_from below the raced
// message's seq.
func TestTranscriptLiveFrom_NoGapConcurrentRace(t *testing.T) {
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

	seqs := journalSeqByMessageID(h.srv, id)
	racedSeq, journaled := seqs[racedID]
	if !journaled {
		t.Fatalf("raced message %s was never journaled", racedID)
	}
	if racedSeq <= got.LiveFrom {
		t.Fatalf("no-gap violated: raced message %s has seq %d <= live_from %d", racedID, racedSeq, got.LiveFrom)
	}

	// The raced message (and nothing else) is already durably journaled
	// for this session above live_from — read directly from the journal,
	// under lock, exactly what handleEvent's own replay snapshot will see
	// at registration, so this test knows exactly how many events to read
	// off the wire below without an open-ended, indefinitely-blocking
	// read: nothing else journals for this session after this point in
	// the test, and the raced turn (driven directly through sess.Prompt,
	// bypassing the server's own busy/idle wrapping) never produces a
	// session.status idle to key an unbounded read on.
	want := journalEventsAbove(h, id, got.LiveFrom)
	if len(want) == 0 {
		t.Fatalf("no durable events above live_from %d for session %s; expected at least the raced message", got.LiveFrom, id)
	}

	sse := h.openSSE(fmt.Sprintf("?from=%d&session=%s", got.LiveFrom, id), "")
	sawRaced := false
	var evs []Event
	for i := 0; i < len(want); i++ {
		ev := sse.nextEvent(t)
		evs = append(evs, ev)
		if ev.Type == evtMessage && ev.Message != nil && ev.Message.ID == racedID {
			sawRaced = true
		}
	}
	if !sawRaced {
		t.Fatalf("raced message %s never arrived resuming from live_from %d: %+v", racedID, got.LiveFrom, evs)
	}
}

// journalEventsAbove reads directly from h.srv's durable journal, under
// lock, every record for sessionID with seq > from — the exact query
// handleEvent's own replay snapshot runs at SSE registration time.
func journalEventsAbove(h *harness, sessionID string, from int64) []Event {
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	var out []Event
	for _, ev := range h.srv.journal {
		if ev.SessionID == sessionID && ev.Seq > from {
			out = append(out, ev)
		}
	}
	return out
}

// TestEventReplayFromEarlierSeq_UnaffectedByLiveFrom is the mirror/replay
// regression pin from docs/design/live-event-tip-cursor.md §3: a consumer
// that resumes /event from an EARLIER seq than any bootstrap cursor — the
// console-read-path mirror's own pattern, replaying a full authoritative
// window — must still see every durable record above that seq, unfiltered,
// regardless of where live_from for the same session happens to land.
func TestEventReplayFromEarlierSeq_UnaffectedByLiveFrom(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("one"), asstTurn("two")}})
	id := h.createSession("")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go 1"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	got, meta := getTranscript(t, h, id)
	if meta.status != 200 {
		t.Fatalf("GET stream_from = %d: %s", meta.status, meta.body)
	}

	// Resume from BEFORE this session ever existed (seq 0) — the mirror's
	// own full-replay shape — and confirm the whole session's message
	// history (both messages so far) still arrives, unaffected by
	// live_from's existence or value.
	sse := h.openSSE(fmt.Sprintf("?from=0&session=%s", id), "")
	resp2, data2 := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go 2"}},
	})
	if resp2.StatusCode != 202 {
		t.Fatalf("prompt_async (2) = %d: %s", resp2.StatusCode, data2)
	}
	h.waitIdle(id)

	evs := readUntilSentinel(t, sse, "two")
	sawOne := false
	for _, ev := range evs {
		if ev.Type == evtMessage && ev.Message != nil && strings.Contains(ev.Message.Parts.Text(), "one") {
			sawOne = true
		}
	}
	if !sawOne {
		t.Fatalf("full replay from seq 0 never delivered the first turn's assistant reply (\"one\", already durable before this SSE connection opened): %+v (live_from was %d)", evs, got.LiveFrom)
	}
}
