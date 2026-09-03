package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// transcriptResponse mirrors transcriptJSON for a test that decodes it as a
// client would, without reaching into the server's own type for anything
// but the field names.
type transcriptResponse struct {
	Messages   []message.Message `json:"messages"`
	StreamFrom int64             `json:"stream_from"`
	LiveFrom   int64             `json:"live_from"`
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
	// Deterministic, not a coin flip: the seam runs synchronously, in this
	// same goroutine, strictly after transcriptSyncedThrough's own
	// sess.History() call returned (that call already produced the
	// `history` slice this request will return) and strictly before its
	// s.mu.Lock() — so the raced message can never be in the snapshot this
	// specific request answers with.
	if inHistory {
		t.Fatalf("raced message %s unexpectedly present in the snapshot — the seam did not run in the documented gap", racedID)
	}

	seqs := journalSeqByMessageID(h.srv, id)
	racedSeq, journaled := seqs[racedID]
	if !journaled {
		t.Fatalf("raced message %s was never journaled", racedID)
	}
	if racedSeq <= got.StreamFrom {
		t.Errorf("gap-safety invariant violated: raced message absent from snapshot yet seq %d <= stream_from %d", racedSeq, got.StreamFrom)
	}
}

// TestTranscriptStreamFrom_CompactionDuringSnapshotStaysRecoverable is the
// regression test for the sharper version of the race above:
// engine/compact.go's Session.Compact does not merely APPEND to history, it
// SPLICES a new summary message into an EARLIER array position (replacing
// the folded range) and then journals the result in array order — so the
// summary can receive a LOWER seq than an already-later message this call's
// own stale snapshot already contains, even though the summary was created
// after that snapshot was taken. transcriptWatermarkLocked's plain
// "restrict to message IDs in history" rule alone is not enough here: it
// would let the summary's lower seq slip below the watermark while the
// summary sits outside history, and unlike an ordinary excluded message
// (which self-heals by arriving live later), a summary in that state is
// permanently unrecoverable — its paired history.compacted record would
// never redeliver either, since the client's SSE resume point already sits
// at or past it.
//
// This builds a session directly (bypassing the harness's own Publish
// wiring, like coldMessages) so the server has never synced a byte of it —
// exactly TestTranscriptStreamFrom_ConsistentWithSnapshot's setup — then
// races a REAL POST /session/{id}/compact into the transcriptSyncRace gap.
// Compaction runs against its own freshly-loaded *engine.Session (the cold
// path loads a new one via claimForPrompt), so it never touches this call's
// own already-captured `history`; the two only ever meet through the
// server's shared journal and s.mu.
func TestTranscriptStreamFrom_CompactionDuringSnapshotStaysRecoverable(t *testing.T) {
	dir := t.TempDir()
	// The harness's OWN provider only needs the compaction summarization
	// call queued — the three turns below are written directly to disk by
	// a throwaway session/provider pair, exactly like coldMessages, so the
	// harness never sees them until the race below touches this session.
	harnessProv := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("SUMMARY", provider.Usage{InputTokens: 5}),
	}}
	h := newHarnessDir(t, dir, harnessProv)

	seedProv := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("one", provider.Usage{InputTokens: 10}),
		compactAsstTurn("two", provider.Usage{InputTokens: 20}),
		compactAsstTurn("three", provider.Usage{InputTokens: 30}),
	}}
	seed := engine.NewSession(engine.Config{
		Providers:  provider.Registry{seedProv.name: seedProv},
		Model:      message.ModelRef{Provider: seedProv.name, Model: "m1"},
		SessionDir: dir,
		WorkDir:    dir,
	})
	for i, text := range []string{"go1", "go2", "go3"} {
		if _, err := seed.Prompt(context.Background(), text); err != nil {
			t.Fatalf("seed Prompt %d: %v", i, err)
		}
	}
	if err := seed.PersistErr(); err != nil {
		t.Fatalf("seed PersistErr: %v", err)
	}

	h.srv.transcriptSyncRace = func() {
		resp, data := h.do("POST", "/session/"+seed.ID+"/compact", map[string]any{"keep_turns": 1})
		if resp.StatusCode != 200 {
			t.Errorf("transcriptSyncRace: compact = %d: %s", resp.StatusCode, data)
		}
	}
	t.Cleanup(func() { h.srv.transcriptSyncRace = nil })

	got, meta := getTranscript(t, h, seed.ID)
	if meta.status != 200 {
		t.Fatalf("GET stream_from = %d: %s", meta.status, meta.body)
	}

	// Find the summary: the one journaled evtHistoryCompacted record for
	// this session names it.
	h.srv.mu.Lock()
	var summaryID string
	for _, ev := range h.srv.journal {
		if ev.Type == evtHistoryCompacted && ev.SessionID == seed.ID {
			summaryID = ev.CompactSummaryID
		}
	}
	h.srv.mu.Unlock()
	if summaryID == "" {
		t.Fatal("transcriptSyncRace never journaled a history.compacted record")
	}

	inHistory := false
	for _, m := range got.Messages {
		if m.ID == summaryID {
			inHistory = true
		}
	}

	seqs := journalSeqByMessageID(h.srv, seed.ID)
	summarySeq, journaled := seqs[summaryID]
	if !journaled {
		t.Fatalf("summary %s was never journaled", summaryID)
	}

	// The gap-safety invariant, exactly as the plain-message race test
	// checks it: the summary must be in BOTH history and seq <=
	// stream_from, or in NEITHER — never seq <= stream_from while absent.
	switch {
	case inHistory && summarySeq <= got.StreamFrom:
	case !inHistory && summarySeq > got.StreamFrom:
	default:
		t.Errorf("gap-safety invariant violated for compaction summary: in history=%v, seq=%d, stream_from=%d",
			inHistory, summarySeq, got.StreamFrom)
	}
}

