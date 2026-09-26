package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// gitChangesTimeout bounds every git subprocess handleGitChanges spawns —
// all local, network-free plumbing against an already-cloned repository, so
// a few seconds is generous; this only keeps a wedged git process from
// hanging the request forever.
const gitChangesTimeout = 30 * time.Second

// gitChangesPatchCap bounds the combined "patch" text at roughly 1 MiB. A
// box's changes panel renders this inline; an agent that has produced a
// multi-megabyte diff needs the file list (always complete) far more than
// it needs every byte of patch text, so the cap trades patch completeness
// for a bounded response instead of ever growing unboundedly.
const gitChangesPatchCap = 1 << 20

// gitChangeFile is one file's entry in GET /git/changes' "files" array.
type gitChangeFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary"`
}

// gitBaseRef is scope=branch's diff base: the default branch's remote-
// tracking ref, and the merge-base commit HEAD actually diffs against.
type gitBaseRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// gitChangesJSON is GET /git/changes' response body.
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

// handleGitChanges answers GET /git/changes: a box's git diff, for the boxes
// web console's Changes panel (see AGENTS.md's contract in the originating
// task). scope=uncommitted diffs HEAD against the working tree; scope=branch
// (default) diffs merge-base(HEAD, default branch) against the working
// tree, so it shows what a PR would contain once the agent commits. Both
// scopes fold in untracked files (as "added") alongside tracked changes.
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

	ctx, cancel := context.WithTimeout(r.Context(), gitChangesTimeout)
	defer cancel()

	ok, err := isGitWorkTree(ctx, dir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusConflict, fmt.Sprintf("not_a_git_repo: %q is not a git work tree", dir))
		return
	}

	head, err := gitOut(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	head = strings.TrimSpace(head)

	branch := ""
	if b, err := gitOut(ctx, dir, "symbolic-ref", "-q", "--short", "HEAD"); err == nil {
		branch = strings.TrimSpace(b)
	}

	resp := gitChangesJSON{Dir: dir, Scope: scope, Branch: branch, Head: head}
	baseTreeish := "HEAD"
	if scope == "branch" {
		display, revision, found := defaultBranchRef(ctx, dir)
		if !found {
			writeErr(w, http.StatusConflict, "no_base: no default branch found (checked origin/HEAD, origin/main, origin/master)")
			return
		}
		mb, err := gitOut(ctx, dir, "merge-base", head, revision)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		sha := strings.TrimSpace(mb)
		resp.Base = &gitBaseRef{Ref: display, SHA: sha}
		baseTreeish = sha
	}

	files, patch, truncated, err := gitChangeSet(ctx, dir, baseTreeish, gitChangesPatchCap)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Files = files
	resp.Patch = patch
	resp.Truncated = truncated
	writeJSON(w, http.StatusOK, resp)
}

// gitCmd builds a git subprocess bounded by ctx, with GIT_OPTIONAL_LOCKS=0 so
// a concurrently-working agent's own git commands (and its index) are never
// contended or mutated by a request running alongside it.
func gitCmd(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	return cmd
}

// gitOut runs a git command to completion and returns its stdout. Any
// non-zero exit is an error — callers that need to tolerate a specific exit
// code (isGitWorkTree, gitDiffOut) inspect *exec.ExitError themselves
// instead of calling this.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := gitCmd(ctx, dir, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// gitDiffOut runs a `git diff --no-index` invocation, where exit code 1
// means "differences found" (diff(1)'s own convention, not an error) and
// only a higher exit code is a real failure.
func gitDiffOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := gitCmd(ctx, dir, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return stdout.String(), nil
	}
	return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
}

