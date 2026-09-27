package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// gitChangesTimeout is the endpoint's overall request budget: a var, not a
// const, so a test can shrink it to deterministically exercise the 409
// too_many_changes path. gitChangesResponseMargin is reserved out of it for
// marshaling and writing the response, so a too_many_changes answer
// reliably finishes before a caller's own timeout — boxes proxies this
// route with its own client timeout kept a few seconds above
// gitChangesTimeout for exactly this reason.
var gitChangesTimeout = 30 * time.Second

const gitChangesResponseMargin = 2 * time.Second

const gitChangesPatchCap = 1 << 20

// gitChangesMetadataCap bounds each of ls-files/--numstat/--name-status's
// own stdout: unlike the patch, files must stay complete, so a change set
// this large answers 409 too_many_changes instead of truncating it. A var,
// not a const, so a test can shrink it to reach that path deterministically.
var gitChangesMetadataCap = 32 << 20

// gitFilterDiscoveryCap bounds filter-driver discovery far below
// gitChangesMetadataCap: each discovered driver becomes three
// GIT_CONFIG_KEY/VALUE override pairs in every diff's environment (up to
// ~7x its discovery bytes), and the environment shares ARG_MAX (2 MiB on
// Linux) with argv. 64 KiB keeps that under ~450 KiB, while a real
// repository configures a handful of drivers.
const gitFilterDiscoveryCap = 64 << 10

// untrackedLargeCutoff mirrors opencode's snapshot (sst/opencode
// packages/opencode/src/snapshot/index.ts): an untracked file this large
// contributes no hunk, so a request never buffers or diffs a multi-
// megabyte blob for a file the agent hasn't even tracked.
const untrackedLargeCutoff = 2 * 1024 * 1024

// gitChangesWaitDelay bounds exec.Cmd.Wait, so a lingering grandchild
// process holding stdout/stderr open can't block it indefinitely.
const gitChangesWaitDelay = 2 * time.Second

type gitChangeFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary"`
	Large     bool   `json:"large"`
}

type gitBaseRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type gitChangesJSON struct {
	Dir       string          `json:"dir"`
	Scope     string          `json:"scope"`
	Branch    string          `json:"branch"`
	Head      string          `json:"head"`
	Base      *gitBaseRef     `json:"base,omitempty"`
	Files     []gitChangeFile `json:"files"`
	Patch     string          `json:"patch"`
	Truncated bool            `json:"truncated"`
}

