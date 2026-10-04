// Package workspace reads the git work tree of a directory. It shells out
// to git and never reaches a runtime or a session.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/majorcontext/harness/protocol"
)

var (
	// ErrInvalid reports a bad scope or a dir outside the root.
	ErrInvalid = errors.New("invalid request")
	// ErrNotRepo reports a dir that is not in a git work tree.
	ErrNotRepo = errors.New("not_a_git_repo")
	// ErrNoBase reports a branch scope with no merge base.
	ErrNoBase = errors.New("no_base")
	// ErrTooManyChanges reports a diff that exceeds its time or size bound.
	ErrTooManyChanges = errors.New("too_many_changes")
)

// budget bounds one Changes call. A caller that proxies the route keeps its
// own timeout a few seconds above it, so a too_many_changes answer arrives
// before the caller gives up.
const budget = 28 * time.Second

const maxPatch = 1 << 20

// Changes diffs the git work tree of dir by scope: "branch" (the default)
// against the merge base with the default branch, or "uncommitted" against
// HEAD. Untracked files count as added. An empty dir is root itself, and
// git may find its repository above root. Any other dir, relative to root,
// must stay under root after symlinks, and so must its repository.
func Changes(ctx context.Context, root, dir, scope string) (protocol.WorkspaceChanges, error) {
	if scope == "" {
		scope = protocol.ScopeBranch
	}
	if scope != protocol.ScopeBranch && scope != protocol.ScopeUncommitted {
		return protocol.WorkspaceChanges{}, fmt.Errorf("%w: scope %q must be \"branch\" or \"uncommitted\"", ErrInvalid, scope)
	}
	abs, real, ceiling, err := resolve(root, dir)
	if err != nil {
		return protocol.WorkspaceChanges{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	c, err := changes(ctx, root, abs, real, ceiling, scope, dir != "")
	if ctx.Err() != nil || errors.Is(err, errMetadataTooLarge) {
		return protocol.WorkspaceChanges{}, fmt.Errorf("%w: request exceeded its time budget diffing a large change set", ErrTooManyChanges)
	}
	return c, err
}

// resolve returns the absolute dir, its real path, and the git ceiling: the
// parent of root for an explicit dir, or none for root itself.
func resolve(root, dir string) (abs, real, ceiling string, err error) {
	if dir == "" {
		real, err = filepath.EvalSymlinks(root)
		return root, real, "", err
	}
	abs = dir
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, abs)
	}
	abs = filepath.Clean(abs)
	if r := filepath.Clean(root); abs != r && !strings.HasPrefix(abs, r+string(os.PathSeparator)) {
		return "", "", "", fmt.Errorf("%w: workdir %q is not under an allowed workspace root", ErrInvalid, dir)
	}
	real, ceiling, err = within(root, abs)
	return abs, real, ceiling, err
}

// within checks dir after symlinks, so a symlink under root cannot point
// outside it, and rejects a missing dir before any git subprocess runs.
func within(root, dir string) (real, ceiling string, err error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", "", fmt.Errorf("%w: dir %q: %w", ErrInvalid, dir, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("%w: dir %q is not a directory", ErrInvalid, dir)
	}
	if real, err = filepath.EvalSymlinks(dir); err != nil {
		return "", "", err
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		rootReal = filepath.Clean(root)
	}
	if real == rootReal || strings.HasPrefix(real, rootReal+string(os.PathSeparator)) {
		return real, filepath.Dir(rootReal), nil
	}
	return "", "", fmt.Errorf("%w: dir %q escapes every allowed workspace root", ErrInvalid, dir)
}

func changes(ctx context.Context, root, dir, real, ceiling, scope string, explicit bool) (protocol.WorkspaceChanges, error) {
	repoRoot, ok, err := repoRootAt(ctx, real, ceiling)
	if err != nil {
		return protocol.WorkspaceChanges{}, err
	}
	// GIT_CEILING_DIRECTORIES has no escape for a colon in a path, so git
	// can walk past a root that holds one; check the repository root too.
	if ok && explicit {
		_, _, err := within(root, repoRoot)
		ok = err == nil
	}
	if !ok {
		return protocol.WorkspaceChanges{}, fmt.Errorf("%w: %q is not a git work tree", ErrNotRepo, dir)
	}
	head := ""
	if out, err := gitOut(ctx, repoRoot, nil, "rev-parse", "HEAD"); err == nil {
		head = strings.TrimSpace(out)
	} else if ctx.Err() != nil {
		return protocol.WorkspaceChanges{}, err
	}
	branch := ""
	if b, err := gitOut(ctx, repoRoot, nil, "symbolic-ref", "-q", "--short", "HEAD"); err == nil {
		branch = strings.TrimSpace(b)
	}
	c := protocol.WorkspaceChanges{Dir: dir, Scope: scope, Branch: branch, Head: head}
	base, err := baseOf(ctx, repoRoot, &c)
	if err != nil {
		return protocol.WorkspaceChanges{}, err
	}
	c.Files, c.Patch, c.Truncated, err = changeSet(ctx, repoRoot, head, base, maxPatch)
	return c, err
}

// baseOf returns the tree-ish that c diffs against, and sets c.Base for the
// branch scope. An unborn HEAD diffs against the empty tree.
func baseOf(ctx context.Context, repoRoot string, c *protocol.WorkspaceChanges) (string, error) {
	switch {
	case c.Scope == protocol.ScopeBranch:
		if c.Head == "" {
			return "", fmt.Errorf("%w: HEAD has no commit yet", ErrNoBase)
		}
		display, revision, found := defaultBranchRef(ctx, repoRoot)
		if !found {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("%w: no default branch found (checked origin/HEAD, origin/main, origin/master)", ErrNoBase)
		}
		mb, err := gitOut(ctx, repoRoot, nil, "merge-base", c.Head, revision)
		if err != nil {
			if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && ctx.Err() == nil && exitErr.ExitCode() == 1 {
				return "", fmt.Errorf("%w: HEAD and %s share no common ancestor", ErrNoBase, display)
			}
			return "", err
		}
		c.Base = &protocol.BaseRef{Ref: display, SHA: strings.TrimSpace(mb)}
		return c.Base.SHA, nil
	case c.Head == "":
		return emptyTree(ctx, repoRoot)
	}
	return "HEAD", nil
}
