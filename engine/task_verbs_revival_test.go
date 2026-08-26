// Tests for reviving a Reap()-ed descendant across the `task` tool's four
// verbs (cancel/status/send/log). Reap collects a done/failed/canceled
// LEAF the instant it settles (Reap's own doc comment, session_manager.go)
// — before this fix, a caller that spawned that child and asked about it
// again after that instant, but before observing Reap's own internal
// timing, got "no such session" for a descendant it plainly still owned.
// resolveOrReviveDescendantLocked closes that gap by falling back to a
// disk-backed resolution, validated against the descendant's own durable
// TaskParentID chain, whenever a live-tree lookup misses. See that
// method's own doc comment, and each of CancelDescendant/DescendantInfo/
// DescendantTranscript/SendToDescendant's, for the full design.
package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/provider"
	"os"
)

// settleAndReapChild spawns a child of parentID that immediately completes
// (scriptedTurns("child", doneTurn(...)) style provider registered under
// model "child"), waits for it to go StatusDone, force-reaps it via the
// production Reap() entry point (no special test hook needed — Reap is
// already exported and callable directly, exactly as
// engine/session_manager_test.go's own waitForReap does), and returns its
// id. cfg.SessionDir MUST already be set on mgr's root — LoadSession (the
// mechanism under test) requires it.
func settleAndReapChild(t *testing.T, mgr *SessionManager, parentID string, agentType string) string {
	t.Helper()
	childID, err := mgr.Spawn(SpawnOptions{ParentID: parentID, Prompt: "go", Model: modelFor("child"), AgentType: agentType})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
	waitForReap(t, mgr, 1, time.Second, "settled child never became reapable")
	if _, ok := mgr.Session(childID); ok {
		t.Fatalf("child %s still tracked after Reap: test setup did not actually reap it", childID)
	}
	return childID
}

// TestSendToDescendantRevivesReapedChild is the headline regression test:
// a settled child, reaped out of the live tree, gets a genuinely NEW turn
// from `send` — proven structurally (blockAfterFirstProvider's second call
// blocks until release, exactly like TestSendToDescendantSettledRelaunches
// Asynchronously's identical proof for the settled-but-unreaped case this
// generalizes), not merely a replay of the first.
//
// Red-verified: reverting resolveOrReviveDescendantLocked's disk fallback
// (making SendToDescendant answer ErrUnknownSession on a live-tree miss,
// the pre-fix behavior) turns this red with exactly the error the live
// incident reported — `engine: unknown session id`.
func TestSendToDescendantRevivesReapedChild(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	childProv := &blockAfterFirstProvider{name: "child", release: release}
	cfg := managedConfig("root", scriptedTurns("root", nil), childProv)
	cfg.SessionDir = dir
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)

	childID := settleAndReapChild(t, mgr, root.ID, AgentGeneralPurpose)

	queued, err := mgr.SendToDescendant(root.ID, childID, "please redo this")
	if err != nil {
		t.Fatalf("SendToDescendant on a reaped child: %v", err)
	}
	if queued {
		t.Error("SendToDescendant on a reaped (settled) child: queued = true, want false (a fresh re-run turn, not an enqueue)")
	}

	// The child must be back in the live tree, genuinely running its
	// second turn — not merely reporting an outcome from thin air.
	info, ok := mgr.Info(childID)
	if !ok {
		t.Fatal("Info(childID) right after SendToDescendant returned: not tracked, want the revived node")
	}
	if info.Status != StatusRunning {
		t.Errorf("revived child status right after SendToDescendant returned = %s, want %s", info.Status, StatusRunning)
	}
	if info.ParentID != root.ID {
		t.Errorf("revived child ParentID = %q, want %q (root)", info.ParentID, root.ID)
	}
	if info.AgentType != AgentGeneralPurpose {
		t.Errorf("revived child AgentType = %q, want %q", info.AgentType, AgentGeneralPurpose)
	}

	close(release)
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
}

