package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// indexOracle is what a full LoadSession reports for the fields
// SessionIndex claims to answer without one. The oracle is the AUTHORITY
// the index replaces — GET /session builds its body from exactly these
// accessors (server/handlers.go's buildSession) — so an index that
// disagrees with it is wrong by definition.
type indexOracle struct {
	ID              string
	CreatedAt       time.Time
	LastActivityAt  time.Time
	Model           message.ModelRef
	Effort          message.Effort
	WorkDir         string
	ParentSession   string
	TaskParentID    string
	TaskAgentType   string
	TaskDepth       int
	SpawnedChildIDs []string
	Messages        int
	Usage           provider.Usage
	LastInputTokens int
	GoalActive      bool
	GoalCondition   string
	Queued          int
	CompactionCount int
	LastCompactedAt time.Time
}

// oracleOf reads the authority: a session loaded from its journal.
func oracleOf(t *testing.T, sess *Session) indexOracle {
	t.Helper()
	o := indexOracle{
		ID:              sess.ID,
		CreatedAt:       sess.CreatedAt(),
		LastActivityAt:  sess.LastActivityAt(),
		Model:           sess.Model(),
		Effort:          sess.Effort(),
		WorkDir:         sess.WorkDir(),
		ParentSession:   sess.ParentSession(),
		TaskParentID:    sess.TaskParentID(),
		TaskAgentType:   sess.TaskAgentType(),
		TaskDepth:       sess.TaskDepth(),
		SpawnedChildIDs: sess.SpawnedChildIDs(),
		Messages:        len(sess.History()),
		Usage:           sess.Usage(),
		Queued:          len(sess.QueuedPrompts()),
		CompactionCount: sess.CompactionCount(),
		LastCompactedAt: sess.LastCompactedAt(),
	}
	o.GoalCondition, o.GoalActive = sess.ActiveGoal()
	if last, ok := sess.LastUsage(); ok {
		o.LastInputTokens = last.InputTokens
	}
	return o
}

// oracleOfIndex projects a SessionIndex onto the same shape.
func oracleOfIndex(ix SessionIndex) indexOracle {
	return indexOracle{
		ID:              ix.ID,
		CreatedAt:       ix.CreatedAt,
		LastActivityAt:  ix.LastActivityAt,
		Model:           ix.Model,
		Effort:          ix.Effort,
		WorkDir:         ix.WorkDir,
		ParentSession:   ix.ParentSession,
		TaskParentID:    ix.TaskParentID,
		TaskAgentType:   ix.TaskAgentType,
		TaskDepth:       ix.TaskDepth,
		SpawnedChildIDs: ix.SpawnedChildIDs,
		Messages:        ix.Messages,
		Usage:           ix.Usage,
		LastInputTokens: ix.LastInputTokens,
		GoalActive:      ix.GoalActive,
		GoalCondition:   ix.GoalCondition,
		Queued:          ix.Queued,
		CompactionCount: ix.CompactionCount,
		LastCompactedAt: ix.LastCompactedAt,
	}
}

// assertIndexMatchesLoad reads id's index and compares it, field for field,
// against a full LoadSession of the same journal.
func assertIndexMatchesLoad(t *testing.T, cfg Config, id string) SessionIndex {
	t.Helper()
	loaded, err := LoadSession(cfg, id)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	ix, err := ReadSessionIndex(cfg.SessionDir, id)
	if err != nil {
		t.Fatalf("ReadSessionIndex: %v", err)
	}
	want, got := oracleOf(t, loaded), oracleOfIndex(ix)
	if wantJSON, gotJSON := mustJSON(t, want), mustJSON(t, got); wantJSON != gotJSON {
		t.Errorf("index disagrees with LoadSession\n index = %s\n  load = %s", gotJSON, wantJSON)
	}
	return ix
}

