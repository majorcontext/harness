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

const gitChangesTimeout = 30 * time.Second

const gitChangesPatchCap = 1 << 20

// gitChangesWaitDelay bounds exec.Cmd.Wait after the context (or an explicit
// Kill) ends a git subprocess: without it, Wait blocks on stdout/stderr pipes
// until every descriptor closes, which a lingering grandchild (a hook, an
// fsmonitor daemon) can hold open indefinitely.
const gitChangesWaitDelay = 2 * time.Second

// gitEmptyTreeSHA1 is git's well-known empty-tree object, stable across
// every repository using the SHA-1 object format. Diffing against it stands
// in for "no commit yet" (an unborn HEAD), so scope=uncommitted still
// answers instead of 500ing.
const gitEmptyTreeSHA1 = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

type gitChangeFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary"`
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
	dir, err := resolveWorkDir(s.opts.WorkspaceRoots, r.URL.Query().Get("dir"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	realDir, ceiling, err := verifyDirWithinRoots(s.opts.WorkspaceRoots, dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), gitChangesTimeout)
	defer cancel()

	repoRoot, ok, err := gitRepoRootAt(ctx, realDir, ceiling)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusConflict, fmt.Sprintf("not_a_git_repo: %q is not a git work tree", dir))
		return
	}

	head := ""
	if out, err := gitOut(ctx, repoRoot, nil, "rev-parse", "HEAD"); err == nil {
		head = strings.TrimSpace(out)
	}
	// A rev-parse HEAD failure here means an unborn branch (no commit yet):
	// repoRoot is already confirmed a real work tree, so any other cause
	// (corruption) would also fail every subsequent git call and surface as
	// its own 500 rather than silently passing as "unborn".

	branch := ""
	if b, err := gitOut(ctx, repoRoot, nil, "symbolic-ref", "-q", "--short", "HEAD"); err == nil {
		branch = strings.TrimSpace(b)
	}

	resp := gitChangesJSON{Dir: dir, Scope: scope, Branch: branch, Head: head}
	baseTreeish := gitEmptyTreeSHA1
	if head != "" {
		baseTreeish = "HEAD"
	}
	if scope == "branch" {
		if head == "" {
			writeErr(w, http.StatusConflict, "no_base: HEAD has no commit yet")
			return
		}
		display, revision, found := defaultBranchRef(ctx, repoRoot)
		if !found {
			writeErr(w, http.StatusConflict, "no_base: no default branch found (checked origin/HEAD, origin/main, origin/master)")
			return
		}
		mb, err := gitOut(ctx, repoRoot, nil, "merge-base", head, revision)
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				writeErr(w, http.StatusConflict, fmt.Sprintf("no_base: HEAD and %s share no common ancestor", display))
				return
			}
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		sha := strings.TrimSpace(mb)
		resp.Base = &gitBaseRef{Ref: display, SHA: sha}
		baseTreeish = sha
	}

	files, patch, truncated, err := gitChangeSet(ctx, repoRoot, baseTreeish, gitChangesPatchCap)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Files = files
	resp.Patch = patch
	resp.Truncated = truncated
	writeJSONNoEscapeHTML(w, http.StatusOK, resp)
}

// verifyDirWithinRoots re-validates resolveWorkDir's already-cleaned dir
// after resolving symlinks, so a symlink planted under an allowed root
// cannot point this endpoint at a repository outside every workspace root.
// It also turns a missing dir into a clean 400 instead of a git subprocess
// failure that isn't an *exec.ExitError. ceiling is the parent of whichever
// root matched, for GIT_CEILING_DIRECTORIES to bound repo discovery at that
// same boundary.
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

// gitCmdHook, when non-nil, is called with every git subprocess's argv
// before it runs — a test-only seam (see git_changes_test.go); always nil in
// production.
var gitCmdHook func(args []string)

// gitStaticSafetyArgs are `-c` overrides every invocation carries: disable
// hook-based fsmonitor (a repo-configured command that runs on every diff
// and can hang), route hooks to /dev/null, and require explicit opt-in
// before treating a directory as a bare repository.
var gitStaticSafetyArgs = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "safe.bareRepository=explicit",
}