// TestDescendantInfoServesReapedChildWithoutReadopting proves the `status`
// verb answers correctly for a reaped descendant AND that answering it has
// no side effect on the tree: status is read-only, so the child must stay
// exactly as absent from mgr.nodes after the call as it was before — see
// DescendantInfo's own doc comment for why re-adopting on a read would be
// wrong (pinning memory, and double-extending the child's budget credit
// window, purely for having been asked about).
func TestDescendantInfoServesReapedChildWithoutReadopting(t *testing.T) {
	dir := t.TempDir()
	usage := provider.Usage{InputTokens: 7, OutputTokens: 5, CacheReadTokens: 1}
	cfg := managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", doneTurnWithUsage("the answer", usage)))
	cfg.SessionDir = dir
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)

	childID := settleAndReapChild(t, mgr, root.ID, AgentExplore)

	node, gotUsage, err := mgr.DescendantInfo(root.ID, childID)
	if err != nil {
		t.Fatalf("DescendantInfo on a reaped child: %v", err)
	}
	if node.ID != childID {
		t.Errorf("ID = %q, want %q", node.ID, childID)
	}
	if node.ParentID != root.ID {
		t.Errorf("ParentID = %q, want %q", node.ParentID, root.ID)
	}
	if node.Depth != 1 {
		t.Errorf("Depth = %d, want 1", node.Depth)
	}
	if node.Status != StatusDone {
		t.Errorf("Status = %s, want done", node.Status)
	}
	if node.AgentType != AgentExplore {
		t.Errorf("AgentType = %q, want %q", node.AgentType, AgentExplore)
	}
	if node.Result != "the answer" {
		t.Errorf("Result = %q, want %q", node.Result, "the answer")
	}
	if gotUsage != usage {
		t.Errorf("Usage = %+v, want %+v", gotUsage, usage)
	}

	if _, ok := mgr.Session(childID); ok {
		t.Error("child is tracked again after a read-only DescendantInfo call: status must not re-adopt a reaped descendant")
	}
}

// TestDescendantTranscriptServesReapedChild proves the `log` verb reads a
// reaped descendant's transcript straight off a disk-loaded *Session,
// without re-adopting it — the log-verb counterpart to
// TestDescendantInfoServesReapedChildWithoutReadopting.
func TestDescendantTranscriptServesReapedChild(t *testing.T) {
	dir := t.TempDir()
	cfg := managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", doneTurn("child's final answer")))
	cfg.SessionDir = dir
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)

	childID := settleAndReapChild(t, mgr, root.ID, AgentGeneralPurpose)

	node, msgs, total, err := mgr.DescendantTranscript(root.ID, childID, 20)
	if err != nil {
		t.Fatalf("DescendantTranscript on a reaped child: %v", err)
	}
	if node.Status != StatusDone {
		t.Errorf("Status = %s, want done", node.Status)
	}
	if total == 0 || len(msgs) == 0 {
		t.Fatalf("DescendantTranscript returned no messages for a settled child (total=%d, len(msgs)=%d)", total, len(msgs))
	}
	found := false
	// Render via the same helper the `task` tool's log action itself uses,
	// so this assertion exercises the identical rendering path a model
	// would see.
	entries := renderTaskLog(msgs)
	for _, e := range entries {
		if e.Text == "child's final answer" {
			found = true
		}
	}
	if !found {
		t.Errorf("rendered log entries %+v do not contain the child's final answer", entries)
	}

	if _, ok := mgr.Session(childID); ok {
		t.Error("child is tracked again after a read-only DescendantTranscript call: log must not re-adopt a reaped descendant")
	}
}

// TestCancelDescendantNoOpsOnReapedChild proves `cancel` against a reaped
// descendant is a no-op success: it reports the descendant's real terminal
// status (never StatusCanceled — nothing was actually canceled) and does
// not re-adopt it — see CancelDescendant's own doc comment for why a
// reaped target can never have had genuine in-flight work to interrupt.
func TestCancelDescendantNoOpsOnReapedChild(t *testing.T) {
	dir := t.TempDir()
	cfg := managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", doneTurn("done")))
	cfg.SessionDir = dir
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)

	childID := settleAndReapChild(t, mgr, root.ID, AgentGeneralPurpose)

	status, err := mgr.CancelDescendant(root.ID, childID)
	if err != nil {
		t.Fatalf("CancelDescendant on a reaped child: %v", err)
	}
	if status != StatusDone {
		t.Errorf("CancelDescendant on a reaped, already-done child: status = %s, want %s (nothing was actually canceled)", status, StatusDone)
	}
	if _, ok := mgr.Session(childID); ok {
		t.Error("child is tracked again after a no-op CancelDescendant call: cancel must not re-adopt a reaped descendant")
	}
}

