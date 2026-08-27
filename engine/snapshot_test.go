package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// foldState is every piece of session state a journal replay reconstructs —
// the exact set a snapshot must carry, and the exact set the
// snapshot-equals-replay invariant (docs/design/journal-snapshotting.md
// §4.3) is stated over.
//
// It is deliberately built by READING the loaded session rather than by
// re-marshaling the snapshot: a test that compared snapshots to snapshots
// would pass for a field the snapshot drops on the floor.
type foldState struct {
	History         json.RawMessage
	Model           message.ModelRef
	Effort          message.Effort
	Usage           provider.Usage
	LastUsage       provider.Usage
	HaveLastUsage   bool
	GoalActive      bool
	GoalCondition   string
	CompactCount    int
	LastCompactedAt string
	Queue           []QueuedPrompt
	QueueNextID     int64
	EnqueueSeq      int64
	ToolResults     map[string]toolResultMeta
	ToolResultNext  int64
	ToolResultBytes int
	MCPSelected     map[string]bool
	SpawnedChildren []string
	Notifications   []taskNotification
	TurnUnsettled   bool
	Committed       *taskNotification
	CreatedAt       string
	WorkDir         string
	ParentSession   string
	TaskParentID    string
	TaskAgentType   string
	TaskDepth       int
}

func foldStateOf(t *testing.T, s *Session) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := json.Marshal(s.history)
	if err != nil {
		t.Fatalf("marshal history: %v", err)
	}
	st := foldState{
		History:         h,
		Model:           s.model,
		Effort:          s.effort,
		Usage:           s.usage,
		LastUsage:       s.lastUsage,
		HaveLastUsage:   s.haveLastUsage,
		GoalActive:      s.goalActive,
		GoalCondition:   s.goalCondition,
		CompactCount:    s.compactCount,
		LastCompactedAt: s.lastCompactedAt.UTC().String(),
		Queue:           s.promptQueue,
		QueueNextID:     s.promptQueueNextID,
		EnqueueSeq:      s.enqueueSeq,
		ToolResults:     s.toolResults,
		ToolResultNext:  s.toolResultNextID,
		ToolResultBytes: s.toolResultBytes,
		MCPSelected:     s.mcpSelected,
		SpawnedChildren: s.spawnedChildIDs,
		Notifications:   s.taskNotifications,
		TurnUnsettled:   s.turnUnsettled,
		Committed:       s.committedOutcome,
		CreatedAt:       s.createdAt.UTC().String(),
		WorkDir:         s.cfg.WorkDir,
		ParentSession:   s.cfg.ParentSession,
		TaskParentID:    s.cfg.TaskParentID,
		TaskAgentType:   s.cfg.TaskAgentType,
		TaskDepth:       s.cfg.TaskDepth,
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal fold state: %v", err)
	}
	return string(b)
}

// snapshotTestProvider returns a scripted provider with n plain assistant
// turns, so a test can drive an arbitrarily long session.
func snapshotTestProvider(n int) *scriptedProvider {
	turns := make([][]provider.Event, 0, n)
	for i := range n {
		turns = append(turns, asstTurn(provider.StopEndTurn, &message.Text{Text: fmt.Sprintf("reply %d", i)}))
	}
	return &scriptedProvider{name: "test", turns: turns}
}

// idleOnly is a cadence so large the every-K trigger never fires in a
// test, leaving the on-idle trigger as the only source of snapshots. It is
// not "off": zero disables snapshot writing entirely (see
// Config.SnapshotEveryRecords), which is what TestSnapshotDisabledWritesNothing
// exercises.
const idleOnly = 1 << 20

// snapshotCfg is persistCfg with an explicit snapshot cadence.
func snapshotCfg(dir string, prov *scriptedProvider, every int) Config {
	cfg := persistCfg(dir, prov)
	cfg.SnapshotEveryRecords = every
	return cfg
}