// TestSessionIndexMatchesLoadSession is the oracle test: for every journal
// shape a session can reach through its own production entry points, the
// index must report exactly what a full LoadSession reports. Each case
// drives the real API (Prompt, Compact, RegisterGoal, EnqueuePrompt,
// SetModel, SetEffort), never a hand-written journal, so the fold is
// verified against the records production actually writes.
func TestSessionIndexMatchesLoadSession(t *testing.T) {
	cases := []struct {
		name string
		// turns scripts the provider; drive runs the session.
		turns [][]provider.Event
		drive func(t *testing.T, s *Session)
	}{
		{
			name:  "never prompted",
			turns: [][]provider.Event{},
			drive: func(t *testing.T, s *Session) {
				if err := s.Persist(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "plain turns",
			turns: [][]provider.Event{
				compactTurn("one", provider.Usage{InputTokens: 10, OutputTokens: 5}),
				compactTurn("two", provider.Usage{InputTokens: 20, OutputTokens: 7, CacheReadTokens: 3, CacheWriteTokens: 4}),
			},
			drive: func(t *testing.T, s *Session) { runTurns(t, s, 2) },
		},
		{
			name: "tool loop",
			turns: [][]provider.Event{
				asstTurn(provider.StopToolUse, toolCall("tc1", "bash", `{"command":"echo hi"}`)),
				asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
			},
			drive: func(t *testing.T, s *Session) {
				if _, err := s.Prompt(context.Background(), "go"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "model and effort switches",
			turns: [][]provider.Event{
				compactTurn("one", provider.Usage{InputTokens: 10}),
			},
			drive: func(t *testing.T, s *Session) {
				runTurns(t, s, 1)
				s.SetModel(message.ModelRef{Provider: "test", Model: "m2"})
				s.SetEffort(message.EffortHigh)
			},
		},
		{
			name: "compaction folds a prefix",
			turns: [][]provider.Event{
				compactTurn("one", provider.Usage{InputTokens: 10}),
				compactTurn("two", provider.Usage{InputTokens: 20}),
				compactTurn("three", provider.Usage{InputTokens: 30}),
				compactSummaryTurn("SUMMARY", provider.Usage{InputTokens: 40, OutputTokens: 8}),
			},
			drive: func(t *testing.T, s *Session) {
				runTurns(t, s, 3)
				if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "active goal",
			turns: [][]provider.Event{
				compactTurn("one", provider.Usage{InputTokens: 10}),
			},
			drive: func(t *testing.T, s *Session) {
				runTurns(t, s, 1)
				if err := s.RegisterGoal("ship it"); err != nil {
					t.Fatal(err)
				}
				if err := s.UpdateGoal("ship it twice"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:  "goal cleared",
			turns: [][]provider.Event{},
			drive: func(t *testing.T, s *Session) {
				if err := s.RegisterGoal("ship it"); err != nil {
					t.Fatal(err)
				}
				s.ClearGoal()
			},
		},
		{
			name:  "prompt queue",
			turns: [][]provider.Event{},
			drive: func(t *testing.T, s *Session) {
				if _, err := s.EnqueuePrompt("first"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.EnqueuePrompt("second"); err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.EnqueuePromptDurable("third", 1); err != nil {
					t.Fatal(err)
				}
				if _, _, ok := s.DequeuePrompt("delivered"); !ok {
					t.Fatal("DequeuePrompt: queue empty")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			prov := &scriptedProvider{name: "test", turns: tc.turns}
			cfg := persistCfg(dir, prov)
			cfg.WorkDir = dir
			cfg.ParentSession = "ses_00000000000000000000000000"
			s := NewSession(cfg)
			tc.drive(t, s)
			if err := s.PersistErr(); err != nil {
				t.Fatalf("PersistErr = %v", err)
			}
			assertIndexMatchesLoad(t, cfg, s.ID)
		})
	}
}

// TestSessionIndexIsCurrentAfterEveryRecord proves the write-through half:
// after each mutation the sidecar already describes the journal, so a
// reader never has to open it.
//
// The proof is destructive on purpose. After each mutation the journal is
// overwritten with garbage of the SAME byte length: any read of the journal
// itself would now fail or fold nonsense, so an index that still reports
// the right message count can only have come from the sidecar.
func TestSessionIndexIsCurrentAfterEveryRecord(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 20}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 2)

	// 2 turns x (user + assistant) = 4 durable messages.
	want := 4
	corruptJournalKeepingSize(t, dir, s.ID)
	ix, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatalf("ReadSessionIndex: %v", err)
	}
	if ix.Messages != want {
		t.Errorf("Messages = %d, want %d (index must be served from the sidecar, not the journal)", ix.Messages, want)
	}
	if ix.Usage.InputTokens != 30 {
		t.Errorf("Usage.InputTokens = %d, want 30", ix.Usage.InputTokens)
	}
}

// corruptJournalKeepingSize replaces a session journal with unparseable
// bytes of identical length AND identical modification time. Length and
// modification time are the whole staleness key ReadSessionIndex checks, so
// this leaves a "current" sidecar in front of a journal no fold can read:
// any answer that still names the right message count can only have come
// from the sidecar.
//
// Production never produces this state — a journal is append-only and has
// one writer — which is exactly why it is a usable probe here.
func corruptJournalKeepingSize(t *testing.T, dir, id string) {
	t.Helper()
	path := filepath.Join(dir, id+".jsonl")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	junk := make([]byte, len(data))
	for i := range junk {
		junk[i] = 'x'
	}
	if err := os.WriteFile(path, junk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
}

// TestReadSessionIndexRefoldsWhenJournalGrows covers the crash window: a
// process that wrote a record but died before its sidecar flush, and any
// journal an older binary wrote with no sidecar at all. The stored index
// covers fewer bytes than the journal holds, so it must be refolded, never
// served.
func TestReadSessionIndexRefoldsWhenJournalGrows(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 20}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 1)

	// Freeze the sidecar as it stands after turn 1, then run turn 2 with
	// the sidecar pinned to the older fold — the exact state a crash
	// between a record write and its flush leaves behind.
	frozen, err := os.ReadFile(filepath.Join(dir, s.ID+sessionIndexSuffix))
	if err != nil {
		t.Fatal(err)
	}
	runTurns(t, s, 1)
	if err := os.WriteFile(filepath.Join(dir, s.ID+sessionIndexSuffix), frozen, 0o644); err != nil {
		t.Fatal(err)
	}

	ix, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatalf("ReadSessionIndex: %v", err)
	}
	if ix.Messages != 4 {
		t.Errorf("Messages = %d, want 4 (a stale sidecar must refold)", ix.Messages)
	}
	// The refold writes back, so the next read takes the fast path.
	corruptJournalKeepingSize(t, dir, s.ID)
	again, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatalf("ReadSessionIndex (second): %v", err)
	}
	if again.Messages != 4 {
		t.Errorf("second read Messages = %d, want 4 (refold must write back)", again.Messages)
	}
}

// TestReadSessionIndexRefoldsAfterJournalShrinks covers ensureLog's
// torn-tail truncation: the journal gets SHORTER than the sidecar's fold.
// A byte-length comparison alone (rather than "grew since") is what catches
// it.
func TestReadSessionIndexRefoldsAfterJournalShrinks(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 20}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 2)

	path := filepath.Join(dir, s.ID+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the final record, as a truncating tail repair would.
	cut := len(data) - 1
	for cut > 0 && data[cut-1] != '\n' {
		cut--
	}
	if err := os.WriteFile(path, data[:cut], 0o644); err != nil {
		t.Fatal(err)
	}

	ix, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatalf("ReadSessionIndex: %v", err)
	}
	if ix.Messages != 3 {
		t.Errorf("Messages = %d, want 3 (a shorter journal must refold)", ix.Messages)
	}
}

// TestReadSessionIndexIgnoresUnusableSidecar covers every way a stored
// index can be unusable: torn (a crash inside the rewrite), empty, a
// checksum that does not cover its bytes, a format version from another
// binary, or an id naming another session. Each must refold silently rather
// than serve or fail.
//
// The version and id cases build a VALID envelope and recompute the
// checksum over the altered bytes. Mangling the index bytes alone would
// fail the checksum first, and the test would pass without ever reaching
// the check it names.
func TestReadSessionIndexIgnoresUnusableSidecar(t *testing.T) {
	sidecars := map[string]func(t *testing.T, ix SessionIndex) []byte{
		"empty": func(*testing.T, SessionIndex) []byte { return nil },
		"torn prefix": func(t *testing.T, ix SessionIndex) []byte {
			b := mustMarshalIndex(t, ix)
			return b[:len(b)/2]
		},
		"checksum does not cover the bytes": func(t *testing.T, ix SessionIndex) []byte {
			ix.Messages = 99
			inner, err := json.Marshal(ix)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(sessionIndexFile{CRC32: crc32.ChecksumIEEE(inner) + 1, Index: inner})
			if err != nil {
				t.Fatal(err)
			}
			return b
		},
		// Each mangled sidecar also carries a WRONG message count, so
		// serving it is detectable. A sidecar mangled only in the field
		// under test would still report the right count, and the case
		// would pass whether the check ran or not.
		"old version": func(t *testing.T, ix SessionIndex) []byte {
			ix.Version = sessionIndexVersion + 1
			ix.Messages = 99
			return mustMarshalIndex(t, ix)
		},
		"wrong id": func(t *testing.T, ix SessionIndex) []byte {
			ix.ID = "ses_0123456789abcdef"
			ix.Messages = 99
			return mustMarshalIndex(t, ix)
		},
	}
	for name, mangle := range sidecars {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
				compactTurn("one", provider.Usage{InputTokens: 10}),
			}}
			cfg := persistCfg(dir, prov)
			s := NewSession(cfg)
			runTurns(t, s, 1)

			ix, err := ReadSessionIndex(dir, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, s.ID+sessionIndexSuffix), mangle(t, ix), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := ReadSessionIndex(dir, s.ID)
			if err != nil {
				t.Fatalf("ReadSessionIndex: %v", err)
			}
			if got.Messages != 2 {
				t.Errorf("Messages = %d, want 2 (an unusable sidecar must refold)", got.Messages)
			}
		})
	}
}