func (s *Server) handleGitChanges(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "branch"
	}
	if scope != "branch" && scope != "uncommitted" {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("scope %q must be \"branch\" or \"uncommitted\"", scope))
		return
	}
	rawDir := r.URL.Query().Get("dir")
	dir, err := resolveWorkDir(s.opts.WorkspaceRoots, rawDir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// An omitted dir is the process's own cwd, which POST /session also
	// never checks against WorkspaceRoots: the operator chose it. Only an
	// explicit dir is confined to the roots (and to their ceiling).
	var realDir, ceiling string
	if rawDir == "" {
		realDir, err = filepath.EvalSymlinks(dir)
	} else {
		realDir, ceiling, err = verifyDirWithinRoots(s.opts.WorkspaceRoots, dir)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), gitChangesTimeout-gitChangesResponseMargin)
	defer cancel()

	repoRoot, ok, err := gitRepoRootAt(ctx, realDir, ceiling)
	if err != nil {
		writeGitErr(w, ctx, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusConflict, fmt.Sprintf("not_a_git_repo: %q is not a git work tree", dir))
		return
	}
	// GIT_CEILING_DIRECTORIES is itself colon-separated with no escape for a
	// colon in a path, so a workspace root containing one could let git walk
	// past the intended boundary into a parent repository. Re-checking
	// repoRoot the same way dir itself was checked catches that regardless.
	if rawDir != "" {
		if _, _, err := verifyDirWithinRoots(s.opts.WorkspaceRoots, repoRoot); err != nil {
			writeErr(w, http.StatusConflict, fmt.Sprintf("not_a_git_repo: %q is not a git work tree", dir))
			return
		}
	}

	head := "" // empty means an unborn branch (no commit yet)
	if out, err := gitOut(ctx, repoRoot, nil, "rev-parse", "HEAD"); err == nil {
		head = strings.TrimSpace(out)
	} else if ctx.Err() != nil {
		writeGitErr(w, ctx, err)
		return
	}

	branch := ""
	if b, err := gitOut(ctx, repoRoot, nil, "symbolic-ref", "-q", "--short", "HEAD"); err == nil {
		branch = strings.TrimSpace(b)
	}

	resp := gitChangesJSON{Dir: dir, Scope: scope, Branch: branch, Head: head}
	baseTreeish := "HEAD"
	switch {
	case scope == "branch":
		if head == "" {
			writeErr(w, http.StatusConflict, "no_base: HEAD has no commit yet")
			return
		}
		display, revision, found := defaultBranchRef(ctx, repoRoot)
		if !found {
			if ctx.Err() != nil {
				writeGitErr(w, ctx, ctx.Err())
				return
			}
			writeErr(w, http.StatusConflict, "no_base: no default branch found (checked origin/HEAD, origin/main, origin/master)")
			return
		}
		mb, err := gitOut(ctx, repoRoot, nil, "merge-base", head, revision)
		if err != nil {
			var exitErr *exec.ExitError
			if ctx.Err() == nil && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				writeErr(w, http.StatusConflict, fmt.Sprintf("no_base: HEAD and %s share no common ancestor", display))
				return
			}
			writeGitErr(w, ctx, err)
			return
		}
		sha := strings.TrimSpace(mb)
		resp.Base = &gitBaseRef{Ref: display, SHA: sha}
		baseTreeish = sha
	case head == "":
		// scope == "uncommitted": no HEAD to diff against, so fall back to
		// the repository's own empty tree. branch scope never reaches here
		// (it always returns above), so this never runs a wasted subprocess
		// for a request that's about to 409 anyway.
		emptyTree, err := gitEmptyTree(ctx, repoRoot)
		if err != nil {
			writeGitErr(w, ctx, err)
			return
		}
		baseTreeish = emptyTree
	}

	files, patch, truncated, err := gitChangeSet(ctx, repoRoot, head, baseTreeish, gitChangesPatchCap)
	if err != nil {
		writeGitErr(w, ctx, err)
		return
	}
	resp.Files = files
	resp.Patch = patch
	resp.Truncated = truncated
	writeJSONNoEscapeHTML(w, http.StatusOK, resp)
}

// writeGitErr answers 409 too_many_changes when ctx's own deadline caused
// err (a killed git subprocess) — a bounded, expected scale ceiling, not a
// server fault — rather than a generic 500.
func writeGitErr(w http.ResponseWriter, ctx context.Context, err error) {
	if ctx.Err() != nil || errors.Is(err, errMetadataTooLarge) {
		writeErr(w, http.StatusConflict, "too_many_changes: request exceeded its time budget diffing a large change set")
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

// verifyDirWithinRoots re-checks dir after symlink resolution, so a symlink
// under an allowed root can't point outside all of them, and rejects a
// missing dir before any git subprocess runs. ceiling is the matched root's
// parent, for GIT_CEILING_DIRECTORIES.
func verifyDirWithinRoots(roots []string, dir string) (real, ceiling string, err error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", "", fmt.Errorf("dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("dir %q is not a directory", dir)
	}
	real, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", err
	}
	effective := roots
	if len(effective) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return "", "", err
		}
		effective = []string{cwd}
	}
	for _, r := range effective {
		rAbs, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		rReal, err := filepath.EvalSymlinks(rAbs)
		if err != nil {
			rReal = filepath.Clean(rAbs)
		}
		if real == rReal || strings.HasPrefix(real, rReal+string(os.PathSeparator)) {
			return real, filepath.Dir(rReal), nil
		}
	}
	return "", "", fmt.Errorf("dir %q escapes every allowed workspace root", dir)
}

// gitCmdHook, non-nil only in tests, is called with every subprocess's argv.
var gitCmdHook func(args []string)

