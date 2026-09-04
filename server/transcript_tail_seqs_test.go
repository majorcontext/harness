package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestTranscriptSeqs_ParallelToMessages pins the contract
// docs/design/transcript-tail-seqs.md states: GET
// /session/{id}/message?stream_from=1 answers a `seqs` array, parallel to
// `messages`, each entry's DURABLE MESSAGE ORDINAL -- a plain 1-based count
// (1, 2, 3, ...) over this session's own message records, the SAME
// numbering before_seq/limit paging uses. It is NOT the box-global
// event-journal seq stream_from/live_from report, which runs ahead of it
// (see TestTranscriptSeqs_PagesAdjacentToRealBeforeSeq for why that
// distinction is load-bearing).
//
// Before this change transcriptJSON carried no such field at all, so a
// client decoding it never saw `seqs` — this test's failure mode without
// the fix is exactly that: len(got.Seqs) == 0 for a session with three
// messages.
func TestTranscriptSeqs_ParallelToMessages(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn("one"), asstTurn("two"), asstTurn("three"),
	}})
	id := h.createSession("")

	for i := 0; i < 3; i++ {
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": "go"}},
		})
		if resp.StatusCode != 202 {
			t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
		}
		h.waitIdle(id)
	}

	got, meta := getTranscript(t, h, id)
	if meta.status != 200 {
		t.Fatalf("GET transcript = %d: %s", meta.status, meta.body)
	}

	// One user + one assistant message per turn: 6 total.
	if len(got.Messages) != 6 {
		t.Fatalf("len(messages) = %d, want 6", len(got.Messages))
	}
	if len(got.Seqs) != len(got.Messages) {
		t.Fatalf("len(seqs) = %d, want %d (parallel to messages)", len(got.Seqs), len(got.Messages))
	}
	// The durable message ordinal is a plain 1-based count over this
	// session's own messages -- exactly [1, 2, 3, 4, 5, 6] here, not
	// merely "positive and increasing" (which the box-global journal seq
	// this field must NOT be would also satisfy).
	for i, seq := range got.Seqs {
		want := int64(i + 1)
		if seq != want {
			t.Errorf("seqs[%d] = %d, want %d (the 1-based durable message ordinal, message %q)", i, seq, want, got.Messages[i].ID)
		}
	}
}

