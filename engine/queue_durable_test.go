package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSessionLog seeds a session log file from raw JSONL lines, the same
// hand-authored-log technique store tests use for replay-shape cases.
func writeSessionLog(t *testing.T, dir, id string, lines ...string) {
	t.Helper()
	var b []byte
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadSessionRestoresEnqueueWatermark: prompt.queued records carrying seq
// rebuild the high-water mark; plain (seq-less) records leave it untouched.
func TestLoadSessionRestoresEnqueueWatermark(t *testing.T) {
	dir := t.TempDir()
	const id = "ses_0000000000000001"
	writeSessionLog(t, dir, id,
		`{"type":"session","id":"ses_0000000000000001","created_at":"2026-07-21T00:00:00Z"}`,
		`{"type":"model","model":"test/m1"}`,
		`{"type":"prompt.queued","prompt":{"id":1,"text":"plain","reason":""}}`,
		`{"type":"prompt.queued","prompt":{"id":2,"text":"first durable","seq":3}}`,
		`{"type":"prompt.queued","prompt":{"id":3,"text":"second durable","seq":7}}`,
	)
	s, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.EnqueueSeq(); got != 7 {
		t.Fatalf("EnqueueSeq = %d, want 7", got)
	}
	q := s.QueuedPrompts()
	if len(q) != 3 {
		t.Fatalf("queue len = %d, want 3", len(q))
	}
	if q[1].Seq != 3 || q[2].Seq != 7 || q[0].Seq != 0 {
		t.Fatalf("folded seqs = %d,%d,%d, want 0,3,7", q[0].Seq, q[1].Seq, q[2].Seq)
	}
}

// TestLoadSessionFoldsDuplicateSeqLastWriterWins: a torn write during a
// failed fsync can leave a prompt.queued record on disk whose
// EnqueuePromptDurable call reported failure; the successful retry then
// writes a second record with the SAME seq but a fresh (burned-forward) ID.
// Replay must converge to exactly ONE queue entry carrying the LATER id —
// matching what live memory held — never two entries or the torn record's id
// (a later prompt.dequeued referencing the retry's id must find its entry).
func TestLoadSessionFoldsDuplicateSeqLastWriterWins(t *testing.T) {
	dir := t.TempDir()
	const id = "ses_0000000000000002"
	writeSessionLog(t, dir, id,
		`{"type":"session","id":"ses_0000000000000002","created_at":"2026-07-21T00:00:00Z"}`,
		`{"type":"model","model":"test/m1"}`,
		`{"type":"prompt.queued","prompt":{"id":1,"text":"msg","seq":5}}`,
		`{"type":"prompt.queued","prompt":{"id":2,"text":"msg","seq":5}}`,
	)
	s, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatal(err)
	}
	q := s.QueuedPrompts()
	if len(q) != 1 {
		t.Fatalf("queue len = %d, want 1 (last-writer-wins fold)", len(q))
	}
	if q[0].ID != 2 || q[0].Seq != 5 {
		t.Fatalf("folded entry = id %d seq %d, want id 2 seq 5", q[0].ID, q[0].Seq)
	}
	if got := s.EnqueueSeq(); got != 5 {
		t.Fatalf("EnqueueSeq = %d, want 5", got)
	}
}