// gitStaticSafetyArgs disable hook-based fsmonitor, require explicit opt-in
// before treating a directory as bare, and point core.hooksPath at a
// directory with no hook scripts: `add -N` (like `add` and `commit`) runs
// the repository's own post-index-change hook otherwise, so a
// repo-controlled hooksPath would run arbitrary code as this process.
var gitStaticSafetyArgs = []string{
	"-c", "core.fsmonitor=false",
	"-c", "safe.bareRepository=explicit",
	"-c", "core.hooksPath=" + os.DevNull,
}

// gitStrippedEnv is never inherited from harness's own environment. The
// first group is `git rev-parse --local-env-vars`: variables that select a
// repository, index, object store, or config, so that (a git hook exports
// GIT_DIR and GIT_INDEX_FILE) they would override cmd.Dir and read another
// repository. The pathspec-mode group would override each command's own
// pathspec parsing, e.g. make add -N's ":(exclude,literal)" a literal path.
var gitStrippedEnv = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_CONFIG": true,
	"GIT_CONFIG_PARAMETERS": true, "GIT_CONFIG_COUNT": true,
	"GIT_OBJECT_DIRECTORY": true, "GIT_DIR": true, "GIT_WORK_TREE": true,
	"GIT_IMPLICIT_WORK_TREE": true, "GIT_GRAFT_FILE": true,
	"GIT_INDEX_FILE": true, "GIT_NO_REPLACE_OBJECTS": true,
	"GIT_REPLACE_REF_BASE": true, "GIT_PREFIX": true,
	"GIT_INTERNAL_SUPER_PREFIX": true, "GIT_SHALLOW_FILE": true,
	"GIT_COMMON_DIR": true,

	"GIT_LITERAL_PATHSPECS": true, "GIT_GLOB_PATHSPECS": true,
	"GIT_NOGLOB_PATHSPECS": true, "GIT_ICASE_PATHSPECS": true,
}

// gitBaseEnv is os.Environ() without gitStrippedEnv; callers add back only
// the values this endpoint sets itself.
func gitBaseEnv() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !gitStrippedEnv[name] {
			out = append(out, kv)
		}
	}
	return out
}

// gitCmd builds a git subprocess bounded by ctx. GIT_LITERAL_PATHSPECS=1
// keeps a pathspec built from a real filename (e.g. "b*.txt") from being
// reinterpreted as a glob.
func gitCmd(ctx context.Context, dir string, extraEnv []string, args ...string) *exec.Cmd {
	args = append(append([]string{}, gitStaticSafetyArgs...), args...)
	if gitCmdHook != nil {
		gitCmdHook(args)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(gitBaseEnv(),
		"GIT_OPTIONAL_LOCKS=0", "GIT_LITERAL_PATHSPECS=1", "GIT_NO_LAZY_FETCH=1"), extraEnv...)
	cmd.WaitDelay = gitChangesWaitDelay
	return cmd
}

// gitCmdMagicPathspecs is gitCmd without GIT_LITERAL_PATHSPECS, for the one
// `add -N` call that relies on `:(exclude,literal)` pathspec magic (its own
// "literal" keeps a glob-like path exact instead).
func gitCmdMagicPathspecs(ctx context.Context, dir string, extraEnv []string, args ...string) *exec.Cmd {
	args = append(append([]string{}, gitStaticSafetyArgs...), args...)
	if gitCmdHook != nil {
		gitCmdHook(args)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(gitBaseEnv(), "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1"), extraEnv...)
	cmd.WaitDelay = gitChangesWaitDelay
	return cmd
}

func gitOut(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	cmd := gitCmd(ctx, dir, extraEnv, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// errMetadataTooLarge marks a gitOutCapped overflow, mapped by writeGitErr to
// 409 too_many_changes regardless of ctx's own remaining budget: unlike the
// patch, metadata output must stay complete, so exceeding cap is a scale
// ceiling to report, not something to truncate.
var errMetadataTooLarge = errors.New("git metadata exceeded its size bound")

// gitOutCapped is gitOut bounded to at most cap bytes of stdout, so a
// change set with unbounded file-list or numstat/name-status output can't
// allocate unbounded memory before ctx's own deadline has a chance to kill
// the subprocess.
func gitOutCapped(ctx context.Context, dir string, extraEnv []string, maxBytes int, args ...string) (string, error) {
	cmd := gitCmd(ctx, dir, extraEnv, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxBytes)+1))
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), readErr)
	}
	if len(data) > maxBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", errMetadataTooLarge
	}
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(data), nil
}