// mustMarshalIndex renders a sidecar exactly as the production writers do,
// checksum included, so a test that alters a field still produces a file
// that reaches the check it means to exercise.
func mustMarshalIndex(t *testing.T, ix SessionIndex) []byte {
	t.Helper()
	b, err := marshalSessionIndex(ix)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSessionIndexSurvivesResume pins the seam a resumed session depends
// on: LoadSession seeds the fold from the journal it replays, so the first
// record the reloaded session writes flushes an index describing the WHOLE
// journal, not just that one record.
func TestSessionIndexSurvivesResume(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 20}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 1)

	// Resume in a second Session object, exactly as the serve API does for
	// a session it evicted, and drive one more turn.
	resumed, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if err := resumed.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	corruptJournalKeepingSize(t, dir, s.ID)
	ix, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatalf("ReadSessionIndex: %v", err)
	}
	if ix.Messages != 4 {
		t.Errorf("Messages = %d, want 4 (a resumed session must flush the whole fold)", ix.Messages)
	}
	if ix.Usage.InputTokens != 30 {
		t.Errorf("Usage.InputTokens = %d, want 30", ix.Usage.InputTokens)
	}
}

// TestListSessionIndexesSkipsNonSessionFiles: the server's own event
// journal (events.jsonl) and a session's sidecar live in the same
// directory. Neither is a session, and neither may appear in a listing or
// fail it.
func TestListSessionIndexesSkipsNonSessionFiles(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 1)

	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"type":"message","seq":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := ListSessionIndexes(dir)
	if err != nil {
		t.Fatalf("ListSessionIndexes: %v", err)
	}
	if len(list) != 1 || list[0].ID != s.ID {
		t.Fatalf("ListSessionIndexes = %+v, want exactly the one session %s", list, s.ID)
	}
}

