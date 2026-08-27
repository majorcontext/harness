package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// pagedSession drives n plain turns against a fresh session in dir and
// returns it. Each turn appends exactly two durable messages: the user
// prompt and the assistant reply.
func pagedSession(t *testing.T, dir string, n int) *Session {
	t.Helper()
	turns := make([][]provider.Event, 0, n)
	for i := 0; i < n; i++ {
		turns = append(turns, compactTurn("reply", provider.Usage{InputTokens: 1}))
	}
	prov := &scriptedProvider{name: "test", turns: turns}
	s := NewSession(persistCfg(dir, prov))
	runTurns(t, s, n)
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr: %v", err)
	}
	return s
}

// wholeSequence is the ORACLE: the durable message sequence, derived from
// the journal's bytes by this test alone.
//
// It calls no engine function. The rule it applies is the contract the
// endpoint publishes, restated here in full: read the records in order;
// each message record appends its id; each compact record removes the ids
// from first_id through last_id and puts its summary id in their place.
// Deriving it from LoadSession instead would share applyCompactRecord with
// the implementation under test, and a fold defect would then agree with
// itself (AGENTS.md's oracle rule).
func wholeSequence(t *testing.T, dir, id string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Type    string `json:"type"`
			Message *struct {
				ID string `json:"id"`
			} `json:"message"`
			Compact *struct {
				FirstID string `json:"first_id"`
				LastID  string `json:"last_id"`
				Summary struct {
					ID string `json:"id"`
				} `json:"summary"`
			} `json:"compact"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			if i == len(lines)-1 {
				continue // a torn final line, which no reader counts
			}
			t.Fatalf("oracle: line %d: %v", i+1, err)
		}
		switch {
		case rec.Type == "message" && rec.Message != nil:
			ids = append(ids, rec.Message.ID)
		case rec.Type == "compact" && rec.Compact != nil:
			first, last := -1, -1
			for j, existing := range ids {
				if first == -1 && existing == rec.Compact.FirstID {
					first = j
				}
				if first != -1 && existing == rec.Compact.LastID {
					last = j
					break
				}
			}
			if first == -1 || last == -1 {
				t.Fatalf("oracle: compact range [%s, %s] not in the sequence", rec.Compact.FirstID, rec.Compact.LastID)
			}
			folded := append([]string{}, ids[:first]...)
			folded = append(folded, rec.Compact.Summary.ID)
			ids = append(folded, ids[last+1:]...)
		}
	}
	return ids
}

// assertOracleAgreesWithLoad cross-checks the oracle against the production
// authority for the shapes where the two must agree: a full LoadSession's
// history, minus the repair messages it derives, is the durable sequence.
// The oracle stays independent; this is the integration check that keeps it
// honest.
func assertOracleAgreesWithLoad(t *testing.T, cfg Config, dir, id string) {
	t.Helper()
	loaded, err := LoadSession(cfg, id)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	var durable []string
	for _, m := range loaded.History() {
		if message.IsSyntheticOrphanID(m.ID) {
			continue
		}
		durable = append(durable, m.ID)
	}
	if !sameIDs(durable, wholeSequence(t, dir, id)) {
		t.Fatalf("oracle disagrees with LoadSession: load = %v, oracle = %v", durable, wholeSequence(t, dir, id))
	}
}

func idsOf(msgs []message.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.ID)
	}
	return out
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReadMessagePageMatchesFullHistory is the oracle test: every page, at
// every offset and size, must be exactly the corresponding window of the
// session's whole durable message sequence — in the same order, with the
// same ids, under the seqs the page reports.
func TestReadMessagePageMatchesFullHistory(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: nil}
	cfg := persistCfg(dir, prov)
	sess := pagedSession(t, dir, 6) // 12 durable messages
	assertOracleAgreesWithLoad(t, cfg, dir, sess.ID)
	want := wholeSequence(t, dir, sess.ID)
	if len(want) != 12 {
		t.Fatalf("test setup: %d durable messages, want 12", len(want))
	}

	for _, limit := range []int{1, 2, 5, 12, 50} {
		for beforeSeq := 0; beforeSeq <= len(want)+1; beforeSeq++ {
			page, err := ReadMessagePage(dir, sess.ID, beforeSeq, limit)
			if err != nil {
				t.Fatalf("ReadMessagePage(before=%d, limit=%d): %v", beforeSeq, limit, err)
			}
			hi := len(want)
			if beforeSeq > 0 && beforeSeq-1 < hi {
				hi = beforeSeq - 1
			}
			lo := hi - limit + 1
			if lo < 1 {
				lo = 1
			}
			if hi < 1 {
				if len(page.Messages) != 0 {
					t.Errorf("before=%d limit=%d: got %d messages, want an empty page", beforeSeq, limit, len(page.Messages))
				}
				continue
			}
			if !sameIDs(idsOf(page.Messages), want[lo-1:hi]) {
				t.Errorf("before=%d limit=%d: page ids = %v, want %v", beforeSeq, limit, idsOf(page.Messages), want[lo-1:hi])
			}
			if page.FirstSeq != lo || page.LastSeq != hi {
				t.Errorf("before=%d limit=%d: seqs = [%d,%d], want [%d,%d]", beforeSeq, limit, page.FirstSeq, page.LastSeq, lo, hi)
			}
			if page.Total != len(want) {
				t.Errorf("before=%d limit=%d: total = %d, want %d", beforeSeq, limit, page.Total, len(want))
			}
			if page.HasMore != (lo > 1) {
				t.Errorf("before=%d limit=%d: has_more = %v, want %v", beforeSeq, limit, page.HasMore, lo > 1)
			}
		}
	}
}

// TestReadMessagePageWalksBackToTheStart drives the whole sequence one page
// at a time, exactly as a console scrolling up does, and asserts the pages
// reassemble into the full history with no gap and no repeat.
func TestReadMessagePageWalksBackToTheStart(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: nil}
	cfg := persistCfg(dir, prov)
	sess := pagedSession(t, dir, 5) // 10 durable messages
	assertOracleAgreesWithLoad(t, cfg, dir, sess.ID)
	want := wholeSequence(t, dir, sess.ID)

	var got []message.Message
	before := 0
	for {
		page, err := ReadMessagePage(dir, sess.ID, before, 3)
		if err != nil {
			t.Fatalf("ReadMessagePage(before=%d): %v", before, err)
		}
		got = append(page.Messages, got...)
		if !page.HasMore {
			break
		}
		before = page.FirstSeq
	}
	if !sameIDs(idsOf(got), want) {
		t.Errorf("paged walk = %v\nwant %v", idsOf(got), want)
	}
}

// TestReadMessagePageUndoesCompaction: a compact record replaces its folded
// range with one summary message. A page numbered over the durable sequence
// must see the summary and never the folded messages, however far back it
// reaches — the reverse of the splice LoadSession applies forward.
func TestReadMessagePageUndoesCompaction(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactTurn("three", provider.Usage{InputTokens: 10}),
		compactTurn("four", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("SUMMARY ONE", provider.Usage{InputTokens: 5}),
		compactSummaryTurn("SUMMARY TWO", provider.Usage{InputTokens: 5}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 4)
	if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 2}); err != nil {
		t.Fatalf("first Compact: %v", err)
	}
	// A second compaction folds the FIRST summary into the second one — the
	// nested case, where a compact record met while skipping is itself the
	// message the outer fold started at.
	if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1}); err != nil {
		t.Fatalf("second Compact: %v", err)
	}

	assertOracleAgreesWithLoad(t, cfg, dir, s.ID)
	want := wholeSequence(t, dir, s.ID)
	page, err := ReadMessagePage(dir, s.ID, 0, 100)
	if err != nil {
		t.Fatalf("ReadMessagePage: %v", err)
	}
	if !sameIDs(idsOf(page.Messages), want) {
		t.Errorf("page ids = %v\nwant %v", idsOf(page.Messages), want)
	}
	if page.Total != len(want) {
		t.Errorf("total = %d, want %d", page.Total, len(want))
	}
}

// TestReadMessagePageReadsOnlyTheTail is the cost claim: a bounded page
// must not depend on the bytes at the start of the journal. Overwriting
// everything except the tail with unreadable bytes leaves a page of the
// newest messages answerable; a reader that walked the whole log could not
// answer it.
//
// The index is written first and left alone, so the seq numbering the page
// reports still comes from the intact fold.
func TestReadMessagePageReadsOnlyTheTail(t *testing.T) {
	dir := t.TempDir()
	sess := pagedSession(t, dir, 40) // 80 durable messages, comfortably > one chunk
	if _, err := ReadSessionIndex(dir, sess.ID); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, sess.ID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the final 8 KiB intact (aligned to a record boundary); make the
	// rest unreadable.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := len(data) - 8192
	if cut <= 0 {
		t.Fatalf("test setup: journal is only %d bytes", len(data))
	}
	for data[cut-1] != '\n' {
		cut++
	}
	for i := 0; i < cut-1; i++ {
		if data[i] != '\n' {
			data[i] = 'x'
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Length and modification time are the index's staleness key. Leaving
	// both alone is what keeps the intact fold in front of a journal whose
	// head is now unreadable.
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}

	page, err := ReadMessagePage(dir, sess.ID, 0, 2)
	if err != nil {
		t.Fatalf("ReadMessagePage: %v", err)
	}
	if len(page.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(page.Messages))
	}
	if page.LastSeq != 80 || page.FirstSeq != 79 {
		t.Errorf("seqs = [%d,%d], want [79,80]", page.FirstSeq, page.LastSeq)
	}
	if !page.HasMore {
		t.Error("has_more = false, want true")
	}
}

// TestReadMessagePageToleratesTornTail: a crash mid-write leaves an
// unparseable final line. The forward reader (scanLog) ignores exactly that
// one line, and so must the backward one — otherwise the newest page of a
// crashed session, the page an operator most wants, is the one that fails.
func TestReadMessagePageToleratesTornTail(t *testing.T) {
	dir := t.TempDir()
	sess := pagedSession(t, dir, 3)
	path := filepath.Join(dir, sess.ID+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message","message":{"id":"msg_torn"`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	page, err := ReadMessagePage(dir, sess.ID, 0, 10)
	if err != nil {
		t.Fatalf("ReadMessagePage: %v", err)
	}
	if page.Total != 6 {
		t.Errorf("total = %d, want 6 (the torn record must not count)", page.Total)
	}
	for _, m := range page.Messages {
		if m.ID == "msg_torn" {
			t.Error("page carries the torn record")
		}
	}
}

