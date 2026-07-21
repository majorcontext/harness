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

// durableTestSession builds a persisted session rooted in a temp dir,
// capturing emitted events.
func durableTestSession(t *testing.T) (*Session, *[]Event) {
	t.Helper()
	var events []Event
	s := NewSession(Config{
		SessionDir: t.TempDir(),
		OnEvent:    func(ev Event) { events = append(events, ev) },
	})
	return s, &events
}

func TestEnqueuePromptDurableAcceptsAndAdvancesWatermark(t *testing.T) {
	s, events := durableTestSession(t)
	id, dup, err := s.EnqueuePromptDurable("hello", 1)
	if err != nil || dup {
		t.Fatalf("EnqueuePromptDurable = id %d dup %v err %v", id, dup, err)
	}
	if id != 1 {
		t.Fatalf("id = %d, want 1", id)
	}
	if got := s.EnqueueSeq(); got != 1 {
		t.Fatalf("EnqueueSeq = %d, want 1", got)
	}
	q := s.QueuedPrompts()
	if len(q) != 1 || q[0].Seq != 1 || q[0].Text != "hello" {
		t.Fatalf("queue = %+v", q)
	}
	// Event carries the seq so a journal tailer can correlate.
	var found bool
	for _, ev := range *events {
		if ev.Type == EventPromptQueued && ev.QueueSeq == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no EventPromptQueued with QueueSeq=1 in %+v", *events)
	}
	// Durable now: a fresh load sees both entry and watermark.
	re, err := LoadSession(Config{SessionDir: s.cfg.SessionDir}, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if re.EnqueueSeq() != 1 || len(re.QueuedPrompts()) != 1 {
		t.Fatalf("reload: seq %d queue %d", re.EnqueueSeq(), len(re.QueuedPrompts()))
	}
}

// Duplicate and out-of-order seqs are the same case: at-or-below watermark
// is a clean no-op (the caller's retry or replay of an already-accepted
// message), nothing persisted or emitted.
func TestEnqueuePromptDurableDuplicateAndStaleSeqAreNoOps(t *testing.T) {
	s, events := durableTestSession(t)
	if _, _, err := s.EnqueuePromptDurable("m5", 5); err != nil {
		t.Fatal(err)
	}
	evBefore := len(*events)
	for _, seq := range []int64{5, 3} {
		id, dup, err := s.EnqueuePromptDurable("dup", seq)
		if err != nil || !dup || id != 0 {
			t.Fatalf("seq %d: id %d dup %v err %v, want 0 true nil", seq, id, dup, err)
		}
	}
	if len(s.QueuedPrompts()) != 1 || len(*events) != evBefore {
		t.Fatalf("duplicate mutated state: queue %d events %d->%d",
			len(s.QueuedPrompts()), evBefore, len(*events))
	}
}

func TestEnqueuePromptDurableRejectsInvalid(t *testing.T) {
	s, _ := durableTestSession(t)
	if _, _, err := s.EnqueuePromptDurable("  ", 1); err == nil {
		t.Fatal("empty text accepted")
	}
	if _, _, err := s.EnqueuePromptDurable("x", 0); err == nil {
		t.Fatal("seq 0 accepted")
	}
	if _, _, err := NewSession(Config{}).EnqueuePromptDurable("x", 1); err == nil {
		t.Fatal("no SessionDir accepted — a durable enqueue with nowhere durable to write must error")
	}
}

// A failed write returns an error (unlike EnqueuePrompt's swallowed
// lastPersistErr — the entire point is that the caller must not 2xx), leaves
// the queue and watermark untouched, and BURNS the assigned ID so a torn
// on-disk record can never collide with a later plain enqueue's ID.
func TestEnqueuePromptDurableWriteFailureReturnsErrorAndBurnsID(t *testing.T) {
	var events []Event
	s := NewSession(Config{
		SessionDir: unwritableSessionDir(t),
		OnEvent:    func(ev Event) { events = append(events, ev) },
	})
	if _, _, err := s.EnqueuePromptDurable("doomed", 1); err == nil {
		t.Fatal("write failure did not surface as error")
	}
	if len(s.QueuedPrompts()) != 0 || s.EnqueueSeq() != 0 || len(events) != 0 {
		t.Fatalf("failed enqueue mutated state: queue %d seq %d events %d",
			len(s.QueuedPrompts()), s.EnqueueSeq(), len(events))
	}
	if s.PersistErr() == nil {
		t.Fatal("PersistErr not set")
	}
	s.mu.Lock()
	next := s.promptQueueNextID
	s.mu.Unlock()
	if next != 2 {
		t.Fatalf("promptQueueNextID = %d, want 2 (ID 1 burned by the failed write)", next)
	}
}

// TestLoadSessionDequeueAfterFoldRemovesRetryEntry pins the guarantee the
// fold's comments promise: after a torn record (id 1) and its successful
// same-seq retry (id 2) fold to one entry with id 2, a prompt.dequeued
// referencing id 2 removes it — and a stray dequeued for the torn id 1 is a
// harmless no-op, never a corruption.
func TestLoadSessionDequeueAfterFoldRemovesRetryEntry(t *testing.T) {
	dir := t.TempDir()
	writeSessionLog(t, dir, "ses_0000000000000003",
		`{"type":"session","id":"ses_0000000000000003","created_at":"2026-07-21T00:00:00Z"}`,
		`{"type":"model","model":"test/m1"}`,
		`{"type":"prompt.queued","prompt":{"id":1,"text":"msg","seq":5}}`,
		`{"type":"prompt.queued","prompt":{"id":2,"text":"msg","seq":5}}`,
		`{"type":"prompt.dequeued","prompt":{"id":1,"text":"msg","reason":"delivered"}}`,
		`{"type":"prompt.dequeued","prompt":{"id":2,"text":"msg","reason":"delivered"}}`,
	)
	s, err := LoadSession(Config{SessionDir: dir}, "ses_0000000000000003")
	if err != nil {
		t.Fatal(err)
	}
	if q := s.QueuedPrompts(); len(q) != 0 {
		t.Fatalf("queue = %+v, want empty (dequeue of retry id removed folded entry; stray torn-id dequeue was a no-op)", q)
	}
	if got := s.EnqueueSeq(); got != 5 {
		t.Fatalf("EnqueueSeq = %d, want 5 (dequeue never lowers the watermark)", got)
	}
}
