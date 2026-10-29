package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/majorcontext/harness/protocol"
)

// Templates for jj log. A commitRow has fields split by a tab. A trunkNames
// line is a name after a rank, so a remote bookmark sorts before a local one.
const (
	commitRow      = `change_id ++ "\t" ++ commit_id ++ "\t" ++ root ++ "\n"`
	localBookmarks = `local_bookmarks.map(|b| b.name()).join("\n") ++ "\n"`
	trunkNames     = `remote_bookmarks.filter(|b| b.remote() != "git").map(|b| "0 " ++ b.name() ++ "@" ++ b.remote()).join("\n") ++ "\n" ++ local_bookmarks.map(|b| "1 " ++ b.name()).join("\n") ++ "\n"`
)

// jjRepo is an open jj work tree. op is the newest operation, so every
// command reads one fixed view and never merges divergent operations.
type jjRepo struct {
	root string
	repo string
	op   string
}

// jjCommit is one row of commitRow.
type jjCommit struct {
	change, commit string
	root           bool
}

// openJJ resolves the repository directory of the work tree at root. A
// secondary workspace has a .jj/repo file that names it.
func openJJ(ctx context.Context, root string) (jjRepo, error) {
	repo := filepath.Join(root, ".jj", "repo")
	if data, err := os.ReadFile(repo); err == nil {
		repo = strings.TrimSpace(string(data))
		if !filepath.IsAbs(repo) {
			repo = filepath.Join(root, ".jj", repo)
		}
	}
	r := jjRepo{root: root, repo: filepath.Clean(repo)}
	op, err := r.newestOp(ctx)
	r.op = op
	return r, err
}

// newestOp returns the operation of the one operation head, or the head that
// ended last when concurrent commands left several.
func (r jjRepo) newestOp(ctx context.Context) (string, error) {
	heads, err := os.ReadDir(filepath.Join(r.repo, "op_heads", "heads"))
	if err != nil {
		return "", err
	}
	best, bestEnd := "", int64(-1)
	for _, h := range heads {
		id := h.Name()
		if len(heads) == 1 {
			return id, nil
		}
		out, err := r.run(ctx, id, "op", "log", "--limit", "1", "--no-graph", "-T", `time.end().format("%s")`)
		if err != nil {
			return "", err
		}
		end, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if err != nil {
			return "", fmt.Errorf("jj op log: end time %q: %w", out, err)
		}
		if end > bestEnd || end == bestEnd && id > best {
			best, bestEnd = id, end
		}
	}
	if best == "" {
		return "", fmt.Errorf("jj repository %s has no operation head", r.repo)
	}
	return best, nil
}

