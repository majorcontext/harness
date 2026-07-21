package engine

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// TestDurableEnqueueRepairsTornHeader is the deterministic regression for
// the torn-header case ensureLog now repairs (see store.go's ensureLog):a
// crash after only a few bytes of the very first header+model write ever
// reached disk leaves a non-empty file with no valid header at all.
// LoadSession already tolerates this (a corrupt/incomplete FINAL line is
// silently ignored — scanLog), but before the repair, resuming WRITES onto
// that file wrongly skipped rewriting the header (gated on size == 0) and
// concatenated the next record directly onto the garbage byte with no
// separating newline — losing that record silently, then hard-failing
// LoadSession entirely ("corrupt record at line 1") the moment a further
// record pushed it off the final-line position. This test drives exactly
// that sequence and asserts neither failure mode reproduces.
func TestDurableEnqueueRepairsTornHeader(t *testing.T) {
	dir := t.TempDir()
	const id = "ses_0000000000000003"
	path := sessionPath(dir, id)

	// A torn header: the process crashed after writing a single byte of
	// ensureLog's header+model buffer, before anything parseable landed.
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatalf("LoadSession over a torn 1-byte header: %v", err)
	}
	if wm := s.EnqueueSeq(); wm != 0 {
		t.Fatalf("EnqueueSeq = %d, want 0 (a torn header carries no restorable state)", wm)
	}

	id1, dup, err := s.EnqueuePromptDurable("first", 1)
	if err != nil || dup {
		t.Fatalf("EnqueuePromptDurable(seq=1): id=%d dup=%v err=%v, want a fresh accepted enqueue", id1, dup, err)
	}

	reloaded, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatalf("reload after the repaired write: %v", err)
	}
	q := reloaded.QueuedPrompts()
	if len(q) != 1 || q[0].ID != id1 || q[0].Text != "first" || q[0].Seq != 1 {
		t.Fatalf("reloaded queue = %+v, want one entry {ID:%d Text:first Seq:1}", q, id1)
	}
	if wm := reloaded.EnqueueSeq(); wm != 1 {
		t.Fatalf("EnqueueSeq after reload = %d, want 1", wm)
	}

	// A second durable enqueue plus a SECOND reload is what proves there is
	// no corrupt-line cascade: pre-fix, this reload would hard-fail with
	// "corrupt record at line 1" once the repaired-in record was no longer
	// the file's last line.
	id2, dup, err := reloaded.EnqueuePromptDurable("second", 2)
	if err != nil || dup {
		t.Fatalf("EnqueuePromptDurable(seq=2): id=%d dup=%v err=%v", id2, dup, err)
	}
	reloaded2, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatalf("second reload must not cascade-fail: %v", err)
	}
	q2 := reloaded2.QueuedPrompts()
	if len(q2) != 2 || q2[0].ID != id1 || q2[1].ID != id2 {
		t.Fatalf("second reload queue = %+v, want ids %d,%d in order", q2, id1, id2)
	}
	if wm := reloaded2.EnqueueSeq(); wm != 2 {
		t.Fatalf("EnqueueSeq after second reload = %d, want 2", wm)
	}
}

