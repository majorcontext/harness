package engine

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/majorcontext/harness/provider"
)

func closeJournalHandle(t *testing.T, s *Session) {
	t.Helper()
	d := s.diskStore()
	d.mu.Lock()
	defer d.mu.Unlock()
	h := d.handles[s.ID]
	if h == nil {
		t.Fatal("test setup: the session has no open journal handle")
	}
	h.f.Close()
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
	closeJournalHandle(t, s)

	if err := s.RegisterGoal("a goal whose record cannot be written"); err != nil {
		t.Fatalf("RegisterGoal: %v", err)
	}
	if s.PersistErr() == nil {
		t.Fatal("test setup: the record write did not fail")
	}
	s.mu.Lock()
	open := s.logOpen
	s.mu.Unlock()
	if open {
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
	closeJournalHandle(t, s)
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
	s.mu.Unlock()
	s.diskStore().Release(s.ID)
	s.mu.Lock()
	s.logOpen = false
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

func diskJournal(t *testing.T, raw string) (*DiskStore, string) {
	t.Helper()
	dir := t.TempDir()
	const id = "ses_0000000000000010"
	if raw != "" {
		if err := os.WriteFile(sessionPath(dir, id), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return NewDiskStore(dir, DiskStoreOptions{}), id
}

func assertJournalBytes(t *testing.T, d *DiskStore, id, want string) {
	t.Helper()
	got, err := os.ReadFile(sessionPath(d.Dir(), id))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("journal = %q, want %q", got, want)
	}
}

func assertLoaded(t *testing.T, d *DiskStore, id string, want ...string) {
	t.Helper()
	got, err := d.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("Load = %q, want %q", got, want)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Fatalf("Load[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDiskStoreTornTailNeverHappened(t *testing.T) {
	d, id := diskJournal(t, "{\"n\":1}\n{\"n\":2")
	assertLoaded(t, d, id, `{"n":1}`)
	if n, err := d.Len(id); n != 1 || err != nil {
		t.Fatalf("Len = %d, %v, want 1, nil", n, err)
	}
	if err := d.Append(id, 1, []byte(`{"n":3}`)); err != nil {
		t.Fatal(err)
	}
	assertJournalBytes(t, d, id, "{\"n\":1}\n{\"n\":3}\n")
}

func TestDiskStoreValidTailMissingNewlineKept(t *testing.T) {
	d, id := diskJournal(t, "{\"n\":1}\n{\"n\":2}")
	assertLoaded(t, d, id, `{"n":1}`, `{"n":2}`)
	if err := d.Append(id, 2, []byte(`{"n":3}`)); err != nil {
		t.Fatal(err)
	}
	assertJournalBytes(t, d, id, "{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n")
}

func TestDiskStoreTornHeaderByteIsRewritten(t *testing.T) {
	d, id := diskJournal(t, "{")
	if _, err := d.Header(id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Header over a torn first record = %v, want ErrNotExist", err)
	}
	if err := d.Append(id, 0, []byte(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	assertJournalBytes(t, d, id, "{\"n\":1}\n")
}

func TestDiskStoreRepairedTornTailDoesNotCount(t *testing.T) {
	d, id := diskJournal(t, "{\"n\":1}\n{\"n\":2")
	if err := d.Append(id, 2, []byte(`{"n":3}`)); !errors.Is(err, ErrAppendConflict) {
		t.Fatalf("Append at 2 over one record plus a torn tail = %v, want ErrAppendConflict", err)
	}
	assertJournalBytes(t, d, id, "{\"n\":1}\n")
}

func TestDiskStoreFailedAppendLeavesNoPartialRecord(t *testing.T) {
	d, id := diskJournal(t, "")
	if err := d.Append(id, 0, []byte(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.handles[id].f.Close()
	d.mu.Unlock()
	if err := d.Append(id, 1, []byte(`{"n":2}`), []byte(`{"n":3}`)); err == nil {
		t.Fatal("Append over a closed handle succeeded")
	}
	assertLoaded(t, d, id, `{"n":1}`)
	if err := d.Append(id, 1, []byte(`{"n":2}`)); err != nil {
		t.Fatalf("Append after the failure: %v", err)
	}
	assertJournalBytes(t, d, id, "{\"n\":1}\n{\"n\":2}\n")
}

func TestDiskStoreRejectsUnsafeNames(t *testing.T) {
	d, _ := diskJournal(t, "")
	for _, id := range []string{"", "..", "a/b", `a\b`} {
		if err := d.Append(id, 0, []byte(`{}`)); err == nil {
			t.Errorf("Append(%q) succeeded", id)
		}
	}
	if err := d.PutBlob("ses_a", "../x", nil); err == nil {
		t.Error("PutBlob with a traversal name succeeded")
	}
}

type conflictStore struct {
	SessionStore
	appends int
}

func (c *conflictStore) Append(id string, at int, records ...[]byte) error {
	c.appends++
	return ErrAppendConflict
}

func TestSessionStopsWritingAfterAppendConflict(t *testing.T) {
	st := &conflictStore{SessionStore: NewDiskStore(t.TempDir(), DiskStoreOptions{})}
	s := NewSession(Config{SessionStore: st})
	if err := s.Persist(); !errors.Is(err, ErrAppendConflict) {
		t.Fatalf("Persist = %v, want ErrAppendConflict", err)
	}
	first := st.appends
	if err := s.Persist(); !errors.Is(err, ErrAppendConflict) {
		t.Fatalf("second Persist = %v, want ErrAppendConflict", err)
	}
	if st.appends != first {
		t.Fatalf("the engine retried an append after a conflict: %d calls, want %d", st.appends, first)
	}
}

type ambiguousStore struct {
	SessionStore
	land bool
}

func (a *ambiguousStore) Append(id string, at int, records ...[]byte) error {
	if a.land {
		if err := a.SessionStore.Append(id, at, records...); err != nil {
			return err
		}
	}
	return errors.New("timeout after commit")
}

func TestSessionSettlesAmbiguousAppendWithLen(t *testing.T) {
	for _, land := range []bool{true, false} {
		st := &ambiguousStore{SessionStore: NewDiskStore(t.TempDir(), DiskStoreOptions{}), land: land}
		s := NewSession(Config{SessionStore: st})
		err := s.Persist()
		if land && err != nil {
			t.Errorf("landed write: Persist = %v, want nil", err)
		}
		if !land && err == nil {
			t.Error("lost write: Persist = nil, want the append error")
		}
		n, _ := st.Len(s.ID)
		if land && n != 2 {
			t.Errorf("landed write: log holds %d records, want header and model", n)
		}
	}
}