// run runs jj at operation op. --ignore-working-copy never snapshots, and
// --at-operation never merges divergent operations, so no command writes.
// The config overrides keep user settings from changing the output.
func (r jjRepo) run(ctx context.Context, op string, args ...string) (string, error) {
	full := append([]string{"--ignore-working-copy", "-R", r.root, "--at-operation=" + op,
		"--no-pager", "--color=never", "--quiet",
		"--config=ui.paginate=never", "--config=ui.color=never", "--config=ui.progress=false"}, args...)
	cmd := exec.CommandContext(ctx, "jj", full...)
	cmd.Dir = r.root
	cmd.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("jj %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// lines runs jj and returns the non-empty lines of its output.
func (r jjRepo) lines(ctx context.Context, args ...string) ([]string, error) {
	out, err := r.run(ctx, r.op, args...)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

func parseJJCommits(rows []string) []jjCommit {
	var out []jjCommit
	for _, row := range rows {
		if f := strings.Split(row, "\t"); len(f) == 3 {
			out = append(out, jjCommit{change: f[0], commit: f[1], root: f[2] == "true"})
		}
	}
	return out
}

func (r jjRepo) parents(ctx context.Context) ([]jjCommit, error) {
	rows, err := r.lines(ctx, "log", "-r", "parents(@)", "--no-graph", "-T", commitRow)
	return parseJJCommits(rows), err
}

// gitDir is the git directory of the repository: git_target names it
// relative to the store.
func (r jjRepo) gitDir() (string, error) {
	store := filepath.Join(r.repo, "store")
	data, err := os.ReadFile(filepath.Join(store, "git_target"))
	if err != nil {
		return "", fmt.Errorf("jj repository has no git object store: %w", err)
	}
	target := strings.TrimSpace(string(data))
	if !filepath.IsAbs(target) {
		target = filepath.Join(store, target)
	}
	return filepath.Clean(target), nil
}

// seedTree returns the tree-ish that the diff runs against: base, or the
// tree of the one parent of @, the empty tree for the root commit, or the
// automatic merge of the trees of several parents. A merge writes its trees
// into the private object directory of env.
func (r jjRepo) seedTree(ctx context.Context, env []string, base string) (string, error) {
	if base != "" {
		return base, nil
	}
	parents, err := r.parents(ctx)
	if err != nil {
		return "", err
	}
	if len(parents) == 0 || len(parents) == 1 && parents[0].root {
		return emptyTree(ctx, r.root, env)
	}
	if len(parents) == 1 {
		return parents[0].commit, nil
	}
	ids := make([]string, len(parents))
	for i, p := range parents {
		ids[i] = p.commit
	}
	return r.mergeTrees(ctx, env, ids)
}

// mergeTrees folds the commits pairwise into one tree. The merge base is the
// common ancestor of all of them, or the empty tree when they share none.
func (r jjRepo) mergeTrees(ctx context.Context, env, ids []string) (string, error) {
	mb, err := emptyTree(ctx, r.root, env)
	if err != nil {
		return "", err
	}
	if out, err := gitOut(ctx, r.root, env, append([]string{"merge-base", "--octopus"}, ids...)...); err == nil {
		mb = strings.TrimSpace(out)
	} else if !isGitWorkTreeErr(err) {
		return "", err
	}
	acc := ids[0]
	for _, id := range ids[1:] {
		// A conflict exits 1 and still prints the tree, with the markers.
		cmd := gitCmd(ctx, r.root, env, "merge-tree", "--write-tree", "--merge-base="+mb, acc, id)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		err := cmd.Run()
		if exitErr, ok := errors.AsType[*exec.ExitError](err); err != nil && (!ok || exitErr.ExitCode() != 1) {
			return "", fmt.Errorf("git merge-tree: %w", err)
		}
		tree, _, _ := strings.Cut(stdout.String(), "\n")
		if len(tree) < 40 {
			return "", fmt.Errorf("git merge-tree: no tree for the parents of @: %v", err)
		}
		acc = tree
	}
	return acc, nil
}

// conflicts lists the files that @ records as conflicts. It lists the files
// of @ only when @ has a conflict.
func (r jjRepo) conflicts(ctx context.Context) ([]string, error) {
	has, err := r.lines(ctx, "log", "-r", "@", "--no-graph", "-T", `if(conflict, "conflict\n")`)
	if err != nil || len(has) == 0 {
		return nil, err
	}
	return r.lines(ctx, "file", "list", "-r", "@", "-T", `if(conflict, path.display() ++ "\n")`)
}

// changesCommandTimeout bounds one run of a changes command.
const changesCommandTimeout = 10 * time.Second

// changesCommand returns harness.changes-command of the repository config,
// or nil when it is unset. Any value but an array of strings is an error.
func (r jjRepo) changesCommand(ctx context.Context) ([]string, error) {
	const key = "harness.changes-command"
	lines, err := r.lines(ctx, "config", "list", "--repo", key, "-T", `name ++ " " ++ json(value) ++ "\n"`)
	if err != nil || len(lines) == 0 {
		return nil, err
	}
	for _, l := range lines {
		if raw, ok := strings.CutPrefix(l, key+" "); ok {
			var argv []string
			if err := json.Unmarshal([]byte(raw), &argv); err != nil || len(argv) == 0 {
				break
			}
			return argv, nil
		}
	}
	return nil, fmt.Errorf("%s in the jj repository config must be an array of strings", key)
}

// runChangesCommand runs argv in the root and decodes its stdout as the
// answer. The scope is branch exactly when base is set.
func (r jjRepo) runChangesCommand(ctx context.Context, argv []string, head Head, base Base, patchCap int) ([]protocol.ChangedFile, string, bool, error) {
	scope, commit := protocol.ScopeBranch, base.Rev.ID
	if commit == "" {
		scope, commit = protocol.ScopeUncommitted, head.Commit
	}
	ctx, cancel := context.WithTimeout(ctx, changesCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = r.root
	cmd.Env = append(os.Environ(), "HARNESS_CHANGES_SCOPE="+scope, "HARNESS_CHANGES_BASE="+commit,
		"HARNESS_CHANGES_PATCH_BYTES="+strconv.Itoa(patchCap))
	cmd.WaitDelay = waitDelay
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	reason := ""
	var out protocol.WorkspaceChanges
	if err := cmd.Run(); err != nil {
		reason = "exit or start failure: " + err.Error()
		if ctx.Err() != nil {
			reason = "run over " + changesCommandTimeout.String()
		}
	} else if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		reason = "stdout is not a WorkspaceChanges document"
	}
	if reason != "" {
		slog.Warn("workspace changes command failed", "command", argv[0], "reason", reason)
		return nil, "", false, fmt.Errorf("changes command %q failed: %s", argv[0], reason)
	}
	if out.Files == nil {
		out.Files = []protocol.ChangedFile{}
	}
	return out.Files, out.Patch, out.Truncated, nil
}