// TestListSessionsMatchesIndex pins the projection: ListSessions now reads
// indexes, and every field SessionInfo carries must still be the value the
// index folded.
func TestListSessionsMatchesIndex(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10, OutputTokens: 5}),
		compactTurn("two", provider.Usage{InputTokens: 20, OutputTokens: 6}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 2)

	infos, err := ListSessions(dir)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("ListSessions returned %d sessions, want 1", len(infos))
	}
	ix, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, want := infos[0], SessionInfo{
		ID:              ix.ID,
		CreatedAt:       ix.CreatedAt,
		Messages:        ix.Messages,
		Usage:           ix.Usage,
		LastInputTokens: ix.LastInputTokens,
	}
	if mustJSON(t, got) != mustJSON(t, want) {
		t.Errorf("ListSessions entry = %s, want %s", mustJSON(t, got), mustJSON(t, want))
	}
}

// TestReadSessionIndexRejectsUnknownSession: an id with no journal is an
// error, not an empty summary — the 404 GET /session/{id} depends on.
func TestReadSessionIndexRejectsUnknownSession(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadSessionIndex(dir, "ses_0123456789abcdef"); err == nil {
		t.Fatal("ReadSessionIndex on a missing journal = nil error, want an error")
	}
	if _, err := ReadSessionIndex(dir, "../escape"); err == nil {
		t.Fatal("ReadSessionIndex on a path-traversal id = nil error, want an error")
	}
}