// buildSession drives turns prompts through s and returns it, draining any
// in-flight snapshot write so the test sees a settled disk.
func drive(t *testing.T, s *Session, turns int) {
	t.Helper()
	for i := range turns {
		if _, err := s.Prompt(context.Background(), fmt.Sprintf("prompt %d", i)); err != nil {
			t.Fatalf("Prompt %d: %v", i, err)
		}
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}
	s.waitSnapshots()
}

// TestSnapshotRoundTrip pins the first half of the §4.3 invariant: a
// session restored from a snapshot taken at seq N holds exactly the state a
// full replay of the same journal at seq N produces. It compares against a
// load with snapshotting switched OFF, so the two paths are compared on the
// same journal bytes rather than against each other's assumptions.
func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(snapshotCfg(dir, snapshotTestProvider(8), 4))
	drive(t, s, 8)

	// The journal head is exactly where the last snapshot landed only by
	// luck, so take an explicit idle snapshot at head first.
	s.snapshotOnIdle()
	s.waitSnapshots()

	snapLoaded, err := LoadSession(snapshotCfg(dir, snapshotTestProvider(0), 4), s.ID)
	if err != nil {
		t.Fatalf("LoadSession (snapshot path): %v", err)
	}
	if snapLoaded.replayedRecords >= snapLoaded.recordsWritten {
		t.Fatalf("replayed %d of %d records — the snapshot was not used", snapLoaded.replayedRecords, snapLoaded.recordsWritten)
	}

	// Full replay of the identical journal: same dir, snapshot removed.
	full := t.TempDir()
	copyFile(t, sessionPath(dir, s.ID), sessionPath(full, s.ID))
	fullLoaded, err := LoadSession(snapshotCfg(full, snapshotTestProvider(0), 4), s.ID)
	if err != nil {
		t.Fatalf("LoadSession (full replay): %v", err)
	}

	if got, want := foldStateOf(t, snapLoaded), foldStateOf(t, fullLoaded); got != want {
		t.Errorf("snapshot-loaded state != full-replay state\n got: %s\nwant: %s", got, want)
	}
}