// TestSendToDescendantUnknownIdStillErrors proves the disk fallback does
// not turn every unresolvable id into a silent success: an id with no
// live node AND no session log on disk (never existed at all) still
// answers ErrUnknownSession, exactly as it did before this fix, now
// exercising the code path that actually attempts (and fails) a
// LoadSession rather than skipping straight to the live-tree miss.
func TestSendToDescendantUnknownIdStillErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := managedConfig("root", scriptedTurns("root", nil))
	cfg.SessionDir = dir
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)

	if _, err := mgr.SendToDescendant(root.ID, "ses_0000000000000000", "hi"); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("SendToDescendant(root, neverexisted): err = %v, want ErrUnknownSession", err)
	}
	if _, _, err := mgr.DescendantInfo(root.ID, "ses_0000000000000000"); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("DescendantInfo(root, neverexisted): err = %v, want ErrUnknownSession", err)
	}
}

// TestSendToDescendantAncestryViolationForReapedNonDescendant proves the
// disk fallback still enforces "only your own ancestors" for a reaped
// target: a child reaped under ONE root must still refuse an unrelated
// root's send/status/cancel, exactly as an ancestry violation refuses a
// LIVE non-descendant — the disk-resolved case is not a backdoor around
// isDescendantLocked's own rule.
func TestSendToDescendantAncestryViolationForReapedNonDescendant(t *testing.T) {
	dir := t.TempDir()
	cfg := managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("other", nil),
		scriptedTurns("child", doneTurn("done")),
	)
	cfg.SessionDir = dir
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)
	otherRoot := mgr.NewRoot(Config{Providers: cfg.Providers, Model: modelFor("other"), System: cfg.System, SessionDir: dir})

	childID := settleAndReapChild(t, mgr, root.ID, AgentGeneralPurpose)

	if _, err := mgr.SendToDescendant(otherRoot.ID, childID, "hi"); !errors.Is(err, ErrNotDescendant) {
		t.Errorf("SendToDescendant from an unrelated root against a reaped non-descendant: err = %v, want ErrNotDescendant", err)
	}
	if _, _, err := mgr.DescendantInfo(otherRoot.ID, childID); !errors.Is(err, ErrNotDescendant) {
		t.Errorf("DescendantInfo from an unrelated root against a reaped non-descendant: err = %v, want ErrNotDescendant", err)
	}
	if _, err := mgr.CancelDescendant(otherRoot.ID, childID); !errors.Is(err, ErrNotDescendant) {
		t.Errorf("CancelDescendant from an unrelated root against a reaped non-descendant: err = %v, want ErrNotDescendant", err)
	}
	if _, ok := mgr.Session(childID); ok {
		t.Error("child is tracked again after refused ancestry-violation calls: a refusal must not adopt anything")
	}
}

// TestSendToDescendantRevivalDoesNotDoubleCreditUsage proves requirement
// #3 (budgetedByChild survives Reap by design specifically so a re-adopt
// cannot double-credit it — see that field's own doc comment): reviving a
// reaped child via `send` and letting it complete a SECOND turn must fold
// only the second turn's OWN new usage into usageByRoot, never re-add the
// first turn's already-credited usage a second time.
func TestSendToDescendantRevivalDoesNotDoubleCreditUsage(t *testing.T) {
	dir := t.TempDir()
	firstUsage := provider.Usage{InputTokens: 100, OutputTokens: 50}
	secondUsage := provider.Usage{InputTokens: 10, OutputTokens: 5}
	// A two-turn script: the child's FIRST Spawn'd turn serves firstUsage,
	// and its SECOND turn — the one `send` launches after reviving it —
	// serves secondUsage. scriptedTurns serves one turn per call, in
	// order, so no blocking/release machinery is needed to pin the two
	// turns apart.
	turns := append(doneTurnWithUsage("first", firstUsage), doneTurnWithUsage("second", secondUsage)...)
	cfg := managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", turns))
	cfg.SessionDir = dir
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)

	childID := settleAndReapChild(t, mgr, root.ID, AgentGeneralPurpose)

	mgr.mu.Lock()
	before := mgr.usageByRoot[root.ID]
	mgr.mu.Unlock()
	if before != firstUsage {
		t.Fatalf("usageByRoot[root] after the first turn = %+v, want %+v (the baseline this test's revival step must not double-count)", before, firstUsage)
	}

	queued, err := mgr.SendToDescendant(root.ID, childID, "again")
	if err != nil {
		t.Fatalf("SendToDescendant on a reaped child: %v", err)
	}
	if queued {
		t.Fatal("SendToDescendant on a reaped, settled child: queued = true, want false")
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	mgr.mu.Lock()
	after := mgr.usageByRoot[root.ID]
	mgr.mu.Unlock()

	want := provider.Usage{
		InputTokens:  firstUsage.InputTokens + secondUsage.InputTokens,
		OutputTokens: firstUsage.OutputTokens + secondUsage.OutputTokens,
	}
	if after != want {
		t.Errorf("usageByRoot[root] after the revived child's second turn = %+v, want %+v (firstUsage+secondUsage exactly once each — a mismatch means the reap+revive double- or under-credited)", after, want)
	}
}