// TestTranscriptWatermarkLocked_CompactionSummarySandwich is the regression
// test for the narrower race TestTranscriptStreamFrom_CompactionDuringSnapshotStaysRecoverable
// above cannot reach: that test's seam runs the whole POST /compact
// synchronously, so by the time the racing GET observes anything, BOTH the
// summary's evtMessage record and its paired evtHistoryCompacted record are
// already journaled — pendingCeilings always has something to cap on. In
// production the two are journaled in separate Publish calls (separate s.mu
// sections; see engine/compact.go's Compact and transcriptWatermarkLocked's
// own doc comment), so a bootstrap read can land in the gap between them:
// summary evtMessage present, evtHistoryCompacted absent. There is no
// existing seam that stalls a real compaction between its two Publish
// calls, so this constructs that exact journal state directly — via the
// same emitDurableLocked production code path syncMessages itself uses, not
// hand-rolled Event literals — and calls transcriptWatermarkLocked
// directly, the narrowest production-faithful entry point for a state that
// has no HTTP-level trigger yet.
func TestTranscriptWatermarkLocked_CompactionSummarySandwich(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	const sessionID = "sess_sandwich"

	// An ordinary message already in history, journaled before the summary.
	first := message.Message{ID: "msg_first", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "first"}}}
	// The compaction summary: a real cmpsum_-prefixed ID (engine.IsCompactionSummaryID
	// gates on exactly this prefix — see compactionSummaryIDTag), journaled as
	// a plain evtMessage. It is EXCLUDED from history below (compaction
	// spliced it in, replacing the folded range) and — this is the sandwich —
	// its evtHistoryCompacted record has NOT been journaled yet.
	summary := message.Message{ID: "cmpsum_test", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: engine.CompactionSummaryBanner + "summary"}}}
	// A stale-history message journaled AFTER the summary, in the gap — the
	// exact shape that pushes `highest` past the summary's own seq when
	// nothing caps it.
	stale := message.Message{ID: "msg_stale", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "stale"}}}

	firstEv := &Event{Type: evtMessage, SessionID: sessionID, Message: &first}
	summaryEv := &Event{Type: evtMessage, SessionID: sessionID, Message: &summary}
	staleEv := &Event{Type: evtMessage, SessionID: sessionID, Message: &stale}

	h.srv.mu.Lock()
	h.srv.emitDurableLocked(firstEv)
	h.srv.emitDurableLocked(summaryEv)
	h.srv.emitDurableLocked(staleEv)
	// Deliberately no evtHistoryCompacted record: this is the gap between
	// compaction's two separate emits, before the second one lands.
	got := h.srv.transcriptWatermarkLocked(sessionID, []message.Message{first, stale})
	h.srv.mu.Unlock()

	if staleEv.Seq <= summaryEv.Seq {
		t.Fatalf("test setup invariant broken: stale seq %d must be > summary seq %d", staleEv.Seq, summaryEv.Seq)
	}

	want := summaryEv.Seq - 1
	if got != want {
		t.Errorf("transcriptWatermarkLocked = %d, want %d (summary %s's own seq %d minus one) — "+
			"a summary absent from history must cap the watermark even with no evtHistoryCompacted "+
			"record yet, or its future history.compacted record becomes an unrecoverable dangling reference",
			got, want, summary.ID, summaryEv.Seq)
	}
}

// TestTranscriptStreamFrom_EmptyHistoryReportsZero pins the deliberate
// choice for a session with nothing journaled yet: stream_from is 0, not
// some higher "current instant" value (e.g. the server's global seq
// counter). A higher fallback would reopen exactly the race
// transcriptWatermarkLocked exists to close, for the session's own FIRST
// message: if a message were mid-race-journaled by someone else between
// this call's unlocked reads and its lock, a global-counter fallback would
// already count it even though it is (trivially, history is empty) absent
// from `messages`. 0 has nothing to protect and nothing to straddle: every
// message that will ever exist for this session gets seq >= 1 > 0.
func TestTranscriptStreamFrom_EmptyHistoryReportsZero(t *testing.T) {
	// Drive unrelated activity first so the process-wide seq counter is
	// already well above 0 — proving 0 is a deliberate per-session answer,
	// not just "nothing has happened in this process yet."
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("noise")}}
	h := newHarness(t, prov)
	other := h.createSession("")
	resp, data := h.do("POST", "/session/"+other+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(other)

	id := h.createSession("")
	got, meta := getTranscript(t, h, id)
	if meta.status != 200 {
		t.Fatalf("GET stream_from = %d: %s", meta.status, meta.body)
	}
	if len(got.Messages) != 0 {
		t.Fatalf("got %d messages, want 0", len(got.Messages))
	}
	if got.StreamFrom != 0 {
		t.Errorf("stream_from = %d, want 0 for an empty transcript", got.StreamFrom)
	}
}

