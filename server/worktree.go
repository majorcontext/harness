package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/majorcontext/harness/typeid"
)

// newWorktreeID mints a fresh, time-sortable directory/meta-file name for a
// server-owned worktree — independent of the eventual session ID, which
// isn't minted until after the worktree already exists (see
// createWorktreeForSession).
func newWorktreeID() string {
	id, err := typeid.New("wt")
	if err != nil {
		panic(err) // crypto/rand failure is unrecoverable, mirrors engine.newID
	}
	return id.String()
}

// gitOpTimeout bounds every git subprocess this file spawns. Worktree
// creation/removal and the plumbing queries below are all local,
// network-free operations against an already-cloned repository, so a few
// seconds is generous; this exists only to keep a wedged git process from
// hanging a session-create/end request forever.
const gitOpTimeout = 30 * time.Second

// worktreeInfo is the in-memory record of a session's dedicated git
// worktree, held on its sessionState for the lifetime of this process (like
// shareWorkdir, it does not survive a reload from disk — see sessionState).
// path is also the session's WorkDir(): tools run there, not in repoRoot.
type worktreeInfo struct {
	id         string // worktreeBase-relative directory/meta-file name
	base       string // worktreeBase this worktree was created under
	path       string // the worktree's own checkout directory
	repoRoot   string // the main repository it was added from
	baseCommit string // the detached-HEAD commit it started at
	metaPath   string // <worktreeBase>/meta/<id>.json, for teardown/sweep
}

// worktreeMeta is worktreeInfo's on-disk form, written the moment a worktree
// is created (before the session is even usable) and removed once the
// worktree itself is torn down. It is the sole durable record of a
// server-owned worktree, which is what lets sweepWorktrees recover from a
// crash between creation and teardown without trusting git plumbing to
// reverse-engineer provenance it was never asked to track.
type worktreeMeta struct {
	SessionID  string `json:"session_id"`
	RepoRoot   string `json:"repo_root"`
	Path       string `json:"path"`
	BaseCommit string `json:"base_commit"`
}

// runGit runs git with dir as its working directory and a bounded timeout,
// returning combined stdout+stderr on failure so callers can surface a
// useful error without a second round trip.
func runGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitOpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// gitRepoRoot resolves dir's git repository root. ok is false, with a nil
// error, exactly when dir is not inside any git working tree at all — the
// distinction callers need to turn "not a repo" into a clean 400 rather than
// a 500 for e.g. a missing git binary or an unreadable directory.
func gitRepoRoot(dir string) (root string, ok bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitOpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, runErr := cmd.Output()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("git rev-parse --show-toplevel: %w", runErr)
	}
	return strings.TrimSpace(string(out)), true, nil
}