// isGitWorkTreeErr distinguishes a plain non-zero git exit from a start/exec
// failure, so a bad answer turns into 409 rather than 500.
func isGitWorkTreeErr(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// gitRepoRootAt resolves dir's work tree root, bounded by ceiling
// (GIT_CEILING_DIRECTORIES). ok is false, err nil, when dir is not inside
// any git work tree up to that boundary.
func gitRepoRootAt(ctx context.Context, dir, ceiling string) (root string, ok bool, err error) {
	var env []string
	if ceiling != "" {
		env = []string{"GIT_CEILING_DIRECTORIES=" + ceiling}
	}
	out, err := gitOut(ctx, dir, env, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctx.Err() == nil && isGitWorkTreeErr(err) {
			return "", false, nil
		}
		return "", false, err
	}
	// TrimSpace would also strip a trailing space that's part of the
	// directory's own real name; --show-toplevel's output ends in exactly
	// one record-terminating newline.
	return strings.TrimSuffix(out, "\n"), true, nil
}

// gitEmptyTree returns dir's empty tree object (SHA-1 or SHA-256, whichever
// it uses), standing in for "no commit yet".
func gitEmptyTree(ctx context.Context, dir string) (string, error) {
	out, err := gitOut(ctx, dir, nil, "hash-object", "-t", "tree", os.DevNull)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// defaultBranchRef checks, in order, origin/HEAD's symbolic target, then
// origin/main, then origin/master, each verified to actually resolve so a
// stale symref falls through instead of 500ing. display is human-facing;
// revision is what merge-base is called with.
func defaultBranchRef(ctx context.Context, dir string) (display, revision string, found bool) {
	if out, err := gitOut(ctx, dir, nil, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil {
		full := strings.TrimSpace(out)
		if _, verr := gitOut(ctx, dir, nil, "rev-parse", "--verify", "-q", full); verr == nil {
			return strings.TrimPrefix(full, "refs/remotes/"), full, true
		}
	}
	for _, name := range []string{"main", "master"} {
		ref := "refs/remotes/origin/" + name
		if _, err := gitOut(ctx, dir, nil, "rev-parse", "--verify", "-q", ref); err == nil {
			return "origin/" + name, ref, true
		}
	}
	return "", "", false
}

type diffNumstat struct {
	additions int
	deletions int
	binary    bool
}

func splitNulZ(s string) []string {
	s = strings.TrimSuffix(s, "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

// parseNumstatZ parses `git diff --numstat -z`, keyed by each file's new
// path. A rename or copy prints an empty path field, then old and new.
func parseNumstatZ(out string) map[string]diffNumstat {
	fields := splitNulZ(out)
	result := make(map[string]diffNumstat, len(fields))
	i := 0
	for i < len(fields) {
		head := fields[i]
		i++
		parts := strings.SplitN(head, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		addStr, delStr, path := parts[0], parts[1], parts[2]
		if path == "" {
			if i+1 >= len(fields) {
				break
			}
			i++ // old path: unused here, name-status carries it
			path = fields[i]
			i++
		}
		binary := addStr == "-" || delStr == "-"
		add, _ := strconv.Atoi(addStr)
		del, _ := strconv.Atoi(delStr)
		result[path] = diffNumstat{additions: add, deletions: del, binary: binary}
	}
	return result
}

type nameStatusEntry struct {
	status  string // first letter only: A, M, D, R, ...
	oldPath string // set only for a rename (or copy)
	newPath string
}

func parseNameStatusZ(out string) []nameStatusEntry {
	fields := splitNulZ(out)
	var entries []nameStatusEntry
	i := 0
	for i < len(fields) {
		status := fields[i]
		i++
		if i >= len(fields) || status == "" {
			break
		}
		if status[0] == 'R' || status[0] == 'C' {
			if i+1 >= len(fields) {
				break
			}
			old := fields[i]
			i++
			newPath := fields[i]
			i++
			entries = append(entries, nameStatusEntry{status: status[:1], oldPath: old, newPath: newPath})
			continue
		}
		path := fields[i]
		i++
		entries = append(entries, nameStatusEntry{status: status[:1], newPath: path})
	}
	return entries
}

// statusWord maps a name-status letter to the contract's status word; an
// undocumented letter (C, T) falls back to "modified".
func statusWord(letter string) string {
	switch letter {
	case "A":
		return "added"
	case "D":
		return "deleted"
	case "R":
		return "renamed"
	default:
		return "modified"
	}
}

// untrackedEntries partitions `ls-files -o --exclude-standard -z`'s raw
// output (no --directory: a "/"-suffixed entry is exactly a nested repo)
// into `add -N` exclude pathspecs and the candidate large files (over
// untrackedLargeCutoff). A nested repo, and every candidate large file, are
// always excluded from intent-to-add — this package never reads a large
// file's content on the strength of its size alone.
func untrackedEntries(repoRoot, lsFilesOut string) (excludeArgs, bigPaths []string) {
	for _, p := range splitNulZ(lsFilesOut) {
		if strings.HasSuffix(p, "/") {
			excludeArgs = append(excludeArgs, ":(exclude,literal)"+p)
			continue
		}
		info, err := os.Lstat(filepath.Join(repoRoot, p))
		if err != nil {
			continue // vanished since ls-files ran
		}
		if info.Size() > untrackedLargeCutoff {
			bigPaths = append(bigPaths, p)
			excludeArgs = append(excludeArgs, ":(exclude,literal)"+p)
		}
	}
	return excludeArgs, bigPaths
}

// addUntrackedIntentToAdd stages every untracked path (other than one named
// in excludeArgs) intent-to-add in one subprocess, regardless of file
// count: pathspec "." lets git itself decide .gitignore and repository
// boundaries. core.splitIndex=false keeps it from writing a shared-index
// file into the real repository. Pathspecs go over stdin
// (--pathspec-from-file), not argv, since tens of thousands of excludes
// would otherwise risk the OS argument-size limit.
func addUntrackedIntentToAdd(ctx context.Context, repoRoot string, env []string, excludeArgs []string) error {
	var stdin bytes.Buffer
	stdin.WriteString(".\x00")
	for _, a := range excludeArgs {
		stdin.WriteString(a)
		stdin.WriteByte(0)
	}
	cmd := gitCmdMagicPathspecs(ctx, repoRoot, env,
		"-c", "core.splitIndex=false", "add", "-N", "--pathspec-from-file=-", "--pathspec-file-nul", "--")
	cmd.Stdin = &stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git add -N: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// diffFilterDriverEnv neutralizes every repo-configured clean/process filter
// driver, discovered once per request, not per file. It returns
// GIT_CONFIG_COUNT/KEY/VALUE pairs, not `-c` arguments: git splits `-c k=v`
// at the first "=", so a driver named "x=y" would never be overridden.
func diffFilterDriverEnv(ctx context.Context, dir string) ([]string, error) {
	out, err := gitOutCapped(ctx, dir, nil, gitFilterDiscoveryCap, "config", "--null", "--name-only", "--get-regexp", `^filter\..*\.(clean|process)$`)
	if err != nil {
		if isGitWorkTreeErr(err) {
			return nil, nil // no configured filter driver matches
		}
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, key := range splitNulZ(out) {
		rest, ok := strings.CutPrefix(key, "filter.")
		last := strings.LastIndex(rest, ".")
		if !ok || last < 0 {
			continue
		}
		name := rest[:last] // may itself contain dots, e.g. "a.b"
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var env []string
	n := 0
	set := func(key, value string) {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n, key), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, value))
		n++
	}
	for _, name := range names {
		set("filter."+name+".clean", "")
		set("filter."+name+".process", "")
		set("filter."+name+".required", "false")
	}
	if n == 0 {
		return nil, nil
	}
	return append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(n)), nil
}

// gitRealPath resolves gitPath (e.g. "index", "objects") via `git rev-parse
// --git-path`, absolutized against repoRoot — this also works inside a git
// worktree, whose index and objects live under the main repository's
// .git/worktrees/<name>/, not a plain .git/.
func gitRealPath(ctx context.Context, repoRoot, gitPath string) (string, error) {
	out, err := gitOut(ctx, repoRoot, nil, "rev-parse", "--git-path", gitPath)
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(out)
	if !filepath.IsAbs(p) {
		p = filepath.Join(repoRoot, p)
	}
	return p, nil
}

// gitQuotePathListEntry quotes path for inclusion in a colon-separated git
// path list (GIT_ALTERNATE_OBJECT_DIRECTORIES): a bare colon in path would
// otherwise split it into more than one entry. Unlike
// GIT_CEILING_DIRECTORIES, git's alternates parsing honors a double-quoted,
// backslash-escaped entry the same way objects/info/alternates does.
func gitQuotePathListEntry(path string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path)
	return `"` + escaped + `"`
}

// gitChangeSet computes files and patch against baseTreeish, folding
// untracked files in as "added" via a private index copy, in a constant
// number of subprocesses regardless of file count. files is always
// complete; patch stops at the last whole file within patchCap.
func gitChangeSet(ctx context.Context, repoRoot, head, baseTreeish string, patchCap int) (files []gitChangeFile, patch string, truncated bool, err error) {
	tmpDir, err := os.MkdirTemp("", "harness-git-changes-")
	if err != nil {
		return nil, "", false, err
	}
	defer os.RemoveAll(tmpDir)
	tmpIndex := filepath.Join(tmpDir, "index")
	tmpObjects := filepath.Join(tmpDir, "objects")
	if err := os.MkdirAll(tmpObjects, 0o700); err != nil {
		return nil, "", false, err
	}

	realIndex, err := gitRealPath(ctx, repoRoot, "index")
	if err != nil {
		return nil, "", false, err
	}
	indexMissing := false
	if data, err := os.ReadFile(realIndex); err == nil {
		if err := os.WriteFile(tmpIndex, data, 0o600); err != nil {
			return nil, "", false, err
		}
	} else if os.IsNotExist(err) {
		indexMissing = true
	} else {
		return nil, "", false, err
	}

	realObjects, err := gitRealPath(ctx, repoRoot, "objects")
	if err != nil {
		return nil, "", false, err
	}
	// GIT_OBJECT_DIRECTORY isolates every object add -N or diff would write
	// (e.g. the empty blob an intent-to-add entry needs) into tmpObjects;
	// GIT_ALTERNATE_OBJECT_DIRECTORIES still resolves a read against every
	// real object.
	env := []string{
		"GIT_INDEX_FILE=" + tmpIndex,
		"GIT_OBJECT_DIRECTORY=" + tmpObjects,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + gitQuotePathListEntry(realObjects),
	}
	filterEnv, err := diffFilterDriverEnv(ctx, repoRoot)
	if err != nil {
		return nil, "", false, err
	}
	// diffEnv is env plus the filter overrides, for every command that can
	// hash or diff working-tree content.
	diffEnv := append(append([]string{}, env...), filterEnv...)

	// A missing real index leaves tmpIndex unwritten, which git reads as a
	// fresh empty index: right for an unborn repository. With a commit, an
	// empty index would make every tracked file look deleted or untracked,
	// so seed it from HEAD instead, as `git reset --mixed` would. read-tree
	// writes no stat data and the diffs never refresh it, so refresh once
	// here (filters neutralized, since refresh hashes working-tree files).
	if indexMissing && head != "" {
		noSplit := []string{"-c", "core.splitIndex=false"}
		if _, err := gitOut(ctx, repoRoot, env, append(noSplit, "read-tree", "HEAD")...); err != nil {
			return nil, "", false, err
		}
		if _, err := gitOut(ctx, repoRoot, diffEnv, append(noSplit, "update-index", "-q", "--refresh")...); err != nil {
			return nil, "", false, err
		}
	}

	// ls-files reads the private index too, so it agrees with the diffs on
	// what is tracked.
	lsFilesOut, err := gitOutCapped(ctx, repoRoot, env, gitChangesMetadataCap, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, "", false, err
	}
	excludeArgs, bigPaths := untrackedEntries(repoRoot, lsFilesOut)
	if err := addUntrackedIntentToAdd(ctx, repoRoot, env, excludeArgs); err != nil {
		return nil, "", false, err
	}

	// Global -c overrides must precede the "diff" subcommand; every other
	// flag is a diff option and must follow it.
	diffArgs := func(rest ...string) []string {
		args := []string{"-c", "diff.autoRefreshIndex=false"}
		args = append(args, "diff", "--no-ext-diff", "--no-textconv", "--submodule=short", "--ignore-submodules=dirty")
		return append(args, rest...)
	}

	numstatOut, err := gitOutCapped(ctx, repoRoot, diffEnv, gitChangesMetadataCap, diffArgs("--numstat", "-z", "-M", baseTreeish)...)
	if err != nil {
		return nil, "", false, err
	}
	nameStatusOut, err := gitOutCapped(ctx, repoRoot, diffEnv, gitChangesMetadataCap, diffArgs("--name-status", "-z", "-M", baseTreeish)...)
	if err != nil {
		return nil, "", false, err
	}
	numstat := parseNumstatZ(numstatOut)

	files = []gitChangeFile{}
	seenPath := make(map[string]bool, len(bigPaths))
	for _, e := range parseNameStatusZ(nameStatusOut) {
		ns := numstat[e.newPath]
		files = append(files, gitChangeFile{
			Path:      e.newPath,
			OldPath:   e.oldPath,
			Status:    statusWord(e.status),
			Additions: ns.additions,
			Deletions: ns.deletions,
			Binary:    ns.binary,
		})
		seenPath[e.newPath] = true
		if e.oldPath != "" {
			seenPath[e.oldPath] = true
		}
	}
	// A big path already excluded from staging shows up here on its own,
	// under its real (typically "deleted") status, whenever baseTreeish
	// already has it under the same path (e.g. a large tracked file `git rm
	// --cached`'d) — reported below too, it would duplicate that entry.
	for _, p := range bigPaths {
		// Recheck: a large file removed after ls-files is silently absent,
		// the same as an ordinary one add -N no longer finds.
		if _, err := os.Lstat(filepath.Join(repoRoot, p)); err != nil {
			continue
		}
		if !seenPath[p] {
			files = append(files, gitChangeFile{Path: p, Status: "added", Large: true})
		}
	}

	patch, truncated, err = runPatchCapped(ctx, repoRoot, diffEnv,
		diffArgs("--no-color", "-M", baseTreeish), patchCap)
	if err != nil {
		return nil, "", false, err
	}
	return files, patch, truncated, nil
}

// diffGitMarker starts every file section of a `git diff` patch, always
// preceded by a newline except at offset 0.
var diffGitMarker = []byte("\ndiff --git ")

// runPatchCapped reads at most patchCap+len(diffGitMarker) bytes, so memory
// never scales with the diff's real size (the overread lets a file ending
// exactly at patchCap still be recognized). Reading that many bytes means
// more output remains, so this kills the subprocess rather than draining
// it, and cuts back to the last whole-file boundary within patchCap.
func runPatchCapped(ctx context.Context, dir string, extraEnv, args []string, patchCap int) (patch string, truncated bool, err error) {
	cmd := gitCmd(ctx, dir, extraEnv, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", false, err
	}

	data, readErr := io.ReadAll(io.LimitReader(stdout, int64(patchCap+len(diffGitMarker))))
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", false, fmt.Errorf("git %s: %w", strings.Join(args, " "), readErr)
	}

	if len(data) > patchCap {
		cut := lastWholeFileBoundary(data, patchCap)
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // reap; Wait's own error is expected (killed) and not reported
		return string(data[:cut]), true, nil
	}
	if err := cmd.Wait(); err != nil {
		return "", false, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(data), false, nil
}

// lastWholeFileBoundary returns the longest prefix of data, no longer than
// limit, ending exactly at a diffGitMarker — never mid-hunk.
func lastWholeFileBoundary(data []byte, limit int) int {
	best := 0
	for i := 0; ; {
		idx := bytes.Index(data[i:], diffGitMarker)
		if idx < 0 {
			break
		}
		idx += i
		if cut := idx + 1; cut <= limit {
			best = cut
			i = idx + 1
		} else {
			break
		}
	}
	return best
}

// writeJSONNoEscapeHTML is writeJSON without HTML escaping, so a patch full
// of '<', '>', and '&' (HTML or JSX) doesn't inflate past its byte cap.
func writeJSONNoEscapeHTML(w http.ResponseWriter, code int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal: " + err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}