// TestTranscriptSeqs_PagesAdjacentToRealBeforeSeq is the test the reported
// wrong-coordinate defect needed and did not have: it takes a `seqs` value
// from the ?stream_from=1 envelope and feeds it to the REAL before_seq/
// limit page endpoint (GET /session/{id}/message?before_seq=N&limit=K,
// server/handlers.go's handleMessagePage -> engine.ReadMessagePage), the
// exact use meetneptune/boxes's console pane makes of it
// (docs/design/transcript-tail-seqs.md, transcript-window.ts's
// loadTranscriptTail).
//
// The session runs enough turns that the box-global event-journal seq
// (what an earlier, wrong revision of messageDurableOrdinals sampled --
// s.seq via s.journal/emitDurableLocked) provably DIVERGES from the
// per-session durable message ordinal before_seq is actually defined in
// terms of: each turn journals its own evtSessionStatus busy/idle
// transition (and this scripted provider's turns also drive an evtModel
// record on the first one), so the journal seq runs ahead of the message
// count by more than one per turn. A test that only asserted seqs was
// monotonic/positive (this file's previous, incomplete version) could not
// catch a value from the WRONG monotonic per-message seq space; this one
// can, because it checks the value against the one contract before_seq
// actually has to honor: paging directly beneath it must be gap-free and
// overlap-free.
func TestTranscriptSeqs_PagesAdjacentToRealBeforeSeq(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn("one"), asstTurn("two"), asstTurn("three"), asstTurn("four"),
	}})
	id := h.createSession("")

	for i := 0; i < 4; i++ {
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": "go"}},
		})
		if resp.StatusCode != 202 {
			t.Fatalf("prompt_async = %d: %s", resp.StatusCode, data)
		}
		h.waitIdle(id)
	}

	got, meta := getTranscript(t, h, id)
	if meta.status != 200 {
		t.Fatalf("GET transcript = %d: %s", meta.status, meta.body)
	}
	if len(got.Messages) != 8 {
		t.Fatalf("len(messages) = %d, want 8 (4 turns)", len(got.Messages))
	}

	// Prove the two seq spaces actually diverged for this session -- a
	// pin that fails LOUDLY (not just "the anchor test below happened not
	// to catch it") if a future change to turn bookkeeping stops
	// journaling the extra per-turn events this test depends on to tell
	// the two spaces apart.
	messageOrdinalOfLast := int64(len(got.Messages))
	if got.StreamFrom <= messageOrdinalOfLast {
		t.Fatalf("test setup invalid: stream_from (box-global journal seq) = %d did not exceed the message ordinal %d -- this session needs more per-turn non-message journal activity to distinguish the two seq spaces", got.StreamFrom, messageOrdinalOfLast)
	}

	// Anchor on a message in the middle of the transcript, not the last
	// one -- the defect this test targets (sending the inflated
	// box-global seq back as before_seq) clamps to the newest page
	// regardless of which message it named, so anchoring away from the
	// end is what makes "clamped to the newest page" and "paged
	// correctly" produce OBSERVABLY different results.
	anchorIdx := 3
	anchorOrdinal := got.Seqs[anchorIdx]
	if anchorOrdinal != int64(anchorIdx+1) {
		t.Fatalf("seqs[%d] = %d, want %d (see TestTranscriptSeqs_ParallelToMessages)", anchorIdx, anchorOrdinal, anchorIdx+1)
	}

	resp, data := h.do("GET", fmt.Sprintf("/session/%s/message?before_seq=%d&limit=100", id, anchorOrdinal), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET message page = %d: %s", resp.StatusCode, data)
	}
	var page messagePageJSON
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatalf("decode page: %v (%s)", err, data)
	}

	// No overlap, no gap: the page's own last_seq must be EXACTLY the
	// anchor's immediate predecessor. The wrong-coordinate defect this
	// test exists to catch instead clamps the window to the newest page
	// (MessagePageWindow: hi = total when before_seq-1 >= total), so
	// page.LastSeq would come back as messageOrdinalOfLast (8), not
	// anchorOrdinal-1 (3) -- and that page would OVERLAP the anchor
	// itself and everything after it, the exact re-fetch-the-tail bug
	// this field exists to fix.
	wantLastSeq := int(anchorOrdinal) - 1
	if page.LastSeq != wantLastSeq {
		t.Fatalf("page.last_seq = %d, want %d (before_seq=%d must return messages immediately BEFORE it, no gap, no overlap): got page %+v",
			page.LastSeq, wantLastSeq, anchorOrdinal, page)
	}
	if page.FirstSeq != 1 {
		t.Errorf("page.first_seq = %d, want 1 (limit=100 comfortably covers this session's whole earlier history)", page.FirstSeq)
	}
	if page.HasMore {
		t.Errorf("page.has_more = true, want false (this page already reaches seq 1)")
	}

	// The page's own message ids must be EXACTLY the messages strictly
	// before the anchor -- confirms adjacency at the CONTENT level, not
	// only the seq bookkeeping.
	if len(page.Messages) != anchorIdx {
		t.Fatalf("page holds %d messages, want %d (messages[0:%d])", len(page.Messages), anchorIdx, anchorIdx)
	}
	for i, raw := range page.Messages {
		var m struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode page.messages[%d]: %v", i, err)
		}
		if m.ID != got.Messages[i].ID {
			t.Errorf("page.messages[%d].id = %q, want %q", i, m.ID, got.Messages[i].ID)
		}
	}
}

