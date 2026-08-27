package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/provider"
)

// unwritableSessionDir returns a SessionDir path guaranteed to make
// (*Session).ensureLog fail on its first disk touch, deterministically,
// regardless of which user runs the test.
//
// Under a normal (non-root) user, chmod 0555 (read+execute, no write) on an
// already-existing directory is enough: ensureLog's os.MkdirAll is a no-op on
// a directory that already exists, so the os.OpenFile(..., O_CREATE, ...)
// that follows hits a permission-denied error, since DAC permission bits are
// enforced normally.
//
// Root bypasses DAC permission checks (a chmod 0555 directory is still
// writable by root on Linux), so chmod alone would make this test silently
// exercise a real, successful write when the suite runs as root — as many CI
// containers and this sandbox do. The deterministic alternative used in that
// case: pre-create a plain FILE at the exact path where SessionDir itself
// needs to be a directory, so every os.MkdirAll of it fails with ENOTDIR — a
// structural filesystem error no privilege level can bypass (you cannot
// mkdir through a path component that already exists as a non-directory).
func unwritableSessionDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	if os.Geteuid() == 0 {
		blocked := filepath.Join(base, "blocked")
		if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("seed blocking file: %v", err)
		}
		// SessionDir names a path *through* the file above; os.MkdirAll can
		// never create it.
		return filepath.Join(blocked, "sessions")
	}
	dir := filepath.Join(base, "sessions")
	if err := os.MkdirAll(dir, 0o555); err != nil {
		t.Fatalf("seed unwritable session dir: %v", err)
	}
	return dir
}

// The Prompt-path equivalent of this test (an unwritable SessionDir must
// surface only through PersistErr, never through Prompt's return error) is
// already covered by TestPersistErrSurfacesWriteFailure in store_test.go; a
// second copy of it here would just be the same assertions on the same
// path, so it was deleted rather than kept for line count. What's genuinely
// additive is exercising the same disk-failure guarantee against
// RegisterGoal's persistGoalLocked path instead, which no existing test
// touches:

// TestRegisterGoalSurvivesUnwritableSessionDir covers the same disk-failure
// guarantee on RegisterGoal's persistGoalLocked path: a goal.set record that
// cannot be written must not stop the goal from registering (RegisterGoal
// only errors when a goal is already active) or panic — it surfaces solely
// through PersistErr.
func TestRegisterGoalSurvivesUnwritableSessionDir(t *testing.T) {
	dir := unwritableSessionDir(t)
	prov := &scriptedProvider{name: "test"}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)

	if err := s.RegisterGoal("reach the goal"); err != nil {
		t.Fatalf("RegisterGoal returned an error from a disk-write failure: %v", err)
	}
	if cond, ok := s.ActiveGoal(); !ok || cond != "reach the goal" {
		t.Errorf("ActiveGoal = %q, %v; want the goal active in memory despite the persist failure", cond, ok)
	}
	if perr := s.PersistErr(); perr == nil {
		t.Fatal("PersistErr() = nil after RegisterGoal against an unwritable SessionDir, want the write failure reported")
	}
}