// TestSessionIndexRejectsMixedSidecarBytes is the torn-read guard. Both
// sidecar writers replace the file in place, so a reader in another process
// can read part of the old file and part of the new one. Such a mix parses:
// the fields are the same, in the same order, and can carry a staleness key
// that matches the journal. The checksum is what turns it into a clean
// miss.
//
// The test builds the exact hazard: a sidecar whose index bytes describe an
// OLD session state while its staleness key describes the CURRENT journal.
func TestSessionIndexRejectsMixedSidecarBytes(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 20}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 1)
	stale, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	runTurns(t, s, 1)
	current, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The mix: turn 1's counts under turn 2's staleness key, checksummed as
	// a naive writer would leave it — over the OLD bytes.
	mixed := stale
	mixed.LogSize, mixed.LogModTime = current.LogSize, current.LogModTime
	inner, err := json.Marshal(mixed)
	if err != nil {
		t.Fatal(err)
	}
	staleInner, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	file, err := json.Marshal(sessionIndexFile{CRC32: crc32.ChecksumIEEE(staleInner), Index: inner})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, s.ID+sessionIndexSuffix), file, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatalf("ReadSessionIndex: %v", err)
	}
	if got.Messages != 4 {
		t.Errorf("Messages = %d, want 4 (a mixed sidecar must refold, never be served)", got.Messages)
	}
}

// TestSessionIndexRefoldsWhenJournalIsRewrittenInPlace: byte length alone
// cannot tell a journal from a same-length replacement. The modification
// time is the second half of the key.
//
// The check is only as good as the filesystem's timestamp resolution, which
// is why this test asserts its own precondition first: on a filesystem that
// reports the rewrite under the SAME modification time, there is nothing
// for the key to catch and the test skips rather than fails.
func TestSessionIndexRefoldsWhenJournalIsRewrittenInPlace(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 1)
	if _, err := ReadSessionIndex(dir, s.ID); err != nil {
		t.Fatal(err)
	}

	// Rewrite the journal at the same length, WITHOUT restoring its
	// modification time — an ordinary external rewrite, unlike
	// corruptJournalKeepingSize's deliberate probe.
	path := filepath.Join(dir, s.ID+".jsonl")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	junk := make([]byte, len(data))
	for i := range junk {
		junk[i] = 'x'
	}
	junk[len(junk)-1] = '\n'
	if err := os.WriteFile(path, junk, 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.ModTime().Equal(before.ModTime()) {
		t.Skip("filesystem reports the same modification time after a rewrite; the staleness key has nothing to detect here")
	}
	if _, err := ReadSessionIndex(dir, s.ID); err == nil {
		t.Fatal("ReadSessionIndex served an index for a rewritten journal, want a refold that fails on the new bytes")
	}
}

