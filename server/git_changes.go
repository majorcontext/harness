package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	head := "" // empty means an unborn branch (no commit yet)
	if out, err := gitOut(ctx, repoRoot, nil, "rev-parse", "HEAD"); err == nil {
		head = strings.TrimSpace(out)
	}

	branch := ""
	if b, err := gitOut(ctx, repoRoot, nil, "symbolic-ref", "-q", "--short", "HEAD"); err == nil {
		branch = strings.TrimSpace(b)
	}

	resp := gitChangesJSON{Dir: dir, Scope: scope, Branch: branch, Head: head}
	baseTreeish := "HEAD"
	if head == "" {
		emptyTree, err := gitEmptyTree(ctx, repoRoot)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		baseTreeish = emptyTree
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

// gitStaticSafetyArgs disable hook-based fsmonitor and require explicit
// opt-in before treating a directory as bare. No command run here uses
// hooks, so core.hooksPath is not set.
var gitStaticSafetyArgs = []string{
	"-c", "core.fsmonitor=false",
	"-c", "safe.bareRepository=explicit",
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
	cmd.Env = append(append(os.Environ(),
		"GIT_OPTIONAL_LOCKS=0", "GIT_LITERAL_PATHSPECS=1", "GIT_NO_LAZY_FETCH=1"), extraEnv...)
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
	out, err := gitOut(ctx, dir, []string{"GIT_CEILING_DIRECTORIES=" + ceiling}, "rev-parse", "--show-toplevel")
	if err != nil {
		if isGitWorkTreeErr(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(out), true, nil
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

// untrackedIndexInput builds `add -N --pathspec-from-file=-`'s NUL-
// terminated stdin from `ls-files -o --directory`'s raw -z output: a wholly
// untracked directory is one pathspec, not one per file, keeping git's own
// (quadratic) pathspec matching off a large untracked tree. Each entry is
// re-Lstat'd (it may have vanished since ls-files ran); a directory
// containing a nested repo is expanded to its own files instead, skipping
// that repo's subtree.
func untrackedIndexInput(repoRoot, lsFilesOut string) []byte {
	var buf bytes.Buffer
	add := func(p string) {
		buf.WriteString(p)
		buf.WriteByte(0)
	}
	for _, p := range splitNulZ(lsFilesOut) {
		full := filepath.Join(repoRoot, p)
		info, err := os.Lstat(full)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			add(p)
			continue
		}
		if hasNestedRepo(full) {
			addPlainFiles(repoRoot, full, add)
		} else {
			add(p)
		}
	}
	return buf.Bytes()
}

// hasNestedRepo reports whether any directory at or under root contains its
// own .git, which a parent pathspec can't recurse `add -N` into.
func hasNestedRepo(root string) bool {
	found := false
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipAll
		}
		if d.Name() == ".git" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// addPlainFiles walks root calling add with each file's repoRoot-relative
// path, skipping any nested-repo subtree.
func addPlainFiles(repoRoot, root string, add func(string)) {
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if _, gerr := os.Lstat(filepath.Join(path, ".git")); gerr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if rel, rerr := filepath.Rel(repoRoot, path); rerr == nil {
			add(rel)
		}
		return nil
	})
}

// addUntrackedIntentToAdd stages every untracked path intent-to-add in the
// private index at env's GIT_INDEX_FILE. A path vanishing between ls-files
// and add -N fails that whole batch, so this retries once against a fresh
// listing. core.splitIndex=false keeps add -N from writing a shared-index
// file into the real repository.
func addUntrackedIntentToAdd(ctx context.Context, repoRoot string, env []string) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		lsFilesOut, err := gitOut(ctx, repoRoot, nil, "ls-files", "--others", "--exclude-standard", "--directory", "-z")
		if err != nil {
			return err
		}
		input := untrackedIndexInput(repoRoot, lsFilesOut)
		if len(input) == 0 {
			return nil
		}
		cmd := gitCmd(ctx, repoRoot, env, "-c", "core.splitIndex=false", "add", "-N", "--pathspec-from-file=-", "--pathspec-file-nul", "--")
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			lastErr = fmt.Errorf("git add -N: %w: %s", err, strings.TrimSpace(stderr.String()))
			continue
		}
		return nil
	}
	return lastErr
}

// diffFilterDriverArgs neutralizes every repo-configured clean/process
// filter driver, discovered once per request, not per file.
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

// gitChangeSet computes files and patch against baseTreeish, folding
// untracked files in as "added" via a private index copy, in a constant
// number of subprocesses regardless of file count. files is always
// complete; patch stops at the last whole file within patchCap.
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
	// A missing realIndex (unborn repository) leaves tmpIndex unwritten:
	// git treats a nonexistent GIT_INDEX_FILE as a fresh empty index.

	env := []string{"GIT_INDEX_FILE=" + tmpIndex}

	if err := addUntrackedIntentToAdd(ctx, repoRoot, env); err != nil {
		return nil, "", false, err
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