// TestSnapshotPlusTailEqualsFullReplay pins the whole §4.3 invariant:
// state(snapshot@N) + replay(N+1..head) ≡ full-replay(0..head), at several
// N. Each iteration takes a snapshot at the current head and then appends
// more records, so the snapshot is genuinely BEHIND the journal when it is
// loaded.
func TestSnapshotPlusTailEqualsFullReplay(t *testing.T) {
	for _, tail := range []int{1, 2, 5, 9} {
		t.Run(fmt.Sprintf("tail=%d", tail), func(t *testing.T) {
			dir := t.TempDir()
			// Snapshotting off during the build: this test drives the
			// anchor explicitly so the snapshot sits at a known N.
			s := NewSession(snapshotCfg(dir, snapshotTestProvider(4+tail), idleOnly))
			drive(t, s, 4)
			s.snapshotOnIdle()
			s.waitSnapshots()
			drive(t, s, tail)

			snapLoaded, err := LoadSession(snapshotCfg(dir, snapshotTestProvider(0), 0), s.ID)
			if err != nil {
				t.Fatalf("LoadSession (snapshot path): %v", err)
			}
			if snapLoaded.replayedRecords >= snapLoaded.recordsWritten {
				t.Fatalf("replayed %d of %d records — the snapshot was not used", snapLoaded.replayedRecords, snapLoaded.recordsWritten)
			}

			full := t.TempDir()
			copyFile(t, sessionPath(dir, s.ID), sessionPath(full, s.ID))
			fullLoaded, err := LoadSession(snapshotCfg(full, snapshotTestProvider(0), idleOnly), s.ID)
			if err != nil {
				t.Fatalf("LoadSession (full replay): %v", err)
			}
			if got, want := foldStateOf(t, snapLoaded), foldStateOf(t, fullLoaded); got != want {
				t.Errorf("snapshot+tail state != full-replay state\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// TestSnapshotCarriesEveryFoldedField drives every fold LoadSession runs —
// model, effort, goal, prompt queue, usage — into one session, snapshots
// it, and proves the reload through the snapshot agrees with the full
// replay field for field. It is the guard the design calls for against a
// fold added without a matching snapshot field.
func TestSnapshotCarriesEveryFoldedField(t *testing.T) {
	dir := t.TempDir()
	cfg := snapshotCfg(dir, snapshotTestProvider(3), idleOnly)
	cfg.WorkDir = t.TempDir()
	cfg.ParentSession = "ses_1111111111111111"
	s := NewSession(cfg)

	drive(t, s, 1)
	s.SetModel(message.ModelRef{Provider: "test", Model: "m2"})
	s.SetEffort(message.EffortHigh)
	if err := s.RegisterGoal("ship it"); err != nil {
		t.Fatalf("RegisterGoal: %v", err)
	}
	if _, err := s.EnqueuePrompt("queued one"); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}
	if _, _, err := s.EnqueuePromptDurable("queued two", 7); err != nil {
		t.Fatalf("EnqueuePromptDurable: %v", err)
	}
	drive(t, s, 2)

	s.snapshotOnIdle()
	s.waitSnapshots()

	snapLoaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession (snapshot path): %v", err)
	}
	full := t.TempDir()
	copyFile(t, sessionPath(dir, s.ID), sessionPath(full, s.ID))
	fullCfg := cfg
	fullCfg.SessionDir = full
	fullLoaded, err := LoadSession(fullCfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession (full replay): %v", err)
	}
	if got, want := foldStateOf(t, snapLoaded), foldStateOf(t, fullLoaded); got != want {
		t.Errorf("snapshot-loaded state != full-replay state\n got: %s\nwant: %s", got, want)
	}
	// Spot-check a couple of fields directly, so a bug that drops BOTH
	// paths' state identically still fails.
	if !snapLoaded.goalActive || snapLoaded.goalCondition != "ship it" {
		t.Errorf("goal = (%v, %q), want (true, %q)", snapLoaded.goalActive, snapLoaded.goalCondition, "ship it")
	}
	if got := snapLoaded.Model(); got != (message.ModelRef{Provider: "test", Model: "m2"}) {
		t.Errorf("model = %v, want test/m2", got)
	}
	if len(snapLoaded.promptQueue) != 2 {
		t.Errorf("queue = %d entries, want 2", len(snapLoaded.promptQueue))
	}
}

// TestSnapshotFallbacks covers every way a snapshot can fail validation.
// Each one must degrade to a full replay that produces the correct state —
// "slower, never wrong".
func TestSnapshotFallbacks(t *testing.T) {
	build := func(t *testing.T) (string, string, string) {
		t.Helper()
		dir := t.TempDir()
		s := NewSession(snapshotCfg(dir, snapshotTestProvider(6), idleOnly))
		drive(t, s, 6)
		s.snapshotOnIdle()
		s.waitSnapshots()
		// The reference state, from a full replay of the same journal.
		full := t.TempDir()
		copyFile(t, sessionPath(dir, s.ID), sessionPath(full, s.ID))
		ref, err := LoadSession(snapshotCfg(full, snapshotTestProvider(0), idleOnly), s.ID)
		if err != nil {
			t.Fatalf("reference LoadSession: %v", err)
		}
		return dir, s.ID, foldStateOf(t, ref)
	}

	cases := []struct {
		name    string
		corrupt func(t *testing.T, dir, id string)
	}{
		{"missing", func(t *testing.T, dir, id string) {
			if err := os.Remove(sessionSnapshotPath(dir, id)); err != nil {
				t.Fatal(err)
			}
		}},
		{"checksum mismatch", func(t *testing.T, dir, id string) {
			p := sessionSnapshotPath(dir, id)
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var f sessionSnapshotFile
			if err := json.Unmarshal(data, &f); err != nil {
				t.Fatal(err)
			}
			f.CRC32++
			b, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"seq ahead of head", func(t *testing.T, dir, id string) {
			rewriteSnapshot(t, dir, id, func(snap *sessionSnapshot) {
				snap.Seq += 1000
			})
		}},
		{"wrong version", func(t *testing.T, dir, id string) {
			rewriteSnapshot(t, dir, id, func(snap *sessionSnapshot) {
				snap.Version = sessionSnapshotVersion + 1
			})
		}},
		{"wrong session id", func(t *testing.T, dir, id string) {
			rewriteSnapshot(t, dir, id, func(snap *sessionSnapshot) {
				snap.ID = "ses_9999999999999999"
			})
		}},
		{"garbage", func(t *testing.T, dir, id string) {
			if err := os.WriteFile(sessionSnapshotPath(dir, id), []byte("{not json"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, id, want := build(t)
			tc.corrupt(t, dir, id)

			loaded, err := LoadSession(snapshotCfg(dir, snapshotTestProvider(0), 0), id)
			if err != nil {
				t.Fatalf("LoadSession: %v", err)
			}
			if loaded.replayedRecords != loaded.recordsWritten {
				t.Errorf("replayed %d of %d records, want a FULL replay", loaded.replayedRecords, loaded.recordsWritten)
			}
			if got := foldStateOf(t, loaded); got != want {
				t.Errorf("fallback state != full-replay state\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// TestSnapshotPartialTempFileNeverLoads is the crash-safety case: a
// half-written <id>.snap.tmp is not a snapshot, and the previous <id>.snap
// still stands.
func TestSnapshotPartialTempFileNeverLoads(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(snapshotCfg(dir, snapshotTestProvider(4), idleOnly))
	drive(t, s, 4)
	s.snapshotOnIdle()
	s.waitSnapshots()

	good, err := os.ReadFile(sessionSnapshotPath(dir, s.ID))
	if err != nil {
		t.Fatalf("no snapshot written: %v", err)
	}
	// A crash mid-write leaves the temp file behind, truncated.
	if err := os.WriteFile(sessionSnapshotTmpPath(dir, s.ID), good[:len(good)/2], 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSession(snapshotCfg(dir, snapshotTestProvider(0), 0), s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if loaded.replayedRecords >= loaded.recordsWritten {
		t.Errorf("replayed %d of %d records — the surviving snapshot was ignored", loaded.replayedRecords, loaded.recordsWritten)
	}
	if got, want := len(loaded.History()), len(s.History()); got != want {
		t.Errorf("history = %d messages, want %d", got, want)
	}
	// And a temp file with NO snapshot beside it loads nothing at all.
	if err := os.Remove(sessionSnapshotPath(dir, s.ID)); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadSession(snapshotCfg(dir, snapshotTestProvider(0), 0), s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if loaded.replayedRecords != loaded.recordsWritten {
		t.Errorf("replayed %d of %d records, want a FULL replay", loaded.replayedRecords, loaded.recordsWritten)
	}
}

// TestSnapshotBoundsReplayedRecords is the point of the whole feature: a
// long session's load replays at most K records however long the journal
// grows. It asserts the REPLAYED-RECORD COUNT, never a wall-clock number
// and never a raw total.
func TestSnapshotBoundsReplayedRecords(t *testing.T) {
	const every = 8
	dir := t.TempDir()
	s := NewSession(snapshotCfg(dir, snapshotTestProvider(60), every))

	var maxReplayed int64
	for turn := range 60 {
		if _, err := s.Prompt(context.Background(), fmt.Sprintf("p%d", turn)); err != nil {
			t.Fatalf("Prompt %d: %v", turn, err)
		}
		s.waitSnapshots()
		loaded, err := LoadSession(snapshotCfg(dir, snapshotTestProvider(0), every), s.ID)
		if err != nil {
			t.Fatalf("LoadSession after turn %d: %v", turn, err)
		}
		if loaded.replayedRecords > maxReplayed {
			maxReplayed = loaded.replayedRecords
		}
		if loaded.replayedRecords > every {
			t.Fatalf("after turn %d: replayed %d records (journal head %d), want <= K=%d",
				turn, loaded.replayedRecords, loaded.recordsWritten, every)
		}
	}
	// The journal is far longer than K, so a bound that held only because
	// nothing was ever written would be no evidence at all.
	if s.recordsWritten <= every {
		t.Fatalf("journal head = %d records, want well past K=%d", s.recordsWritten, every)
	}
	if maxReplayed == 0 {
		t.Fatal("no load ever replayed a record — the test measured nothing")
	}
}

// TestSnapshotDisabledWritesNothing pins the config seam: a session with
// snapshotting off writes no .snap at all and loads exactly as it always
// has.
func TestSnapshotDisabledWritesNothing(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(snapshotCfg(dir, snapshotTestProvider(20), 0))
	drive(t, s, 20)

	if _, err := os.Stat(sessionSnapshotPath(dir, s.ID)); !os.IsNotExist(err) {
		t.Errorf("stat .snap = %v, want not-exist with snapshotting disabled", err)
	}
	loaded, err := LoadSession(snapshotCfg(dir, snapshotTestProvider(0), 0), s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if loaded.replayedRecords != loaded.recordsWritten {
		t.Errorf("replayed %d of %d records, want a FULL replay", loaded.replayedRecords, loaded.recordsWritten)
	}
	if got, want := len(loaded.History()), len(s.History()); got != want {
		t.Errorf("history = %d messages, want %d", got, want)
	}
}

// TestSnapshotWritesAreCoalesced pins rule 4: only one snapshot per session
// is ever in flight, so the every-K and on-idle triggers cannot race to
// write the same file.
func TestSnapshotWritesAreCoalesced(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(snapshotCfg(dir, snapshotTestProvider(4), 2))
	drive(t, s, 4)
	for range 10 {
		s.snapshotOnIdle()
	}
	s.mu.Lock()
	inFlight := s.snapshotting
	s.mu.Unlock()
	if inFlight {
		// One may legitimately still be running; what must never happen is
		// two. The counter below is the real assertion.
		_ = inFlight
	}
	s.waitSnapshots()
	if got := s.snapshotWrites.Load(); got == 0 {
		t.Fatal("no snapshot written")
	}
	if got := s.snapshotConcurrentPeak.Load(); got > 1 {
		t.Errorf("peak concurrent snapshot writers = %d, want 1", got)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// rewriteSnapshot mutates a stored snapshot and rewrites it WITH a valid
// checksum, so the test exercises the field's own validation rather than
// the checksum's.
func rewriteSnapshot(t *testing.T, dir, id string, fn func(*sessionSnapshot)) {
	t.Helper()
	data, err := os.ReadFile(sessionSnapshotPath(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	var f sessionSnapshotFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	var snap sessionSnapshot
	if err := json.Unmarshal(f.Snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	fn(&snap)
	b, err := marshalSessionSnapshot(&snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionSnapshotPath(dir, id), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotRefusesWhileMemoryIsAheadOfJournal pins the capture
// precondition (snapshotSafeLocked): SessionManager splits some mutations
// into an in-memory half and a DEFERRED durable half, and a snapshot taken
// between the two would carry the mutation while leaving its record in the
// tail — so the reload would apply it twice.
//
// It drives the production pair directly, in the order SessionManager runs
// it, and asserts on the reloaded state rather than on the guard's own
// boolean: a duplicate message is the defect, and the reload is where it
// shows.
func TestSnapshotRefusesWhileMemoryIsAheadOfJournal(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(snapshotCfg(dir, snapshotTestProvider(1), idleOnly))
	drive(t, s, 1)

	closing := s.appendMemoryOnly(message.Message{
		ID:    "msg_deferred",
		Role:  message.RoleAssistant,
		Parts: message.Parts{&message.Text{Text: "lost to restart"}},
	})
	// The window: memory holds the message, the journal does not. Advance
	// the journal with an unrelated record so a snapshot is due, then try
	// to take one — this is exactly the state the guard must refuse.
	s.SetModel(message.ModelRef{Provider: "test", Model: "m2"})
	s.snapshotOnIdle()
	s.waitSnapshots()
	s.persistAppendedMessage(closing)
	s.waitSnapshots()

	loaded, err := LoadSession(snapshotCfg(dir, snapshotTestProvider(0), idleOnly), s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	var n int
	for _, m := range loaded.History() {
		if m.ID == "msg_deferred" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("reloaded history holds %d copies of the deferred message, want 1", n)
	}
	if got, want := len(loaded.History()), len(s.History()); got != want {
		t.Errorf("history = %d messages, want %d", got, want)
	}
}

// TestSnapshotRefusesWhileNotificationIsAheadOfJournal is the same
// precondition for the OTHER split pair: a child-completion notification
// appended to memory under m.mu, with its record written after m.mu
// releases. A snapshot in that window duplicates the notification on
// reload, which the parent renders to the model as two completed children.
func TestSnapshotRefusesWhileNotificationIsAheadOfJournal(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(snapshotCfg(dir, snapshotTestProvider(1), idleOnly))
	drive(t, s, 1)

	n := taskNotification{ChildID: "ses_2222222222222222", Agent: "explore", Status: StatusDone, Result: "done"}
	s.enqueueTaskNotificationMemoryOnly(n)
	// The window: memory holds the notification, the journal does not.
	// Advance the journal with an unrelated record so a snapshot is due,
	// then try to take one — exactly the state the guard must refuse.
	s.SetModel(message.ModelRef{Provider: "test", Model: "m2"})
	s.snapshotOnIdle()
	s.waitSnapshots()
	s.persistQueuedTaskNotification(n)
	s.waitSnapshots()

	loaded, err := LoadSession(snapshotCfg(dir, snapshotTestProvider(0), idleOnly), s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	loaded.mu.Lock()
	got := len(loaded.taskNotifications)
	loaded.mu.Unlock()
	if got != 1 {
		t.Errorf("reloaded session holds %d pending notifications, want 1", got)
	}
}

// TestSnapshotAnchorSurvivesTornTail pins the anchor's domain: seq is a
// journal LINE NUMBER, and a crash mid-write leaves a torn final line that
// the load tolerates (scanLog drops it) and the next write repairs away
// (ensureLog truncates it). A resumed session that counted that dropped
// line would take every later anchor one line too high — permanently ahead
// of the journal head, so every one of its snapshots would be rejected as
// seq-ahead and the session would never load fast again.
func TestSnapshotAnchorSurvivesTornTail(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(snapshotCfg(dir, snapshotTestProvider(2), idleOnly))
	drive(t, s, 2)

	// A crash mid-write: a partial record with no trailing newline.
	f, err := os.OpenFile(sessionPath(dir, s.ID), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message","message":{"id":"msg_torn","ro`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg := snapshotCfg(dir, snapshotTestProvider(1), idleOnly)
	resumed, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession over torn tail: %v", err)
	}
	if _, err := resumed.Prompt(context.Background(), "after the crash"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if err := resumed.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}
	resumed.waitSnapshots()

	reloaded, err := LoadSession(snapshotCfg(dir, snapshotTestProvider(0), idleOnly), s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if reloaded.replayedRecords >= reloaded.recordsWritten {
		t.Errorf("replayed %d of %d records — the anchor taken after a torn tail was rejected",
			reloaded.replayedRecords, reloaded.recordsWritten)
	}
	if got, want := len(reloaded.History()), len(resumed.History()); got != want {
		t.Errorf("history = %d messages, want %d", got, want)
	}
}