// TestSessionIndexMatchesLoadSessionForOrphanToolCall: a journal whose last
// turn died between a tool call and its result. LoadSession repairs it with
// a synthetic tool result, which CHANGES the message count and the activity
// timestamp a reader sees. The index must report the repaired numbers, or
// GET /session and GET /session/{id}/message would disagree about how many
// messages a session has.
func TestSessionIndexMatchesLoadSessionForOrphanToolCall(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"}
	cfg := persistCfg(dir, prov)
	id := "ses_0123456789abcdef"
	journal := `{"type":"session","id":"ses_0123456789abcdef","created_at":"2026-01-02T03:04:05Z","workdir":"/w"}
{"type":"model","model":"test/m1"}
{"type":"message","message":{"id":"msg_u1","role":"user","parts":[{"type":"text","text":"go"}],"created_at":"2026-01-02T03:04:06Z"}}
{"type":"message","message":{"id":"msg_a1","role":"assistant","parts":[{"type":"tool_call","call_id":"tc1","name":"bash","arguments":{}}],"created_at":"2026-01-02T03:04:07Z"}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(journal), 0o644); err != nil {
		t.Fatal(err)
	}
	ix := assertIndexMatchesLoad(t, cfg, id)
	if ix.DurableMessages != 2 {
		t.Errorf("DurableMessages = %d, want 2 (the records themselves)", ix.DurableMessages)
	}
	if ix.Messages != 3 {
		t.Errorf("Messages = %d, want 3 (the repair adds one)", ix.Messages)
	}
}

// TestSessionIndexReportsIncompleteForALegacyJournal: a journal that never
// recorded a model (a crash tore the initial model record away, or the log
// predates it) cannot be answered from a fold — LoadSession answers those
// from the loading Config. The index must say so rather than report an
// empty model.
func TestSessionIndexReportsIncompleteForALegacyJournal(t *testing.T) {
	dir := t.TempDir()
	id := "ses_0123456789abcdef"
	for name, journal := range map[string]string{
		"torn model record": `{"type":"session","id":"ses_0123456789abcdef","created_at":"2026-01-02T03:04:05Z","workdir":"/w"}
{"type":"model","mod`,
		"legacy header without workdir": `{"type":"session","id":"ses_0123456789abcdef","created_at":"2026-01-02T03:04:05Z"}
{"type":"model","model":"test/m1"}
`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(dir, name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(journal), 0o644); err != nil {
				t.Fatal(err)
			}
			ix, err := ReadSessionIndex(dir, id)
			if err != nil {
				t.Fatalf("ReadSessionIndex: %v", err)
			}
			if ix.Complete {
				t.Error("Complete = true, want false: the journal records no model or no workdir, so only a load can answer")
			}
		})
	}
}

// TestSessionIndexSurvivesAnOversizedHeader: a session header is not
// bounded in size — ensureLog writes task_tool_names into it. A header
// larger than the first-line peek must not make the session invisible to a
// read or a listing.
func TestSessionIndexSurvivesAnOversizedHeader(t *testing.T) {
	dir := t.TempDir()
	tools := make([]string, 4000)
	for i := range tools {
		tools[i] = "mcp__server__tool_with_a_long_name_" + string(rune('a'+i%26))
	}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
	}}
	cfg := persistCfg(dir, prov)
	cfg.WorkDir = dir
	cfg.TaskParentID = "ses_0123456789abcdef"
	cfg.TaskAgentType = "explore"
	cfg.TaskToolNames = tools
	s := NewSession(cfg)
	runTurns(t, s, 1)

	// Drop the sidecar, forcing the refold path — the one that peeks at the
	// first line before reading the file.
	if err := os.Remove(filepath.Join(dir, s.ID+sessionIndexSuffix)); err != nil {
		t.Fatal(err)
	}
	ix, err := ReadSessionIndex(dir, s.ID)
	if err != nil {
		t.Fatalf("ReadSessionIndex on an oversized header: %v", err)
	}
	if ix.Messages != 2 {
		t.Errorf("Messages = %d, want 2", ix.Messages)
	}
	list, err := ListSessionIndexes(dir)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSessionIndexes = %v, %v; want the one session", list, err)
	}
}