// TestReadMessagePageEmptySession: a session with a journal but no messages
// pages to an empty result, not an error.
func TestReadMessagePageEmptySession(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"}
	s := NewSession(persistCfg(dir, prov))
	if err := s.Persist(); err != nil {
		t.Fatal(err)
	}
	page, err := ReadMessagePage(dir, s.ID, 0, 10)
	if err != nil {
		t.Fatalf("ReadMessagePage: %v", err)
	}
	if len(page.Messages) != 0 || page.Total != 0 || page.HasMore {
		t.Errorf("page = %+v, want an empty page", page)
	}
}

// TestReadMessagePageCapsLimit: the endpoint exists to bound a read, so an
// oversized limit is capped rather than honored.
func TestReadMessagePageCapsLimit(t *testing.T) {
	dir := t.TempDir()
	sess := pagedSession(t, dir, 2)
	page, err := ReadMessagePage(dir, sess.ID, 0, MaxMessagePageLimit*10)
	if err != nil {
		t.Fatalf("ReadMessagePage: %v", err)
	}
	if len(page.Messages) != 4 {
		t.Errorf("got %d messages, want all 4", len(page.Messages))
	}
}

// TestReadMessagePageTailAndFoldPathsAgree: a compacted session is served
// two different ways depending on how far back the page reaches — the
// backward tail walk while the page stays in the uncompacted tail, the
// forward fold once it crosses a compact record. The two must produce the
// same messages under the same seqs, or a console would see a page change
// shape as it scrolled.
func TestReadMessagePageTailAndFoldPathsAgree(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactTurn("three", provider.Usage{InputTokens: 10}),
		compactTurn("four", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("SUMMARY", provider.Usage{InputTokens: 5}),
		compactTurn("five", provider.Usage{InputTokens: 10}),
		compactTurn("six", provider.Usage{InputTokens: 10}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 4)
	if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	runTurns(t, s, 2)

	assertOracleAgreesWithLoad(t, cfg, dir, s.ID)
	want := wholeSequence(t, dir, s.ID)
	whole, err := ReadMessagePage(dir, s.ID, 0, 100)
	if err != nil {
		t.Fatalf("whole page: %v", err)
	}
	if !sameIDs(idsOf(whole.Messages), want) {
		t.Fatalf("whole page ids = %v, want %v", idsOf(whole.Messages), want)
	}
	// Page one message at a time across the compaction boundary: each page
	// must equal the same window of the whole sequence.
	for seq := 1; seq <= whole.Total; seq++ {
		page, err := ReadMessagePage(dir, s.ID, seq+1, 1)
		if err != nil {
			t.Fatalf("page at seq %d: %v", seq, err)
		}
		if len(page.Messages) != 1 || page.Messages[0].ID != want[seq-1] {
			t.Errorf("page at seq %d = %v, want %s", seq, idsOf(page.Messages), want[seq-1])
		}
		if page.FirstSeq != seq || page.LastSeq != seq {
			t.Errorf("page at seq %d reports seqs [%d,%d]", seq, page.FirstSeq, page.LastSeq)
		}
	}
}

// TestReadMessagePageSkipsDerivedRepairMessages: a journal whose last turn
// died between a tool call and its result. A full load repairs it with a
// synthetic tool result, and GET /session counts that message. A page must
// NOT: the synthetic message has no record, so it has no seq, and inventing
// one would renumber every page a client already holds.
func TestReadMessagePageSkipsDerivedRepairMessages(t *testing.T) {
	dir := t.TempDir()
	id := "ses_0123456789abcdef"
	journal := `{"type":"session","id":"ses_0123456789abcdef","created_at":"2026-01-02T03:04:05Z","workdir":"/w"}
{"type":"model","model":"test/m1"}
{"type":"message","message":{"id":"msg_u1","role":"user","parts":[{"type":"text","text":"go"}],"created_at":"2026-01-02T03:04:06Z"}}
{"type":"message","message":{"id":"msg_a1","role":"assistant","parts":[{"type":"tool_call","call_id":"tc1","name":"bash","arguments":{}}],"created_at":"2026-01-02T03:04:07Z"}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(journal), 0o644); err != nil {
		t.Fatal(err)
	}
	page, err := ReadMessagePage(dir, id, 0, 10)
	if err != nil {
		t.Fatalf("ReadMessagePage: %v", err)
	}
	if page.Total != 2 {
		t.Errorf("total = %d, want 2 (the two records, not the derived repair)", page.Total)
	}
	if len(page.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(page.Messages))
	}
	for _, m := range page.Messages {
		if message.IsSyntheticOrphanID(m.ID) {
			t.Errorf("page carries a derived repair message %q", m.ID)
		}
	}
	// The index still reports the repaired count, which is what GET
	// /session reports; the two numbers are deliberately different.
	ix, err := ReadSessionIndex(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Messages != 3 || ix.DurableMessages != 2 {
		t.Errorf("index reports messages=%d durable=%d, want 3 and 2", ix.Messages, ix.DurableMessages)
	}
}

// TestScanLogBackwardBoundaries drives the reverse scanner directly, over
// the shapes a journal actually reaches: a record larger than one read
// block, a record spanning three blocks, a file with no final newline,
// blank lines, and an empty file. The scanner's contract is "every
// non-empty line, newest first, and only the file's final line may be
// torn"; each case checks the lines it yields, in order.
func TestScanLogBackwardBoundaries(t *testing.T) {
	big := strings.Repeat("A", revChunkBytes+7)     // one block plus change
	huge := strings.Repeat("B", revChunkBytes*2+11) // spans three blocks
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"empty file", "", nil},
		{"single line with newline", "one\n", []string{"one"}},
		{"single line without newline", "one", []string{"one"}},
		{"two lines", "one\ntwo\n", []string{"two", "one"}},
		{"blank lines between records", "one\n\n\ntwo\n", []string{"two", "one"}},
		{"trailing blank lines", "one\ntwo\n\n\n", []string{"two", "one"}},
		{"record larger than one block", "one\n" + big + "\ntwo\n", []string{"two", big, "one"}},
		{"record spanning three blocks", "one\n" + huge + "\n", []string{huge, "one"}},
		{"two oversized records", big + "\n" + huge + "\n", []string{huge, big}},
		// A bound past the end of the file: a caller holding a stale index
		// names one. The scan reads the file's real bytes rather than
		// failing with an EOF from ReadAt.
		{"bound past the end", "one\ntwo\n", []string{"two", "one"}},
		{"CRLF terminators", "one\r\ntwo\r\n", []string{"two", "one"}},
		{"line exactly one block", strings.Repeat("C", revChunkBytes) + "\n", []string{strings.Repeat("C", revChunkBytes)}},
		{"terminator on a block boundary", strings.Repeat("D", revChunkBytes-1) + "\n" + "tail\n", []string{"tail", strings.Repeat("D", revChunkBytes-1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "log")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			end := int64(len(tc.content))
			if tc.name == "bound past the end" {
				end += 4096
			}
			var got []string
			fi, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err := scanLogBackward(f, end, fi.Size(), func(line []byte, _ bool) (bool, error) {
				got = append(got, string(line))
				return true, nil
			}); err != nil {
				t.Fatalf("scanLogBackward: %v", err)
			}
			if !sameIDs(got, tc.want) {
				if len(got) != len(tc.want) {
					t.Fatalf("got %d lines, want %d", len(got), len(tc.want))
				}
				for i := range got {
					if got[i] != tc.want[i] {
						t.Fatalf("line %d differs: got %d bytes, want %d bytes", i, len(got[i]), len(tc.want[i]))
					}
				}
			}
		})
	}
}

// TestScanLogBackwardMarksOnlyTheFinalLineAsTail: the torn-line allowance
// belongs to the file's last record and to nothing else, matching scanLog's
// forward rule. A trailing newline, or trailing blank lines, must not spend
// it on a complete record.
func TestScanLogBackwardMarksOnlyTheFinalLineAsTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var tails []string
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := scanLogBackward(f, 15, fi.Size(), func(line []byte, isTail bool) (bool, error) {
		if isTail {
			tails = append(tails, string(line))
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(tails) != 1 || tails[0] != "three" {
		t.Errorf("lines marked as the tail = %v, want exactly [three]", tails)
	}
}

// TestReadMessagePageOversizedRecord is the page-level counterpart: a
// journal carrying one message far larger than a read block must page
// correctly, and the page must carry that message whole.
func TestReadMessagePageOversizedRecord(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn(strings.Repeat("x", revChunkBytes*2+64), provider.Usage{InputTokens: 10}),
		compactTurn("small", provider.Usage{InputTokens: 10}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 2)
	assertOracleAgreesWithLoad(t, cfg, dir, s.ID)

	page, err := ReadMessagePage(dir, s.ID, 0, 4)
	if err != nil {
		t.Fatalf("ReadMessagePage: %v", err)
	}
	if !sameIDs(idsOf(page.Messages), wholeSequence(t, dir, s.ID)) {
		t.Fatalf("page ids = %v, want the whole sequence", idsOf(page.Messages))
	}
	var found bool
	for _, m := range page.Messages {
		for _, p := range m.Parts {
			if txt, ok := p.(*message.Text); ok && len(txt.Text) > revChunkBytes {
				found = true
			}
		}
	}
	if !found {
		t.Error("the oversized message did not survive the page read whole")
	}
}

// TestReadMessagePageAfterATruncatingRepair: a crash leaves a torn record,
// a later ensureLog truncates it away, and the journal is now SHORTER than
// the index that already folded it. The next page read must notice and
// refold, rather than number a page against a record that no longer exists.
func TestReadMessagePageAfterATruncatingRepair(t *testing.T) {
	dir := t.TempDir()
	sess := pagedSession(t, dir, 3)
	path := filepath.Join(dir, sess.ID+".jsonl")

	// Append a torn record, let the index fold cover it, then truncate it
	// away exactly as ensureLog's repair does.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message","message":{"id":"msg_torn`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSessionIndex(dir, sess.ID); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := bytes.LastIndexByte(data, '\n') + 1
	if err := os.WriteFile(path, data[:cut], 0o644); err != nil {
		t.Fatal(err)
	}
	// Keep the modification time: without this the index refolds on the
	// stat alone, and the stale-bound path under test never runs.
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}

	page, err := ReadMessagePage(dir, sess.ID, 0, 10)
	if err != nil {
		t.Fatalf("ReadMessagePage after a truncating repair: %v", err)
	}
	if page.Total != 6 || len(page.Messages) != 6 {
		t.Errorf("page = total %d, %d messages; want 6 and 6", page.Total, len(page.Messages))
	}
}