// TestPlainEnqueueRepairsTornTailAfterValidRecords is the deterministic
// regression for the torn-tail-after-valid-records case: a journal with a
// real header, model, and one queued record, followed by a partial trailing
// line (no closing brace, no newline) from a crash mid-write of a SECOND
// record. LoadSession already folds the valid prefix correctly; this test
// additionally drives a write (which is where the repair in ensureLog
// happens) and two further reloads, asserting the torn bytes are gone from
// disk (truncated away, never concatenated onto), every real record
// survives in order, and no reload ever errors.
func TestPlainEnqueueRepairsTornTailAfterValidRecords(t *testing.T) {
	dir := t.TempDir()
	const id = "ses_0000000000000004"
	path := sessionPath(dir, id)

	valid := `{"type":"session","id":"ses_0000000000000004","created_at":"2026-07-21T00:00:00Z"}` + "\n" +
		`{"type":"model","model":"test/m1"}` + "\n" +
		`{"type":"prompt.queued","prompt":{"id":1,"text":"first"}}` + "\n"
	torn := `{"type":"prompt.queued","prompt":{"id":2,"tex` // crash mid-write: no closing brace, no trailing '\n'
	if err := os.WriteFile(path, []byte(valid+torn), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatalf("LoadSession over valid records + a torn tail: %v", err)
	}
	q := s.QueuedPrompts()
	if len(q) != 1 || q[0].ID != 1 || q[0].Text != "first" {
		t.Fatalf("loaded queue = %+v, want one entry {ID:1 Text:first}", q)
	}

	// The first write after load is where the repair actually fires.
	id2, err := s.EnqueuePrompt("second")
	if err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}
	if id2 != 2 {
		t.Fatalf("EnqueuePrompt id = %d, want 2 (nextID must advance past the torn record's id 2, folded from the valid id=1 record alone)", id2)
	}

	reloaded, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatalf("first reload: %v", err)
	}
	id3, err := reloaded.EnqueuePrompt("third")
	if err != nil {
		t.Fatalf("EnqueuePrompt on reloaded session: %v", err)
	}
	reloaded2, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatalf("second reload: %v", err)
	}

	q2 := reloaded2.QueuedPrompts()
	wantIDs := []int64{1, id2, id3}
	wantTexts := []string{"first", "second", "third"}
	if len(q2) != len(wantIDs) {
		t.Fatalf("final queue = %+v, want %d entries", q2, len(wantIDs))
	}
	for i, p := range q2 {
		if p.ID != wantIDs[i] || p.Text != wantTexts[i] {
			t.Fatalf("final queue[%d] = %+v, want {ID:%d Text:%s}", i, p, wantIDs[i], wantTexts[i])
		}
	}
	if wm := reloaded2.EnqueueSeq(); wm != 0 {
		t.Fatalf("EnqueueSeq = %d, want 0 (every enqueue here is plain, no seq)", wm)
	}

	// Structural check that the torn bytes are truly gone from disk (not
	// just invisible to the fold): every line must be complete, valid JSON
	// — a substring search for the torn text is unreliable here since
	// "text" (a legitimate field name) itself starts with "tex". Exactly 5
	// complete lines: session, model, and the three queued records.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	if len(lines) != 5 {
		t.Fatalf("journal has %d lines, want 5 (no leftover torn line): %s", len(lines), data)
	}
	for i, line := range lines {
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("journal line %d is not valid, complete JSON (torn bytes survived the repair): %v\nline: %s", i+1, err, line)
		}
	}
}

// TestDurableEnqueueRepairsMissingTrailingNewlineWithoutDataLoss is the
// deterministic regression for ensureLog's second repair case: a file that
// does not end in '\n' does NOT always mean a torn write. The tail can be a
// complete, valid record whose closing newline alone never made it to disk
// (or, as here, was cut off by a byte-exact truncation) — LoadSession
// parses it and folds it in just fine (scanLog's tolerance is parse-based,
// not newline-based), so it is already durable and already visible. An
// earlier version of the repair could not tell the two cases apart and
// truncated this case away too, silently destroying an accepted record —
// exactly the failure the rapid model test's TornCrashReload action found.
// The correct repair APPENDS the missing newline instead, preserving the
// record.
func TestDurableEnqueueRepairsMissingTrailingNewlineWithoutDataLoss(t *testing.T) {
	dir := t.TempDir()
	const id = "ses_0000000000000005"
	path := sessionPath(dir, id)

	s := NewSession(Config{SessionDir: dir})
	s.ID = id
	if _, _, err := s.EnqueuePromptDurable("first", 1); err != nil {
		t.Fatalf("EnqueuePromptDurable(seq=1): %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if data[len(data)-1] != '\n' {
		t.Fatalf("precondition: journal should end in '\\n' before the test truncates it: %q", data)
	}
	// Drop exactly the trailing '\n': the record itself is complete and
	// valid, only its terminator is missing.
	if err := os.Truncate(path, int64(len(data)-1)); err != nil {
		t.Fatal(err)
	}

	reloaded, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatalf("LoadSession over a complete record missing only its trailing newline: %v", err)
	}
	q := reloaded.QueuedPrompts()
	if len(q) != 1 || q[0].ID != 1 || q[0].Text != "first" || q[0].Seq != 1 {
		t.Fatalf("reloaded queue = %+v, want one entry {ID:1 Text:first Seq:1} — the record must NOT be lost", q)
	}
	if wm := reloaded.EnqueueSeq(); wm != 1 {
		t.Fatalf("EnqueueSeq = %d, want 1", wm)
	}

	// Trigger ensureLog's repair via a write, then verify the first record
	// SURVIVED (not truncated away) alongside the new one.
	if _, _, err := reloaded.EnqueuePromptDurable("second", 2); err != nil {
		t.Fatalf("EnqueuePromptDurable(seq=2): %v", err)
	}
	reloaded2, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatalf("reload after repair: %v", err)
	}
	q2 := reloaded2.QueuedPrompts()
	if len(q2) != 2 || q2[0].ID != 1 || q2[0].Text != "first" || q2[1].ID != 2 || q2[1].Text != "second" {
		t.Fatalf("final queue = %+v, want [{ID:1 Text:first Seq:1} {ID:2 Text:second Seq:2}] — repair must preserve the first record, not truncate it", q2)
	}
	if wm := reloaded2.EnqueueSeq(); wm != 2 {
		t.Fatalf("EnqueueSeq = %d, want 2", wm)
	}
}