// TestSessionIndexAfterEnsureLogTailRepair drives ensureLog's OWN two
// repair branches, rather than editing a journal by hand, and checks the
// index still agrees with a full load afterwards. Branch 1 truncates a torn
// record away; branch 2 terminates a complete record whose newline never
// landed. The two move the journal's length in opposite directions, and
// logSize has to follow each.
func TestSessionIndexAfterEnsureLogTailRepair(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail string
	}{
		{"torn record is truncated", `{"type":"message","message":{"id":"msg_x`},
		{"complete record is terminated", `{"type":"message","message":{"id":"msg_x","role":"user","parts":[{"type":"text","text":"hi"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
				compactTurn("one", provider.Usage{InputTokens: 10}),
				compactTurn("two", provider.Usage{InputTokens: 20}),
			}}
			cfg := persistCfg(dir, prov)
			first := NewSession(cfg)
			runTurns(t, first, 1)

			// Append an unterminated tail, as a crash mid-write leaves.
			path := filepath.Join(dir, first.ID+".jsonl")
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(tc.tail); err != nil {
				t.Fatal(err)
			}
			f.Close()

			// Resume and write one more record: ensureLog repairs the tail
			// first, and the flush that follows must describe the repaired
			// journal.
			resumed, err := LoadSession(cfg, first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resumed.Prompt(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			if err := resumed.PersistErr(); err != nil {
				t.Fatalf("PersistErr: %v", err)
			}
			assertIndexMatchesLoad(t, cfg, first.ID)

			// And the sidecar must be current, not merely correct after a
			// refold: corrupt the journal at the same key and read again.
			corruptJournalKeepingSize(t, dir, first.ID)
			if _, err := ReadSessionIndex(dir, first.ID); err != nil {
				t.Errorf("sidecar is not current after a tail repair: %v", err)
			}
		})
	}
}

// TestSessionIndexPinsTheRequestedID: the filename names the session, not
// the header record inside it. LoadSession pins the same way, so a journal
// copied to a new name reports the new name on both paths. Without this,
// GET /session/{id} could answer with a different id than it was asked
// about, and every later read would refold.
func TestSessionIndexPinsTheRequestedID(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	runTurns(t, s, 1)

	// Copy the journal under a second id, header and all.
	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const copied = "ses_0123456789abcdef"
	if err := os.WriteFile(filepath.Join(dir, copied+".jsonl"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ix, err := ReadSessionIndex(dir, copied)
	if err != nil {
		t.Fatalf("ReadSessionIndex: %v", err)
	}
	if ix.ID != copied {
		t.Errorf("ID = %q, want %q (the filename names the session)", ix.ID, copied)
	}
	loaded, err := LoadSession(cfg, copied)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != ix.ID {
		t.Errorf("LoadSession reports %q, the index reports %q", loaded.ID, ix.ID)
	}
	// The second read must take the stored index, not refold: an id that
	// never matches would make every read pay a fold forever.
	corruptJournalKeepingSize(t, dir, copied)
	again, err := ReadSessionIndex(dir, copied)
	if err != nil {
		t.Fatalf("second ReadSessionIndex: %v", err)
	}
	if again.Messages != ix.Messages {
		t.Errorf("second read = %d messages, want %d from the stored index", again.Messages, ix.Messages)
	}
}

// TestIndexFoldWorkIsConstantPerRecord is the cost guard for the WRITE
// path. Session.writeRecord folds and flushes after every record, so any
// work here that grows with history length is O(n^2) over a session's life
// — an index that makes reads cheap and writes quadratic is a net loss on
// exactly the long sessions it exists for. A review caught that shape.
//
// Bytes allocated, not elapsed time, are the measurement: they are
// deterministic under a fixed input, and they separate constant work from a
// re-fold cleanly. Object COUNT does not: a re-fold of a skeleton with no
// orphan returns the same slice and allocates one copy, so it looks
// constant while copying the whole history.
//
// The ratio, not an absolute, is the assertion. A skeleton forty times
// longer must not cost anything like forty times as much per record.
func TestIndexFoldWorkIsConstantPerRecord(t *testing.T) {
	build := func(n int) *indexFold {
		f := &indexFold{header: true}
		for i := 0; i < n; i++ {
			role := message.RoleUser
			if i%2 == 1 {
				role = message.RoleAssistant
			}
			f.appendMessage(message.Message{ID: fmt.Sprintf("m%d", i), Role: role})
		}
		return f
	}
	bytesPerOp := func(f *indexFold) uint64 {
		const runs = 200
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for i := 0; i < runs; i++ {
			f.snapshot(1, time.Time{})
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / runs
	}
	small := bytesPerOp(build(100))
	large := bytesPerOp(build(4000))
	// 40x the history. Constant work stays flat; a re-fold copies the whole
	// skeleton every time. Four is generous for the first and unreachable
	// for the second.
	if small > 0 && large > small*4 {
		t.Errorf("snapshot costs %d bytes at 100 messages and %d at 4000: work grows with history length", small, large)
	}
}

// TestIndexFoldRepairCountMatchesAFullRecount: the repair count is
// maintained as records arrive, and a full recount is the truth it must
// equal. This is the drift the incremental path can develop and the oracle
// test cannot see on its own, because a shape whose incremental and full
// answers agree by luck passes both.
func TestIndexFoldRepairCountMatchesAFullRecount(t *testing.T) {
	call := func(id string) message.Parts {
		return message.Parts{&message.ToolCall{CallID: id}}
	}
	result := func(id string) message.Parts {
		return message.Parts{&message.ToolResult{CallID: id}}
	}
	shapes := map[string][]message.Message{
		"plain turns": {
			{ID: "u1", Role: message.RoleUser},
			{ID: "a1", Role: message.RoleAssistant},
		},
		"matched tool call": {
			{ID: "u1", Role: message.RoleUser},
			{ID: "a1", Role: message.RoleAssistant, Parts: call("tc1")},
			{ID: "t1", Role: message.RoleTool, Parts: result("tc1")},
		},
		"orphan at the tail": {
			{ID: "u1", Role: message.RoleUser},
			{ID: "a1", Role: message.RoleAssistant, Parts: call("tc1")},
		},
		"orphan mid-history": {
			{ID: "u1", Role: message.RoleUser},
			{ID: "a1", Role: message.RoleAssistant, Parts: call("tc1")},
			{ID: "u2", Role: message.RoleUser},
			{ID: "a2", Role: message.RoleAssistant, Parts: call("tc2")},
			{ID: "t2", Role: message.RoleTool, Parts: result("tc2")},
		},
		"partial results": {
			{ID: "a1", Role: message.RoleAssistant, Parts: message.Parts{&message.ToolCall{CallID: "tc1"}, &message.ToolCall{CallID: "tc2"}}},
			{ID: "t1", Role: message.RoleTool, Parts: result("tc1")},
		},
		"two orphans in a row": {
			{ID: "a1", Role: message.RoleAssistant, Parts: call("tc1")},
			{ID: "a2", Role: message.RoleAssistant, Parts: call("tc2")},
			{ID: "u1", Role: message.RoleUser},
		},
	}
	for name, msgs := range shapes {
		t.Run(name, func(t *testing.T) {
			f := &indexFold{header: true}
			// Check after EVERY append, not only at the end: the
			// incremental update runs once per message, and a shape that
			// converges at the end can still be wrong in the middle, which
			// is what a reader of a live session would see.
			for i := range msgs {
				f.appendMessage(msgs[i])
				incremental := f.repairs
				f.recountRepairs()
				if incremental != f.repairs {
					t.Fatalf("after %d messages: incremental count %d, full recount %d", i+1, incremental, f.repairs)
				}
				repaired := message.ResolveOrphanToolCalls(append([]message.Message(nil), f.messages...))
				if want := len(repaired) - len(f.messages); f.repairs != want {
					t.Fatalf("after %d messages: count %d, but the repair inserts %d", i+1, f.repairs, want)
				}
			}
		})
	}
}