// TestTranscriptSeqs_PagesAdjacentAcrossCompaction is
// TestTranscriptSeqs_PagesAdjacentToRealBeforeSeq's sibling with a
// compaction in play: this numbering already shipped wrong once (the
// review that caught it), and the fold-adjustment argument
// messageDurableOrdinals' own doc comment makes -- that history already
// IS the post-fold view, so a plain sequential count over it matches
// engine/messagepage.go's own count -- was REASONED, not tested. Every
// other seqs test here uses an uncompacted session.
//
// The session is built from a COLD fixture (like
// TestTranscriptStreamFrom_SyntheticOrphanRepairNeverJournaled) rather
// than live prompts: 4 turns, msg_1..msg_8, where turn 4's assistant
// message (msg_8) is a lone tool_call with no following tool_result --
// the exact orphan shape message.ResolveOrphanToolCalls repairs on load,
// same fixture pattern as that test. It is placed in the LAST turn,
// deliberately outside the fold range this test computes below: Compact
// already handles a synthetic repair message correctly WHEN IT SITS
// inside the fold range too (see Compact's own doc comment on
// spliceFirstID/journaledFirstID), but that is a different, already-
// covered question. What THIS test needs is the ordinary case a console
// pane actually hits -- a compacted session whose still-visible tail
// happens to contain an unrepaired tool call -- with the skip (the
// synthetic's zero ordinal) and the fold (the summary's own ordinal)
// composing in the SAME seqs array, which placing it in the kept range
// proves directly.
//
// POST /session/{id}/compact with keep_turns=2 folds the oldest 2 of 4
// turns (msg_1..msg_4) into one summary message, keeping turns 3-4
// (msg_5..msg_8, plus the synthetic repair) live. Post-compaction history
// is therefore: [summary, msg_5, msg_6, msg_7, msg_8, SYNTHETIC] -- 6
// entries, 5 with a durable ordinal (1..5) and one (the synthetic) with
// none.
func TestTranscriptSeqs_PagesAdjacentAcrossCompaction(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessDir(t, dir, &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactAsstTurn("SUMMARY of turns 1-2", provider.Usage{InputTokens: 5}),
	}})

	id := "ses_5292000000000199"
	fixture := `{"type":"session","id":"` + id + `","created_at":"2025-01-02T03:04:05Z"}
{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"task 1"}]}}
{"type":"message","message":{"id":"msg_2","role":"assistant","parts":[{"type":"text","text":"reply 1"}]}}
{"type":"message","message":{"id":"msg_3","role":"user","parts":[{"type":"text","text":"task 2"}]}}
{"type":"message","message":{"id":"msg_4","role":"assistant","parts":[{"type":"text","text":"reply 2"}]}}
{"type":"message","message":{"id":"msg_5","role":"user","parts":[{"type":"text","text":"task 3"}]}}
{"type":"message","message":{"id":"msg_6","role":"assistant","parts":[{"type":"text","text":"reply 3"}]}}
{"type":"message","message":{"id":"msg_7","role":"user","parts":[{"type":"text","text":"task 4"}]}}
{"type":"message","message":{"id":"msg_8","role":"assistant","parts":[{"type":"tool_call","call_id":"A","name":"bash","arguments":{}}]}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, data := h.do("POST", "/session/"+id+"/compact", map[string]any{"keep_turns": 2, "model": "test/m1"})
	if resp.StatusCode != 200 {
		t.Fatalf("compact status %d: %s", resp.StatusCode, data)
	}
	var out compactResponseJSON
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode compact response: %v (%s)", err, data)
	}
	if out.TurnsFolded != 2 {
		t.Fatalf("turns_folded = %d, want 2 (turns 1-2, msg_1..msg_4): %+v", out.TurnsFolded, out)
	}

	got, meta := getTranscript(t, h, id)
	if meta.status != 200 {
		t.Fatalf("GET transcript = %d: %s", meta.status, meta.body)
	}
	if len(got.Messages) != 6 {
		t.Fatalf("len(messages) = %d, want 6 (1 summary + msg_5,6,7,8 + 1 synthetic repair): ids %v", len(got.Messages), messageIDs(got.Messages))
	}
	if len(got.Seqs) != len(got.Messages) {
		t.Fatalf("len(seqs) = %d, want %d (parallel to messages)", len(got.Seqs), len(got.Messages))
	}

	// The summary took the folded range's own position -- history[0] --
	// and every original msg_1..msg_4 is gone from the live view.
	if got.Messages[0].ID == "msg_1" || got.Messages[0].ID == "msg_3" {
		t.Fatalf("messages[0] = %q, want the compaction summary (msg_1/msg_3 should have been folded away)", got.Messages[0].ID)
	}

	orphanIdx := -1
	for i, m := range got.Messages {
		if message.IsSyntheticOrphanID(m.ID) {
			orphanIdx = i
		}
	}
	if orphanIdx == -1 {
		t.Fatalf("no synthetic orphan-repair message in the post-compaction messages %v -- fixture did not trigger the repair, or Compact dropped it", messageIDs(got.Messages))
	}

	// (a): the EXACT ordinal sequence, fold and skip composed in one pass
	// -- not merely monotonic, which an inflated journal-seq value (the
	// original defect) would also satisfy after a fold.
	wantSeqs := []int64{1, 2, 3, 4, 5, 0}
	// The synthetic repair is always the LAST message (ResolveOrphanToolCalls
	// appends it right after the tool_call it closes, and msg_8 -- the
	// orphaned tool_call -- is this fixture's own last message), so
	// wantSeqs' trailing 0 lines up with orphanIdx == 5 by construction;
	// assert that construction held before trusting the comparison below.
	if orphanIdx != len(got.Messages)-1 {
		t.Fatalf("orphan-repair message at index %d, want the last index %d (test fixture assumption)", orphanIdx, len(got.Messages)-1)
	}
	for i, seq := range got.Seqs {
		if seq != wantSeqs[i] {
			t.Errorf("seqs[%d] = %d, want %d (message %q)", i, seq, wantSeqs[i], got.Messages[i].ID)
		}
	}

	// (b): anchor AFTER the fold (ordinal 4, msg_7) and page backward
	// through the REAL before_seq endpoint. The defect this pins would
	// instead send an inflated box-global journal seq here, which
	// MessagePageWindow clamps to the newest page -- last_seq would come
	// back as 5 (this session's own newest durable ordinal), not 3, and
	// the page would OVERLAP everything from the anchor onward.
	anchorOrdinal := got.Seqs[3] // msg_7
	if anchorOrdinal != 4 {
		t.Fatalf("seqs[3] = %d, want 4 (msg_7's ordinal)", anchorOrdinal)
	}
	resp, data = h.do("GET", fmt.Sprintf("/session/%s/message?before_seq=%d&limit=100", id, anchorOrdinal), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET message page = %d: %s", resp.StatusCode, data)
	}
	var page messagePageJSON
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatalf("decode page: %v (%s)", err, data)
	}
	wantLastSeq := int(anchorOrdinal) - 1
	if page.LastSeq != wantLastSeq {
		t.Fatalf("page.last_seq = %d, want %d (before_seq=%d immediately after the fold must return messages immediately BEFORE it, no gap, no overlap): got page %+v",
			page.LastSeq, wantLastSeq, anchorOrdinal, page)
	}
	if page.FirstSeq != 1 {
		t.Errorf("page.first_seq = %d, want 1 (limit=100 reaches the summary, this session's own oldest durable ordinal)", page.FirstSeq)
	}
	if page.HasMore {
		t.Errorf("page.has_more = true, want false (this page already reaches ordinal 1)")
	}
	if len(page.Messages) != 3 {
		t.Fatalf("page holds %d messages, want 3 (summary, msg_5, msg_6 -- messages[0:3])", len(page.Messages))
	}
	for i, raw := range page.Messages {
		var m struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode page.messages[%d]: %v", i, err)
		}
		if m.ID != got.Messages[i].ID {
			t.Errorf("page.messages[%d].id = %q, want %q", i, m.ID, got.Messages[i].ID)
		}
	}
}

// messageIDs is a small debug helper: the ids of a []message.Message, in
// order, for a t.Fatalf message that needs to show what a fixture actually
// produced.
func messageIDs(msgs []message.Message) []string {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return ids
}