// TestReadMessagePageStaleIndexBound drives the window the public call
// cannot: a page read that has ALREADY taken an index when another process
// repairs a torn tail and shortens the journal. The bytes the page was
// numbered against are gone, so the read must report staleness rather than
// serve a page whose sequence numbers describe records that no longer
// exist, or fail with a bare EOF from its scan.
//
// The seam is readMessagePageWithIndex, which takes the index the caller
// already holds. ReadMessagePage takes a fresh index and retries once, so
// the public call answers correctly in the same situation.
func TestReadMessagePageStaleIndexBound(t *testing.T) {
	dir := t.TempDir()
	sess := pagedSession(t, dir, 3)
	stale, err := ReadSessionIndex(dir, sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The repair: drop the final record, as ensureLog's truncating branch
	// does. The index in hand still names the longer journal.
	path := filepath.Join(dir, sess.ID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := bytes.LastIndexByte(data[:len(data)-1], '\n') + 1
	if err := os.WriteFile(path, data[:cut], 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := readMessagePageWithIndex(dir, sess.ID, stale, 0, 10); !errors.Is(err, ErrStaleMessagePage) {
		t.Errorf("readMessagePageWithIndex with a stale bound = %v, want ErrStaleMessagePage", err)
	}
	// The public call refolds and answers: five durable messages remain.
	page, err := ReadMessagePage(dir, sess.ID, 0, 10)
	if err != nil {
		t.Fatalf("ReadMessagePage: %v", err)
	}
	if page.Total != 5 || len(page.Messages) != 5 {
		t.Errorf("page = total %d, %d messages; want 5 and 5", page.Total, len(page.Messages))
	}
}

// TestPageErrorClassifiesATruncationDuringTheScan covers the narrower race
// the pre-scan size check cannot: the truncation lands AFTER that check and
// the scan itself fails on bytes that are gone. The failure must still be
// reported as staleness, not as a numbering fault the caller cannot act on.
func TestPageErrorClassifiesATruncationDuringTheScan(t *testing.T) {
	dir := t.TempDir()
	sess := pagedSession(t, dir, 2)
	ix, err := ReadSessionIndex(dir, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sess.ID+".jsonl")
	if err := os.Truncate(path, ix.LogSize-10); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pageError(f, sess.ID, ix, errors.New("short read")); !errors.Is(err, ErrStaleMessagePage) {
		t.Errorf("pageError on a shortened journal = %v, want ErrStaleMessagePage", err)
	}
	if err := pageError(f, sess.ID, SessionIndex{LogSize: 1}, errors.New("real fault")); errors.Is(err, ErrStaleMessagePage) {
		t.Error("pageError reported staleness for a journal that did not shrink")
	}
}

// TestFoldedPageReadsOnlyTheIndexedPrefix: the fold path reads exactly the
// journal prefix its index summarized, and nothing past it.
//
// The record appended below is a COMPACT record, not just a large one,
// because that is the shape that proves the bound: a compaction folds
// earlier messages away and renumbers every seq after it. A page that read
// past its own index would answer with those new numbers while reporting
// the old total — one response describing two instants of the journal.
func TestFoldedPageReadsOnlyTheIndexedPrefix(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactTurn("three", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("SUMMARY", provider.Usage{InputTokens: 5}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 3)
	if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 2}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	ix, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := wholeSequence(t, dir, s.ID)

	// A second compaction lands AFTER the index was taken, exactly as a
	// concurrent turn's own compaction would. It folds the sequence the
	// index numbered, so a read that saw it would renumber the page.
	history := s.History()
	if len(history) < 3 {
		t.Fatalf("test setup: history is %d messages", len(history))
	}
	record := map[string]any{
		"type": "compact",
		"compact": map[string]any{
			"first_id":     history[0].ID,
			"last_id":      history[1].ID,
			"turns_folded": 1,
			"summary":      map[string]any{"id": "cmpsum_appended", "role": "user"},
		},
	}
	line, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, s.ID+".jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// The page is served from the index already in hand — the seam that
	// makes "one instant of the journal" checkable.
	page, err := readMessagePageWithIndex(dir, s.ID, ix, 0, 100)
	if err != nil {
		t.Fatalf("readMessagePageWithIndex: %v", err)
	}
	if !sameIDs(idsOf(page.Messages), want) {
		t.Errorf("page ids = %v, want %v (the appended compaction is outside the read)", idsOf(page.Messages), want)
	}
	if page.Total != len(want) {
		t.Errorf("total = %d, want %d", page.Total, len(want))
	}
}

// TestFoldedPageDecodesOnlyThePage is the cost guard for the fold path. A
// page that reaches into compacted history folds every line through the
// slim shape, but it must decode IN FULL only the records the page carries.
// An earlier revision ran a second scan that decoded every line into a full
// record to find the wanted ones, which decoded every message body in the
// journal — the cost this endpoint exists to remove. A review caught it.
//
// The probe is a record carrying a part of an unknown type. It is valid
// JSON, and the slim fold walks past it: indexPart keeps only tool-call and
// tool-result parts. A FULL decode of that same line fails, because
// unmarshalPart rejects an unknown part type (message/message.go). So a
// page that excludes that message proves the record was never fully
// decoded, and a page that includes it fails loudly.
func TestFoldedPageDecodesOnlyThePage(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactTurn("three", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("SUMMARY", provider.Usage{InputTokens: 5}),
		compactTurn("four", provider.Usage{InputTokens: 10}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 3)
	if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	runTurns(t, s, 1)

	// Rewrite the OLDEST message record — folded away by the compaction, so
	// no page below asks for it — to carry an unknown part type.
	path := filepath.Join(dir, s.ID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var poisonedID string
	poisonedLine := -1
	for i, line := range lines {
		var probe struct {
			Type    string `json:"type"`
			Message *struct {
				ID string `json:"id"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &probe) != nil || probe.Type != recMessage || probe.Message == nil {
			continue
		}
		lines[i] = `{"type":"message","message":{"id":"` + probe.Message.ID +
			`","role":"user","parts":[{"type":"from_a_newer_binary","text":"x"}]}}`
		poisonedID, poisonedLine = probe.Message.ID, i
		break
	}
	if poisonedID == "" {
		t.Fatal("test setup: no message record to rewrite")
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Assert the probe's premise directly: the rewritten line must be
	// foldable and NOT fully decodable. If canonical message decoding ever
	// accepts an unknown part type, this fails loudly here rather than
	// quietly turning the guard below into a test that passes from birth.
	var full record
	if err := json.Unmarshal([]byte(lines[poisonedLine]), &full); err == nil {
		t.Fatal("the probe record decodes in full, so excluding it proves nothing")
	}

	// The slim fold must still accept the journal: this probe is only
	// meaningful while the record is foldable and merely undecodable.
	ix, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatalf("the slim fold rejected the record, so this probe proves nothing: %v", err)
	}
	if ix.DurableMessages < 3 {
		t.Fatalf("test setup: %d durable messages", ix.DurableMessages)
	}

	// A page over the compacted history: it crosses the compact record, so
	// it takes the fold path, and it must not decode the rewritten record.
	page, err := ReadMessagePage(dir, s.ID, 0, ix.DurableMessages)
	if err != nil {
		t.Fatalf("ReadMessagePage: %v", err)
	}
	for _, m := range page.Messages {
		if m.ID == poisonedID {
			t.Fatalf("the page carried the folded-away record %q", poisonedID)
		}
	}
	if !sameIDs(idsOf(page.Messages), wholeSequence(t, dir, s.ID)) {
		t.Errorf("page ids = %v, want the whole durable sequence", idsOf(page.Messages))
	}
}