// TestTranscriptStreamFrom_RejectsCombinationWithPaging: stream_from names
// a different response envelope than before_seq/limit. Answering one
// silently (handleMessages used to let before_seq/limit win, discarding
// stream_from) hides that the caller named two incompatible intentions —
// intParam enforces the identical rule against a repeated before_seq or
// limit value for the same reason.
func TestTranscriptStreamFrom_RejectsCombinationWithPaging(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("")

	for _, query := range []string{"?stream_from=1&before_seq=5", "?stream_from=1&limit=5"} {
		resp, data := h.do("GET", "/session/"+id+"/message"+query, nil)
		if resp.StatusCode != 400 {
			t.Errorf("GET message%s = %d, want 400: %s", query, resp.StatusCode, data)
		}
	}
}

// TestTranscriptStreamFrom_SyntheticOrphanRepairNeverJournaled: a
// message.ResolveOrphanToolCalls repair (LoadSession's load-time patch for
// an assistant tool_call with no matching tool_result anywhere a provider's
// wire protocol requires one — see message.IsSyntheticOrphanID) exists only
// in this process's in-memory history; it is never itself persisted to the
// session's own log (see engine/store.go's LoadSession). It must still
// appear in the returned `messages` — this endpoint mirrors the
// unparameterized bare-array shape, which already includes it, unlike the
// before_seq/limit page (handleMessagePage's own doc comment: a page
// "never adds the load-time repair," reading verbatim from the durable log
// instead) — but it must never receive a durable seq: durableOnly
// (handlers.go) already enforces "a page must never give one a seq,
// whichever path produced the page" for the identical reason, and nothing
// backs its "seen" mark across a restart, since LoadSession re-derives it
// fresh on every load rather than replaying it from events.jsonl.
//
// The fixture file is written to disk AFTER the harness boots (like
// TestTranscriptStreamFrom_ConsistentWithSnapshot), not before: writing it
// first would let boot-time reconcile() — a separate, PRE-EXISTING loop
// with this exact same characteristic, unrelated to this change — journal
// the repair before transcriptSyncedThrough ever runs, which would pass or
// fail this test on reconcile()'s behavior instead of the code this test
// exists to cover.
func TestTranscriptStreamFrom_SyntheticOrphanRepairNeverJournaled(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	id := "ses_5292000000000099"
	// msg_2 is an assistant tool_call with no following tool-role result —
	// the exact orphan shape message.ResolveOrphanToolCalls repairs, mirrors
	// engine/compact_test.go's nep5292FixtureLines.
	fixture := `{"type":"session","id":"` + id + `","created_at":"2025-01-02T03:04:05Z"}
{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"task 1"}]}}
{"type":"message","message":{"id":"msg_2","role":"assistant","parts":[{"type":"tool_call","call_id":"A","name":"bash","arguments":{}}]}}
{"type":"message","message":{"id":"msg_3","role":"user","parts":[{"type":"text","text":"task 2"}]}}
{"type":"message","message":{"id":"msg_4","role":"assistant","parts":[{"type":"text","text":"done"}]}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	got, meta := getTranscript(t, h, id)
	if meta.status != 200 {
		t.Fatalf("GET stream_from = %d: %s", meta.status, meta.body)
	}
	if len(got.Messages) != 5 {
		t.Fatalf("got %d messages, want 5 (4 raw + 1 synthetic repair)", len(got.Messages))
	}
	var orphanID string
	for _, m := range got.Messages {
		if message.IsSyntheticOrphanID(m.ID) {
			orphanID = m.ID
		}
	}
	if orphanID == "" {
		t.Fatal("no synthetic orphan-repair message in the returned messages — fixture did not trigger the repair")
	}

	seqs := journalSeqByMessageID(h.srv, id)
	if _, journaled := seqs[orphanID]; journaled {
		t.Errorf("synthetic orphan-repair message %s was journaled with a durable seq — it has no durable identity to journal against", orphanID)
	}
}

// TestTranscriptStreamFromUnknownSessionIsNotFound: an id with no journal
// and no live session is a 404, exactly like the bare-array and
// before_seq/limit branches already report it.
func TestTranscriptStreamFromUnknownSessionIsNotFound(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, _ := h.do("GET", "/session/ses_0123456789abcdef/message?stream_from=1", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("GET unknown session stream_from = %d, want 404", resp.StatusCode)
	}
}