// addWorktree creates a new git worktree at path, detached at repoRoot's
// current HEAD (never on a branch: two 'worktree' sessions on the same repo
// must never conflict, and git refuses to check the same branch out twice —
// see the package doc on workdirHolderLocked's isolation bypass). It returns
// the commit the worktree started at, which worktreeClean uses to tell
// "nothing happened here" apart from "new, unpushed commits happened here".
func addWorktree(repoRoot, path string) (baseCommit string, err error) {
	head, err := runGit(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	head = strings.TrimSpace(head)
	if _, err := runGit(repoRoot, "worktree", "add", "--detach", path, head); err != nil {
		return "", fmt.Errorf("git worktree add: %w", err)
	}
	return head, nil
}

// removeWorktree removes a git worktree without --force: git itself refuses
// when the worktree has modified or untracked files, which is the same
// safety net worktreeClean already checked — this call is expected to
// succeed whenever the caller has already confirmed clean, and its failure
// (e.g. a TOCTOU write landing in between) must fall back to "kept", never
// to a forced deletion.
func removeWorktree(repoRoot, path string) error {
	_, err := runGit(repoRoot, "worktree", "remove", path)
	return err
}

// worktreeClean reports whether a worktree has no uncommitted changes and no
// unpushed commits, i.e. whether removing it destroys nothing:
//
//   - `git status --porcelain` must be empty (no staged, unstaged, or
//     untracked files).
//   - If HEAD hasn't moved from baseCommit, nothing was ever committed here
//     — clean regardless of remotes.
//   - If HEAD has moved, the new commit(s) must already be reachable from
//     some remote-tracking branch (i.e. pushed) — otherwise they are
//     unpushed work and the worktree is dirty.
//
// Any git failure is surfaced as an error with clean=false: an isolation
// mode whose whole purpose is "never destroy work" must fail closed on an
// ambiguous read, not guess clean.
func worktreeClean(path, baseCommit string) (bool, error) {
	status, err := runGit(path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(status) != "" {
		return false, nil
	}
	head, err := runGit(path, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	head = strings.TrimSpace(head)
	if head == strings.TrimSpace(baseCommit) {
		return true, nil
	}
	remoteContains, err := runGit(path, "branch", "-r", "--contains", head)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(remoteContains) != "", nil
}

// writeWorktreeMeta persists m as <base>/meta/<id>.json, creating the meta
// directory if needed. createWorktreeForSession calls it three times, each
// patching in more of worktreeMeta's fields as they become known: first
// (RepoRoot, Path only) before addWorktree ever runs, again with BaseCommit
// once addWorktree succeeds, and finally with SessionID once the session is
// minted (recordWorktreeOwner). Writing the provisional meta before
// addWorktree means a crash at any point from then on leaves enough on disk
// for sweepWorktrees to find and adjudicate the worktree on the next serve
// start — including the pathological case where addWorktree itself never
// got to run at all.
func writeWorktreeMeta(base, id string, m worktreeMeta) (metaPath string, err error) {
	dir := filepath.Join(base, "meta")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	metaPath = filepath.Join(dir, id+".json")
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		return "", err
	}
	return metaPath, nil
}

func readWorktreeMeta(path string) (worktreeMeta, error) {
	var m worktreeMeta
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}

// sessionResumable reports whether sessionID's session log
// (<sessionDir>/<sessionID>.jsonl) still exists — i.e. whether the session
// survived a restart and may still be resumed via LoadSession, which
// restores its WorkDir (the worktree path) verbatim and expects tools to
// keep running there. A meta with no session ID yet (the provisional meta
// written before a worktree's owning session exists — see
// createWorktreeForSession) is never resumable: nothing could possibly
// resume it.
func sessionResumable(sessionDir, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(sessionDir, sessionID+".jsonl"))
	return err == nil
}

// sweepWorktrees adjudicates every worktree this process (or a predecessor
// that crashed) ever recorded under base's meta directory: a missing
// worktree directory just needs its git admin metadata and meta file
// dropped (git worktree prune handles the former); an existing, clean one is
// removed the same way normal per-session teardown would; a dirty (or
// unreadable-so-assumed-dirty) one is left exactly where it is, and onKept
// — when non-nil — is invoked with its session ID and path so the caller can
// journal the same "kept" record a graceful teardown would have written,
// ensuring a crash never silently drops that signal.
//
// A meta whose owning session is still resumable (its session log —
// <sessionDir>/<session_id>.jsonl — still exists) is left completely alone:
// no removal, no clean/dirty judgment at all, no kept journal. A graceful
// restart leaves a resumable session's worktree and log both intact; only a
// genuinely terminal session (no log at all) is ever reaped or judged here.
// Once a terminal session's dirty worktree has been journaled as kept, its
// meta is dropped — the journal record is the durable trace of that
// decision, not the meta file, so a later sweep of the same still-dirty
// worktree (nothing else has touched it) doesn't re-journal it forever.
//
// A missing or empty meta directory is a no-op, so calling this
// unconditionally at serve start is always safe and cheap.
func sweepWorktrees(base, sessionDir string, onKept func(sessionID, path string)) {
	metaDir := filepath.Join(base, "meta")
	entries, err := os.ReadDir(metaDir)
	if err != nil {
		return
	}
	prunedRepos := make(map[string]bool)
	pruneOnce := func(repoRoot string) {
		if repoRoot == "" || prunedRepos[repoRoot] {
			return
		}
		prunedRepos[repoRoot] = true
		runGit(repoRoot, "worktree", "prune")
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		metaPath := filepath.Join(metaDir, e.Name())
		m, err := readWorktreeMeta(metaPath)
		if err != nil {
			// Unreadable meta: nothing safe to decide. Leave it for a human.
			continue
		}
		if sessionResumable(sessionDir, m.SessionID) {
			// The owning session may still be resumed; its next prompt
			// expects this exact worktree to exist. Touch nothing.
			continue
		}
		info, statErr := os.Stat(m.Path)
		if statErr != nil || !info.IsDir() {
			// The worktree directory is already gone (removed out from
			// under us, or never fully created); just reconcile the
			// repo's admin metadata and drop our own bookkeeping.
			pruneOnce(m.RepoRoot)
			os.Remove(metaPath)
			continue
		}
		clean, cleanErr := worktreeClean(m.Path, m.BaseCommit)
		if cleanErr == nil && clean {
			if rmErr := removeWorktree(m.RepoRoot, m.Path); rmErr == nil {
				pruneOnce(m.RepoRoot)
				os.Remove(metaPath)
				continue
			}
		}
		if onKept != nil {
			onKept(m.SessionID, m.Path)
		}
		// The kept record above is now the durable trace of this decision;
		// drop the meta so a later sweep of this same terminal, still-dirty
		// worktree doesn't re-journal it on every future restart. The
		// worktree itself stays exactly where it is.
		os.Remove(metaPath)
	}
}