// TestSendToDescendantRevivalSingleWinnerUnderConcurrentAdopt hammers the
// concurrency requirement: a `send`-driven revival racing a CONCURRENT,
// independent AdoptReloaded of the SAME reaped id (simulating, e.g., the
// server's own handleSpawnChild parent-lookup fallback touching the same
// id at the same moment) must produce exactly one live node for childID —
// never two competing *Session objects backing the same on-disk log, and
// never a corrupted parent.children list (a double-adopt would append
// childID to root's children twice). Run with `-race -count=1000
// GOMAXPROCS=2` to hammer the race per AGENTS.md's concurrency-testing
// rule; a single run here still exercises the same code path
// deterministically enough to catch a gross ordering bug and, under
// -race, any unsynchronized access.
func TestSendToDescendantRevivalSingleWinnerUnderConcurrentAdopt(t *testing.T) {
	dir := t.TempDir()
	// Two scripted turns: the child's original Spawn'd turn, and the
	// SECOND turn SendToDescendant's settled-target restart always
	// launches, regardless of which of the two racing goroutines below
	// happens to win the adopt itself — AdoptReloaded never launches a
	// turn on its own, only SendToDescendant's own settled-target branch
	// does, so exactly one fresh turn always follows the race, never two.
	childTurns := append(doneTurn("first"), doneTurn("second")...)
	cfg := managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", childTurns))
	cfg.SessionDir = dir
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)

	childID := settleAndReapChild(t, mgr, root.ID, AgentGeneralPurpose)

	loadCfg := root.configSnapshot()

	var wg sync.WaitGroup
	wg.Add(2)
	var sendErr error
	go func() {
		defer wg.Done()
		_, sendErr = mgr.SendToDescendant(root.ID, childID, "revive via send")
	}()
	go func() {
		defer wg.Done()
		if loaded, err := LoadSession(loadCfg, childID); err == nil {
			_ = mgr.AdoptReloaded(loaded) // "already managed" ignored on the loser, by design — see AdoptReloaded's own doc comment
		}
	}()
	wg.Wait()

	if sendErr != nil {
		t.Fatalf("SendToDescendant racing a concurrent AdoptReloaded: %v", sendErr)
	}

	mgr.mu.Lock()
	n, ok := mgr.nodes[childID]
	var childCount int
	if p, pok := mgr.nodes[root.ID]; pok {
		for _, cid := range p.children {
			if cid == childID {
				childCount++
			}
		}
	}
	mgr.mu.Unlock()

	if !ok {
		t.Fatal("childID not tracked after the race: revival was lost entirely")
	}
	if childCount != 1 {
		t.Errorf("root.children contains childID %d time(s), want exactly 1 (a double-adopt corrupted the tree)", childCount)
	}
	if n.parentID != root.ID {
		t.Errorf("revived node ParentID = %q, want %q", n.parentID, root.ID)
	}

	waitForStatus(t, mgr, childID, StatusDone, time.Second)
}

// TestLoadSessionTaskParentReadsHeaderOnly proves the ancestry walk's
// header read returns the durable TaskParentID from the first line alone,
// and fails loudly on a file whose first record is not a header.
func TestLoadSessionTaskParentReadsHeaderOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SessionDir: dir}
	s := NewSession(Config{SessionDir: dir, TaskParentID: "ses_parent00000000000000000"})
	if err := s.ensureLog(); err != nil {
		t.Fatal(err)
	}
	got, err := loadSessionTaskParent(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ses_parent00000000000000000" {
		t.Fatalf("loadSessionTaskParent = %q", got)
	}
	// A non-header first line errors instead of guessing.
	bad := s.ID[:len(s.ID)-1] + "x"
	if err := os.WriteFile(sessionPath(dir, bad), []byte("{\"type\":\"model\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSessionTaskParent(cfg, bad); err == nil {
		t.Fatal("want error for non-header first record")
	}
}