// TestFailedRecordWriteDropsTheLogHandle covers the window a partial write
// opens. When Write fails after putting bytes on disk, the journal's last
// line is torn. ensureLog knows how to repair that, but it only runs when
// the session has no open handle: its fast path returns immediately while
// one exists. A session that kept its handle would append the NEXT record
// directly onto the torn line with no separator. The two lines become one
// unparseable line, and scanLog hard-fails the whole session as soon as any
// later record makes it non-final — so one failed write could poison a log
// permanently. The retry of a failed EnqueuePromptDurable is exactly that
// shape.
//
// The failure is injected at the OS level, by closing the session's own
// file descriptor: the next Write returns an error, through the production
// persist path, with no stub in the way.
func TestFailedRecordWriteDropsTheLogHandle(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 1)

	// The bytes a partial write leaves behind: a torn final line.
	path := sessionPath(dir, s.ID)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message","message":{"id":"msg_torn`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Make the session's own next write fail.
	s.mu.Lock()
	s.logFile.Close()
	s.mu.Unlock()

	if err := s.RegisterGoal("a goal whose record cannot be written"); err != nil {
		t.Fatalf("RegisterGoal: %v", err)
	}
	if s.PersistErr() == nil {
		t.Fatal("test setup: the record write did not fail")
	}
	s.mu.Lock()
	handle := s.logFile
	s.mu.Unlock()
	if handle != nil {
		t.Error("a failed record write kept the log handle; the next write appends onto the torn line instead of repairing it")
	}

	// The next record must reopen, repair, and append cleanly.
	s.ClearGoal()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "msg_torn") {
		t.Error("the torn line survived; ensureLog's tail repair did not run")
	}
	if _, err := LoadSession(cfg, s.ID); err != nil {
		t.Errorf("journal is no longer loadable after a failed write: %v", err)
	}
}

// TestIndexRecoversAfterAFailedWrite: a failed record write marks the
// session's fold broken, because the fold no longer knows what the journal
// holds. It must not stay broken for the life of the session object — every
// later read would refold the whole journal, which is the cost the index
// exists to remove. The reopen a failed write forces (see writeRecord)
// re-seeds the fold from the repaired journal.
func TestIndexRecoversAfterAFailedWrite(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 20}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 1)

	// Make the next write fail, at the OS level, through the production
	// persist path.
	s.mu.Lock()
	s.logFile.Close()
	s.mu.Unlock()
	if err := s.RegisterGoal("a goal whose record cannot be written"); err != nil {
		t.Fatalf("RegisterGoal: %v", err)
	}
	if s.PersistErr() == nil {
		t.Fatal("test setup: the record write did not fail")
	}
	s.mu.Lock()
	broken := s.index.broken
	s.mu.Unlock()
	if !broken {
		t.Fatal("test setup: the failed write did not mark the fold broken")
	}

	// The next turn reopens the log, and the fold must come back with it.
	// PersistErr is deliberately not checked: it is sticky, so it still
	// reports the failure this test injected.
	runTurns(t, s, 1)
	s.mu.Lock()
	broken = s.index.broken
	s.mu.Unlock()
	if broken {
		t.Error("the fold is still broken after a reopen; the session writes no index for the rest of its life")
	}

	// And the sidecar it writes must be current: corrupt the journal at an
	// unchanged staleness key, so only a current sidecar can answer.
	corruptJournalKeepingSize(t, dir, s.ID)
	ix, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatalf("ReadSessionIndex: %v", err)
	}
	if ix.Messages != 4 {
		t.Errorf("Messages = %d, want 4 (the re-seeded fold must cover the whole journal)", ix.Messages)
	}
}

// TestIndexRecoveryCountsARecordTheFoldNeverSaw is the sharp edge of the
// recovery above. A failed Write can land a record's bytes and not its
// trailing newline. ensureLog's tail repair then takes its case-2 branch:
// the tail parses, so the record is terminated and KEPT. The fold never saw
// it.
//
// Re-seeding from the file is what makes the fold agree with those bytes.
// Merely clearing the broken flag would resume flushing a sidecar short by
// one message, while logSize claimed the whole file — a stale index that
// reads as current, which no reader would ever refold.
func TestIndexRecoveryCountsARecordTheFoldNeverSaw(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 20}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 1) // two durable messages

	// The exact shape of a write that landed its bytes and not its
	// newline, with the session's fold left broken by that failure.
	path := sessionPath(dir, s.ID)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message","message":{"id":"msg_landed","role":"user","parts":[{"type":"text","text":"hi"}]}}`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s.mu.Lock()
	s.index.broken = true
	s.logFile.Close()
	s.logFile = nil
	s.mu.Unlock()

	// The next turn reopens, repairs (case 2: the record is kept), and
	// re-seeds the fold.
	runTurns(t, s, 1) // two more durable messages

	// Only a CURRENT sidecar can answer once the journal is unreadable at
	// an unchanged staleness key.
	corruptJournalKeepingSize(t, dir, s.ID)
	ix, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatalf("ReadSessionIndex: %v", err)
	}
	if ix.Messages != 5 {
		t.Errorf("Messages = %d, want 5: two turns of two, plus the record the repair kept", ix.Messages)
	}
}
