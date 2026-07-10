package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newGitRepo creates a fresh git repository with one commit, so tests have a
// real repo to point workdir_isolation=worktree at.
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-q")
	runTestGit(t, dir, "config", "user.email", "test@example.com")
	runTestGit(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "seed.txt")
	runTestGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// --- non-git 400 ---

// TestCreateSessionWorktreeIsolationRejectsNonGitWorkdir verifies that
// workdir_isolation=worktree against a directory that is not inside any git
// repository fails the session-create request outright with a clear 400,
// rather than silently degrading to shared-workdir behavior.
func TestCreateSessionWorktreeIsolationRejectsNonGitWorkdir(t *testing.T) {
	dir := t.TempDir() // deliberately not a git repo
	h := newWorkdirHarness(t, &scriptedProvider{name: "test"}, []string{dir})
	resp, data := h.do("POST", "/session", map[string]any{
		"workdir": dir, "workdir_isolation": "worktree",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create status %d, want 400: %s", resp.StatusCode, data)
	}
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(data, &e)
	if e.Error == "" {
		t.Fatalf("400 body missing error: %s", data)
	}

	// No session was created: the list stays empty.
	resp, data = h.do("GET", "/session", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status %d: %s", resp.StatusCode, data)
	}
	var sessions []json.RawMessage
	if err := json.Unmarshal(data, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected no session created on the 400 path, got %d", len(sessions))
	}
}

// TestCreateSessionUnknownIsolationRejected verifies workdir_isolation is a
// closed enum: anything other than "shared"/"worktree" (including simply
// misspelling it) is a 400, not a silent fall-back to shared.
func TestCreateSessionUnknownIsolationRejected(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("POST", "/session", map[string]any{"workdir_isolation": "clone"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create status %d, want 400: %s", resp.StatusCode, data)
	}
}

// --- isolation between two concurrent worktree sessions ---

// TestWorktreeIsolationSessionsDoNotShareWrites verifies the structural
// isolation property the whole feature exists for: two 'worktree' sessions
// pointed at the same repo get distinct checkout directories (neither of
// which is the original repo working tree), and a file written into one is
// invisible from the other and from the original repo.
func TestWorktreeIsolationSessionsDoNotShareWrites(t *testing.T) {
	repo := newGitRepo(t)
	h := newWorkdirHarness(t, &scriptedProvider{name: "test"}, []string{repo})

	_, dataA := h.do("POST", "/session", map[string]any{
		"model": "test/m1", "workdir": repo, "workdir_isolation": "worktree",
	})
	_, dataB := h.do("POST", "/session", map[string]any{
		"model": "test/m1", "workdir": repo, "workdir_isolation": "worktree",
	})
	wdA := sessionWorkDir(t, dataA)
	wdB := sessionWorkDir(t, dataB)

	if wdA == "" || wdB == "" {
		t.Fatalf("expected non-empty worktree paths, got %q / %q", wdA, wdB)
	}
	if wdA == wdB {
		t.Fatalf("expected distinct worktree paths for the two sessions, both got %q", wdA)
	}
	if wdA == repo || wdB == repo {
		t.Fatalf("worktree path must not be the original repo (%s): got %q / %q", repo, wdA, wdB)
	}

	if err := os.WriteFile(filepath.Join(wdA, "only_a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wdB, "only_b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(wdB, "only_a.txt")); !os.IsNotExist(err) {
		t.Errorf("session B's worktree can see session A's write (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(wdA, "only_b.txt")); !os.IsNotExist(err) {
		t.Errorf("session A's worktree can see session B's write (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "only_a.txt")); !os.IsNotExist(err) {
		t.Errorf("original repo can see session A's write (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "only_b.txt")); !os.IsNotExist(err) {
		t.Errorf("original repo can see session B's write (err=%v)", err)
	}
}

// --- claim bypass ---

// TestWorktreeIsolationClaimBypass mirrors TestPromptSameWorkdirBusyRejected
// but with workdir_isolation=worktree on both sides: session A holds the
// same *input* workdir busy, yet session B's prompt is still accepted — the
// workdir-busy claim never applies to 'worktree' sessions.
func TestWorktreeIsolationClaimBypass(t *testing.T) {
	repo := newGitRepo(t)
	prov := newBlockingProvider("test")
	h := newWorkdirHarness(t, prov, []string{repo})
	t.Cleanup(prov.releaseAll)

	idA := h.createSessionBody(map[string]any{
		"model": "test/m1", "workdir": repo, "workdir_isolation": "worktree",
	})
	idB := h.createSessionBody(map[string]any{
		"model": "test/m1", "workdir": repo, "workdir_isolation": "worktree",
	})

	resp, data := h.do("POST", "/session/"+idA+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt A status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // A is now blocked mid-stream, holding its own worktree

	resp, data = h.do("POST", "/session/"+idB+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "second"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt B (worktree isolation) status %d, want 202 (claim must not apply): %s", resp.StatusCode, data)
	}
}

// --- session end: clean removal ---

// TestWorktreeIsolationCleanRemovedOnEnd verifies that a 'worktree' session
// with no changes at all is removed — directory and all — on DELETE
// /session/{id}.
func TestWorktreeIsolationCleanRemovedOnEnd(t *testing.T) {
	repo := newGitRepo(t)
	h := newWorkdirHarness(t, &scriptedProvider{name: "test"}, []string{repo})

	id := h.createSessionBody(map[string]any{
		"model": "test/m1", "workdir": repo, "workdir_isolation": "worktree",
	})
	_, data := h.do("GET", "/session/"+id, nil)
	wd := sessionWorkDir(t, data)
	if _, err := os.Stat(wd); err != nil {
		t.Fatalf("worktree missing before end: %v", err)
	}

	resp, data := h.do("DELETE", "/session/"+id, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("end status %d, want 204: %s", resp.StatusCode, data)
	}
	if _, err := os.Stat(wd); !os.IsNotExist(err) {
		t.Errorf("expected clean worktree removed on session end, stat err=%v", err)
	}
}

// --- session end: dirty preservation + journaled record ---

// TestWorktreeIsolationDirtyKeptAndJournaled verifies that a 'worktree'
// session with uncommitted changes is left in place on DELETE
// /session/{id} — never destroyed — and that a workdir.worktree_kept durable
// record naming its path is journaled, so an orchestrator can find the work.
func TestWorktreeIsolationDirtyKeptAndJournaled(t *testing.T) {
	repo := newGitRepo(t)
	h := newWorkdirHarness(t, &scriptedProvider{name: "test"}, []string{repo})

	id := h.createSessionBody(map[string]any{
		"model": "test/m1", "workdir": repo, "workdir_isolation": "worktree",
	})
	_, data := h.do("GET", "/session/"+id, nil)
	wd := sessionWorkDir(t, data)

	if err := os.WriteFile(filepath.Join(wd, "dirty.txt"), []byte("uncommitted work"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, data := h.do("DELETE", "/session/"+id, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("end status %d, want 204: %s", resp.StatusCode, data)
	}
	if _, err := os.Stat(wd); err != nil {
		t.Fatalf("expected dirty worktree kept in place, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(wd, "dirty.txt")); err != nil {
		t.Fatalf("expected the uncommitted file to survive, stat err=%v", err)
	}

	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	found := false
	for _, ev := range h.srv.journal {
		if ev.Type == evtWorktreeKept && ev.SessionID == id && ev.WorktreePath == wd {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a workdir.worktree_kept record for session %s path %s in the journal, got %+v", id, wd, h.srv.journal)
	}
}

// --- startup sweep ---

// TestServeStartSweepsStaleWorktrees simulates a crashed process: two
// worktrees (one clean, one dirty) exist on disk with their meta files but
// no resident session — as if the previous server died right after creating
// them. Building a fresh *Server against the same SessionDir must sweep at
// construction: the clean one is removed, the dirty one is kept and
// journaled.
func TestServeStartSweepsStaleWorktrees(t *testing.T) {
	repo := newGitRepo(t)
	sessDir := t.TempDir()
	base := filepath.Join(sessDir, "worktrees")

	cleanPath := filepath.Join(base, "wt_clean")
	cleanBase, err := addWorktree(repo, cleanPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeWorktreeMeta(base, "wt_clean", worktreeMeta{
		SessionID: "ses_fakeclean0000", RepoRoot: repo, Path: cleanPath, BaseCommit: cleanBase,
	}); err != nil {
		t.Fatal(err)
	}

	dirtyPath := filepath.Join(base, "wt_dirty")
	dirtyBase, err := addWorktree(repo, dirtyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirtyPath, "uncommitted.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeWorktreeMeta(base, "wt_dirty", worktreeMeta{
		SessionID: "ses_fakedirty0000", RepoRoot: repo, Path: dirtyPath, BaseCommit: dirtyBase,
	}); err != nil {
		t.Fatal(err)
	}

	srv := newServer(t, sessDir, &scriptedProvider{name: "test"}, 0)

	if _, err := os.Stat(cleanPath); !os.IsNotExist(err) {
		t.Errorf("expected the clean leftover worktree to be removed by the startup sweep, stat err=%v", err)
	}
	if _, err := os.Stat(dirtyPath); err != nil {
		t.Errorf("expected the dirty leftover worktree to be kept by the startup sweep, stat err=%v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	found := false
	for _, ev := range srv.journal {
		if ev.Type == evtWorktreeKept && ev.SessionID == "ses_fakedirty0000" && ev.WorktreePath == dirtyPath {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a workdir.worktree_kept record for the dirty leftover, got %+v", srv.journal)
	}
}

// ============================================================================
// All five tests below (FINDING 1's a/b/c, FINDING 2's two) are written
// against this exact commit's UNMODIFIED code — sweepWorktrees's current
// two-argument signature, createWorktreeForSession's current
// addWorktree-then-writeWorktreeMeta ordering. They are run once, right
// here, before any production code changes, to establish the true baseline
// red/green state for every one of them. Only after that does the fix work
// begin.
// ============================================================================

// --- FINDING 1 ---

// TestSweepLeavesResumableSessionWorktreeAloneOnRestart is case (a): a
// 'worktree' session was created and its log persisted (handleCreate always
// calls sess.Persist() immediately), then the process "restarts" (a fresh
// *Server is built over the same SessionDir) before the session ever ends.
// Its worktree is clean and its log is still on disk — the session is
// resumable — so the startup sweep must leave the worktree completely
// alone.
func TestSweepLeavesResumableSessionWorktreeAloneOnRestart(t *testing.T) {
	repo := newGitRepo(t)
	sessDir := t.TempDir()
	prov := &scriptedProvider{name: "test"}

	srv1 := newServer(t, sessDir, prov, 0, func(o *Options) {
		o.WorkspaceRoots = []string{repo}
	})
	ts1 := httptest.NewServer(srv1)
	t.Cleanup(ts1.Close)
	h := &harness{t: t, dir: sessDir, token: "secret-run-token", srv: srv1, ts: ts1}

	id := h.createSessionBody(map[string]any{
		"model": "test/m1", "workdir": repo, "workdir_isolation": "worktree",
	})
	_, data := h.do("GET", "/session/"+id, nil)
	wd := sessionWorkDir(t, data)
	if _, err := os.Stat(wd); err != nil {
		t.Fatalf("worktree missing right after create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessDir, id+".jsonl")); err != nil {
		t.Fatalf("expected the session log to be persisted immediately on create: %v", err)
	}

	// Simulate the graceful restart: build a fresh *Server against the same
	// SessionDir. New() runs the startup sweep at construction.
	srv2 := newServer(t, sessDir, prov, 0, func(o *Options) {
		o.WorkspaceRoots = []string{repo}
	})
	_ = srv2

	if _, err := os.Stat(wd); err != nil {
		t.Fatalf("expected the resumable session's worktree to survive the restart sweep, stat err=%v", err)
	}
}

// TestSweepTerminalCleanWorktreeStillRemoved is case (b): a session with NO
// log on disk (genuinely terminal) whose worktree is clean must still be
// removed by the startup sweep, exactly as before this change.
func TestSweepTerminalCleanWorktreeStillRemoved(t *testing.T) {
	repo := newGitRepo(t)
	sessDir := t.TempDir()
	base := filepath.Join(sessDir, "worktrees")

	cleanPath := filepath.Join(base, "wt_clean")
	cleanBase, err := addWorktree(repo, cleanPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeWorktreeMeta(base, "wt_clean", worktreeMeta{
		SessionID: "ses_fakeclean0000", RepoRoot: repo, Path: cleanPath, BaseCommit: cleanBase,
	}); err != nil {
		t.Fatal(err)
	}
	// No <SessionDir>/ses_fakeclean0000.jsonl exists: this session is
	// terminal, not resumable.

	sweepWorktrees(base, sessDir, func(sessionID, path string) {
		t.Errorf("unexpected kept event for a clean terminal worktree: %s %s", sessionID, path)
	})

	if _, err := os.Stat(cleanPath); !os.IsNotExist(err) {
		t.Errorf("expected the clean terminal worktree to be removed by the sweep, stat err=%v", err)
	}
}

// TestSweepTerminalDirtyWorktreeKeptOnceThenSilent is case (c): a genuinely
// terminal session (no log on disk) whose worktree is dirty is left on disk
// and journaled via onKept — but only once. The reviewer's sub-issue is
// that the meta was never dropped after the kept journal fired, so an
// identical dirty leftover re-fires onKept on every subsequent sweep
// forever.
func TestSweepTerminalDirtyWorktreeKeptOnceThenSilent(t *testing.T) {
	repo := newGitRepo(t)
	sessDir := t.TempDir()
	base := filepath.Join(sessDir, "worktrees")

	dirtyPath := filepath.Join(base, "wt_dirty")
	dirtyBase, err := addWorktree(repo, dirtyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirtyPath, "uncommitted.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	metaPath, err := writeWorktreeMeta(base, "wt_dirty", worktreeMeta{
		SessionID: "ses_fakedirty0000", RepoRoot: repo, Path: dirtyPath, BaseCommit: dirtyBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	// No <SessionDir>/ses_fakedirty0000.jsonl exists: this session is
	// terminal, not resumable.

	keptCount := 0
	sweepWorktrees(base, sessDir, func(sessionID, path string) {
		if sessionID == "ses_fakedirty0000" && path == dirtyPath {
			keptCount++
		}
	})
	if keptCount != 1 {
		t.Fatalf("expected exactly one kept event on the first sweep, got %d", keptCount)
	}
	if _, err := os.Stat(dirtyPath); err != nil {
		t.Fatalf("expected the dirty worktree to survive, stat err=%v", err)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Errorf("expected the meta file to be dropped once kept was journaled, stat err=%v", err)
	}

	// A second sweep over the same, untouched directory must be silent.
	sweepWorktrees(base, sessDir, func(sessionID, path string) {
		t.Errorf("unexpected second kept event for an already-dropped meta: %s %s", sessionID, path)
	})
	if _, err := os.Stat(dirtyPath); err != nil {
		t.Errorf("expected the dirty worktree to still be on disk after the second, silent sweep: %v", err)
	}
}

// --- FINDING 2 ---

// newEmptyGitRepo creates a git repository with NO commits — 'git init'
// only. gitRepoRoot still succeeds inside it (rev-parse --show-toplevel
// doesn't need a commit), but 'git rev-parse HEAD' (the first thing
// addWorktree does) fails deterministically: an ambiguous HEAD with no
// commits to resolve. That gives createWorktreeForSession a reachable,
// deterministic addWorktree failure with nothing exotic.
func newEmptyGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-q")
	return dir
}

// countMetaFiles returns the number of meta/*.json files under base.
func countMetaFiles(t *testing.T, base string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(base, "meta"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// TestCreateWorktreeForSessionLeavesNoMetaWhenAddWorktreeErrors verifies
// that when addWorktree fails, createWorktreeForSession leaves no meta file
// behind: whatever bookkeeping it wrote in the process (provisional or
// otherwise) must be cleaned up on that error path.
func TestCreateWorktreeForSessionLeavesNoMetaWhenAddWorktreeErrors(t *testing.T) {
	repo := newEmptyGitRepo(t)
	srv := newServer(t, t.TempDir(), &scriptedProvider{name: "test"}, 0)

	wt, err := srv.createWorktreeForSession(repo)
	if err == nil {
		t.Fatalf("expected createWorktreeForSession to fail against a commit-less repo, got wt=%+v", wt)
	}
	if wt != nil {
		t.Errorf("expected a nil worktreeInfo on error, got %+v", wt)
	}

	base, baseErr := srv.worktreeBaseDir()
	if baseErr != nil {
		t.Fatal(baseErr)
	}
	if n := countMetaFiles(t, base); n != 0 {
		t.Errorf("expected no leaked meta files after an addWorktree failure, found %d", n)
	}
}

// TestSweepPrunesProvisionalMetaCrashWindow characterizes the recovery half
// of the finding-2 fix: a meta file exists (as createWorktreeForSession's
// reordered first step will leave it) but the worktree directory was never
// created (addWorktree never ran, or the process died before it
// completed). The sweep must treat this like any other meta whose worktree
// directory is missing: prune it silently.
//
// Written against sweepWorktrees's CURRENT (two-argument, unfixed)
// signature — this is deliberate: this test's job is to characterize the
// recovery mechanism the finding-2 fix depends on, and that mechanism
// (sweepWorktrees' missing-directory branch) already exists today, before
// either fix. Once finding 1 adds the sessionDir parameter, this call site
// is updated along with the others.
func TestSweepPrunesProvisionalMetaCrashWindow(t *testing.T) {
	repo := newGitRepo(t)
	sessDir := t.TempDir()
	base := filepath.Join(sessDir, "worktrees")

	provisionalPath := filepath.Join(base, "wt_crash")
	metaPath, err := writeWorktreeMeta(base, "wt_crash", worktreeMeta{
		RepoRoot: repo, Path: provisionalPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(provisionalPath); !os.IsNotExist(err) {
		t.Fatalf("test setup: expected no worktree directory yet, stat err=%v", err)
	}

	sweepWorktrees(base, sessDir, func(sessionID, path string) {
		t.Errorf("unexpected kept event for a crash-window provisional meta: %s %s", sessionID, path)
	})

	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Errorf("expected the provisional meta to be pruned, stat err=%v", err)
	}
}

// TestSweepOwnerlessMetaWithLiveWorktreeAdjudicated pins the crash artifact
// the createSession ordering (owner recorded BEFORE Persist) can leave
// behind: a meta whose SessionID was never patched in, but whose worktree
// directory fully materialized. With no owner there is nothing that could
// ever resume it — sessionResumable("") must be false — so the sweep
// adjudicates it like any abandoned worktree: clean → removed, meta
// dropped. If sessionResumable ever treated an empty SessionID as
// resumable, this artifact would leak forever (skipped on every sweep).
func TestSweepOwnerlessMetaWithLiveWorktreeAdjudicated(t *testing.T) {
	repo := newGitRepo(t)
	sessDir := t.TempDir()
	base := filepath.Join(sessDir, "worktrees")

	path := filepath.Join(base, "wt_ownerless")
	baseCommit, err := addWorktree(repo, path)
	if err != nil {
		t.Fatal(err)
	}
	metaPath, err := writeWorktreeMeta(base, "wt_ownerless", worktreeMeta{
		RepoRoot: repo, Path: path, BaseCommit: baseCommit, // SessionID never recorded
	})
	if err != nil {
		t.Fatal(err)
	}

	sweepWorktrees(base, sessDir, func(sessionID, path string) {
		t.Errorf("unexpected kept event for an ownerless clean worktree: %s %s", sessionID, path)
	})

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected the ownerless clean worktree to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Errorf("expected the ownerless meta to be dropped, stat err=%v", err)
	}
}