// gitCmd builds a git subprocess bounded by ctx. GIT_OPTIONAL_LOCKS=0 and
// running every diff against a private index copy (see gitChangeSet) keep
// the real .git/index untouched. GIT_LITERAL_PATHSPECS=1 keeps a "--"
// pathspec built from a real file's name (e.g. "b*.txt") from being
// reinterpreted as a glob or `:(...)` magic pathspec.
func gitCmd(ctx context.Context, dir string, extraEnv []string, args ...string) *exec.Cmd {
	args = append(append([]string{}, gitStaticSafetyArgs...), args...)
	if gitCmdHook != nil {
		gitCmdHook(args)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_LITERAL_PATHSPECS=1"), extraEnv...)
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

// isGitWorkTreeErr reports whether err is a plain non-zero git exit (as
// opposed to a start/exec failure) — the distinction gitRepoRootAt needs to
// turn "not a repo" into a clean 409 rather than a 500 for e.g. a missing
// git binary.
func isGitWorkTreeErr(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// gitRepoRootAt resolves dir's git work tree root, bounding discovery at
// ceiling (GIT_CEILING_DIRECTORIES) so it can never walk up past an allowed
// workspace root. ok is false, with a nil error, exactly when dir is not
// inside any git work tree up to that boundary.
func gitRepoRootAt(ctx context.Context, dir, ceiling string) (root string, ok bool, err error) {
	out, err := gitOut(ctx, dir, []string{"GIT_CEILING_DIRECTORIES=" + ceiling}, "rev-parse", "--show-toplevel")
	if err != nil {
		if isGitWorkTreeErr(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(out), true, nil
}

// defaultBranchRef resolves scope=branch's diff base, in the contract's
// documented order: refs/remotes/origin/HEAD's symbolic target, else
// refs/remotes/origin/main, else refs/remotes/origin/master. Every
// candidate's target is verified to actually resolve before it is accepted,
// so a stale origin/HEAD (left pointing at a branch a `fetch --prune`
// removed) falls through to the next candidate instead of 500ing. display
// is the human-facing ref (e.g. "origin/main"); revision is what merge-base
// is actually called with.
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

// parseNumstatZ parses `git diff --numstat -z` output into a map keyed by
// each file's new (current) path. A rename or copy prints its added/deleted
// counts followed by an EMPTY path field, then old and new path as separate
// NUL-terminated fields.
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

// statusWord maps a name-status letter to the contract's status word. C
// (copy) and T (type-change) are not documented outcomes; they fall back to
// "modified" rather than an empty or invalid status.
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

// untrackedIndexInput builds the NUL-terminated stdin for `git add -N
// --pathspec-from-file=- --pathspec-file-nul`, from ls-files -o's raw -z
// output, re-verified with Lstat: an entry ls-files already reported may
// have vanished since (a race with the agent's own edits — `add -N` aborts
// its ENTIRE batch on one missing pathspec) or be a directory (an untracked
// nested git repository, which ls-files reports as e.g. "vendor/dep/"
// without recursing into it — git add -N can't intent-to-add a directory).
// Both are silently dropped from the request rather than failing it or
// inventing a status the contract doesn't define.
func untrackedIndexInput(repoRoot, lsFilesOut string) []byte {
	var buf bytes.Buffer
	for _, p := range splitNulZ(lsFilesOut) {
		info, err := os.Lstat(filepath.Join(repoRoot, p))
		if err != nil || info.IsDir() {
			continue
		}
		buf.WriteString(p)
		buf.WriteByte(0)
	}
	return buf.Bytes()
}

// diffFilterDriverArgs neutralizes every repo-configured clean/process
// content filter driver (e.g. git-lfs, or a repo-local .gitattributes
// filter) so diffing a working-tree file can never execute one: a filter
// driver is an arbitrary command that git itself runs to transform a blob's
// content, and it can be configured at any config scope (this box's system
// gitconfig has git-lfs's filter.lfs.* registered, so this is not
// hypothetical). Discovered once per request, not per file.
func diffFilterDriverArgs(ctx context.Context, dir string) ([]string, error) {
	out, err := gitOut(ctx, dir, nil, "config", "--null", "--name-only", "--get-regexp", `^filter\..*\.(clean|process)$`)
	if err != nil {
		if isGitWorkTreeErr(err) {
			return nil, nil // no configured filter driver matches
		}
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, key := range splitNulZ(out) {
		parts := strings.SplitN(key, ".", 3)
		if len(parts) == 3 && parts[0] == "filter" && !seen[parts[1]] {
			seen[parts[1]] = true
			names = append(names, parts[1])
		}
	}
	sort.Strings(names)
	var args []string
	for _, name := range names {
		args = append(args,
			"-c", "filter."+name+".clean=",
			"-c", "filter."+name+".process=",
			"-c", "filter."+name+".required=false",
		)
	}
	return args, nil
}

// gitChangeSet computes files and patch for GET /git/changes' committed-vs-
// working-tree comparison against baseTreeish, folding in untracked files
// as "added". It runs a constant number of git subprocesses regardless of
// how many files changed: untracked paths are folded into the SAME
// numstat/name-status/patch invocations that cover tracked changes, by
// pointing GIT_INDEX_FILE at a private copy of the real index with those
// paths staged --intent-to-add (see untrackedIndexInput) — the real
// .git/index is only ever read (a plain file copy), never opened by any git
// command. files is always complete; patch stops at the last whole file
// that fits within patchCap, without reading or generating any file's diff
// past that point.
func gitChangeSet(ctx context.Context, repoRoot, baseTreeish string, patchCap int) (files []gitChangeFile, patch string, truncated bool, err error) {
	tmpDir, err := os.MkdirTemp("", "harness-git-changes-")
	if err != nil {
		return nil, "", false, err
	}
	defer os.RemoveAll(tmpDir)
	tmpIndex := filepath.Join(tmpDir, "index")

	realIndexOut, err := gitOut(ctx, repoRoot, nil, "rev-parse", "--git-path", "index")
	if err != nil {
		return nil, "", false, err
	}
	realIndex := strings.TrimSpace(realIndexOut)
	if !filepath.IsAbs(realIndex) {
		realIndex = filepath.Join(repoRoot, realIndex)
	}
	if data, err := os.ReadFile(realIndex); err == nil {
		if err := os.WriteFile(tmpIndex, data, 0o600); err != nil {
			return nil, "", false, err
		}
	} else if !os.IsNotExist(err) {
		return nil, "", false, err
	}
	// A missing realIndex (an unborn repository that has never run `git
	// add`) leaves tmpIndex unwritten too: git treats a GIT_INDEX_FILE path
	// that doesn't exist as a fresh empty index, exactly like a real unborn
	// repository's own index.

	env := []string{"GIT_INDEX_FILE=" + tmpIndex}

	lsFilesOut, err := gitOut(ctx, repoRoot, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, "", false, err
	}
	if input := untrackedIndexInput(repoRoot, lsFilesOut); len(input) > 0 {
		cmd := gitCmd(ctx, repoRoot, env, "add", "-N", "--pathspec-from-file=-", "--pathspec-file-nul", "--")
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, "", false, fmt.Errorf("git add -N: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
	}

	filterArgs, err := diffFilterDriverArgs(ctx, repoRoot)
	if err != nil {
		return nil, "", false, err
	}
	// Global -c overrides must precede the "diff" subcommand; every other
	// flag is a diff option and must follow it.
	diffArgs := func(rest ...string) []string {
		args := []string{"-c", "diff.autoRefreshIndex=false"}
		args = append(args, filterArgs...)
		args = append(args, "diff", "--no-ext-diff", "--no-textconv", "--submodule=short", "--ignore-submodules=dirty")
		return append(args, rest...)
	}

	numstatOut, err := gitOut(ctx, repoRoot, env, diffArgs("--numstat", "-z", "-M", baseTreeish)...)
	if err != nil {
		return nil, "", false, err
	}
	nameStatusOut, err := gitOut(ctx, repoRoot, env, diffArgs("--name-status", "-z", "-M", baseTreeish)...)
	if err != nil {
		return nil, "", false, err
	}
	numstat := parseNumstatZ(numstatOut)

	files = []gitChangeFile{}
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
	}

	patch, truncated, err = runPatchCapped(ctx, repoRoot, env,
		diffArgs("--no-color", "-M", baseTreeish), patchCap)
	if err != nil {
		return nil, "", false, err
	}
	return files, patch, truncated, nil
}

// runPatchCapped runs a `git diff` whose stdout may be arbitrarily large,
// reading at most patchCap+1 bytes so memory use never scales with the
// diff's real size. Reading exactly that many bytes means more output
// remains; runPatchCapped then kills the subprocess rather than draining
// it, and cuts the captured prefix back to the last complete file's "diff
// --git " boundary within patchCap bytes — never a partial hunk.
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

	data, readErr := io.ReadAll(io.LimitReader(stdout, int64(patchCap)+1))
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", false, fmt.Errorf("git %s: %w", strings.Join(args, " "), readErr)
	}

	if len(data) > patchCap {
		cut := lastWholeFileBoundary(data[:patchCap])
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // reap; Wait's own error is expected (killed) and not reported
		return string(data[:cut]), true, nil
	}
	if err := cmd.Wait(); err != nil {
		return "", false, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(data), false, nil
}

// lastWholeFileBoundary returns the length of the longest prefix of data
// that ends exactly at a file boundary: right before some "diff --git "
// header, never mid-hunk. Every such header after the first is preceded by
// a newline (the previous file's last line); the first file's own header at
// offset 0 has no such preceding newline and so is never matched here,
// which is correct — data is a possibly mid-file truncated prefix, so
// finding no second boundary means not even the first file is confirmed
// complete within it.
func lastWholeFileBoundary(data []byte) int {
	idx := bytes.LastIndex(data, []byte("\ndiff --git "))
	if idx < 0 {
		return 0
	}
	return idx + 1
}

// writeJSONNoEscapeHTML is writeJSON's counterpart for a response whose
// string fields (here, unified diff text) legitimately contain a lot of
// '<', '>', and '&' — e.g. a diff of HTML or JSX. json.Marshal's default
// HTML-escaping can inflate such a payload well past its documented cap;
// this endpoint's patch is capped in raw bytes, so escaping it back out
// would silently break that contract.
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