// isGitWorkTree reports whether dir is inside a git work tree. A non-git
// directory is a normal false/nil result, not an error — the distinction
// handleGitChanges needs to turn "not a repo" into a clean 409 rather than a
// 500 for e.g. a missing git binary.
func isGitWorkTree(ctx context.Context, dir string) (bool, error) {
	cmd := gitCmd(ctx, dir, "rev-parse", "--is-inside-work-tree")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("git rev-parse --is-inside-work-tree: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()) == "true", nil
}

// defaultBranchRef resolves scope=branch's diff base, in the contract's
// documented order: refs/remotes/origin/HEAD's symbolic target, else
// refs/remotes/origin/main, else refs/remotes/origin/master. display is the
// human-facing ref (e.g. "origin/main"); revision is what merge-base is
// actually called with.
func defaultBranchRef(ctx context.Context, dir string) (display, revision string, found bool) {
	if out, err := gitOut(ctx, dir, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil {
		full := strings.TrimSpace(out)
		return strings.TrimPrefix(full, "refs/remotes/"), full, true
	}
	for _, name := range []string{"main", "master"} {
		ref := "refs/remotes/origin/" + name
		if _, err := gitOut(ctx, dir, "rev-parse", "--verify", "-q", ref); err == nil {
			return "origin/" + name, ref, true
		}
	}
	return "", "", false
}

// diffNumstat is one file's --numstat -z reading: line counts, or Binary
// with both counts left zero when git reports "-"/"-" (its own binary-file
// signal).
type diffNumstat struct {
	additions int
	deletions int
	binary    bool
}

// splitNulZ splits a -z-terminated git output into its NUL-separated
// fields, dropping the single trailing empty field the final terminator
// otherwise produces.
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
// NUL-terminated fields — the same shape `git diff --no-index --numstat -z
// -- /dev/null <path>` happens to use for a plain untracked file (old =
// "/dev/null"), so untrackedNumstat reuses this same parser.
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

// nameStatusEntry is one `git diff --name-status -z` record.
type nameStatusEntry struct {
	status  string // first letter only: A, M, D, R, ...
	oldPath string // set only for a rename (or copy)
	newPath string
}

// parseNameStatusZ parses `git diff --name-status -z` output, preserving
// git's own file order — gitChangeSet relies on that order matching
// parseNumstatZ's underlying diff (same invocation, same tree-ish, same -M).
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

// untrackedNumstat reads one untracked file's line counts via `git diff
// --no-index` against /dev/null, the same bounded, non-mutating comparison
// gitChangeSet's patch half uses for the same file.
func untrackedNumstat(ctx context.Context, dir, path string) (diffNumstat, error) {
	out, err := gitDiffOut(ctx, dir, "diff", "--numstat", "-z", "--no-index", "--", "/dev/null", path)
	if err != nil {
		return diffNumstat{}, err
	}
	return parseNumstatZ(out)[path], nil
}

// gitChangeSet computes files and patch for GET /git/changes' committed-vs-
// working-tree comparison against baseTreeish (either "HEAD", for
// scope=uncommitted, or a merge-base SHA, for scope=branch), folding in
// untracked files as "added". files is always complete; patch stops at the
// last whole file that fits within patchCap, setting truncated when it does.
func gitChangeSet(ctx context.Context, dir, baseTreeish string, patchCap int) (files []gitChangeFile, patch string, truncated bool, err error) {
	numstatOut, err := gitOut(ctx, dir, "diff", "--numstat", "-z", "-M", baseTreeish)
	if err != nil {
		return nil, "", false, err
	}
	nameStatusOut, err := gitOut(ctx, dir, "diff", "--name-status", "-z", "-M", baseTreeish)
	if err != nil {
		return nil, "", false, err
	}
	numstat := parseNumstatZ(numstatOut)

	var parts []string
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
		pathspecs := []string{e.newPath}
		if e.oldPath != "" {
			pathspecs = []string{e.oldPath, e.newPath}
		}
		args := append([]string{"diff", "--no-color", "-M", baseTreeish, "--"}, pathspecs...)
		part, err := gitOut(ctx, dir, args...)
		if err != nil {
			return nil, "", false, err
		}
		parts = append(parts, part)
	}

	othersOut, err := gitOut(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, "", false, err
	}
	for _, p := range splitNulZ(othersOut) {
		ns, err := untrackedNumstat(ctx, dir, p)
		if err != nil {
			return nil, "", false, err
		}
		files = append(files, gitChangeFile{Path: p, Status: "added", Additions: ns.additions, Deletions: ns.deletions, Binary: ns.binary})
		part, err := gitDiffOut(ctx, dir, "diff", "--no-color", "--no-index", "--", "/dev/null", p)
		if err != nil {
			return nil, "", false, err
		}
		parts = append(parts, part)
	}

	var buf strings.Builder
	for _, part := range parts {
		if buf.Len()+len(part) > patchCap {
			truncated = true
			break
		}
		buf.WriteString(part)
	}
	return files, buf.String(), truncated, nil
}
