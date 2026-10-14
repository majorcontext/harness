package workspace

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/majorcontext/harness/protocol"
)

// gitEngine reads a git work tree.
type gitEngine struct{}

// Name returns protocol.EngineGit.
func (gitEngine) Name() string { return protocol.EngineGit }

// Open finds the work tree root of dir below ceiling.
func (gitEngine) Open(ctx context.Context, dir, ceiling string) (Repo, bool, error) {
	root, ok, err := repoRootAt(ctx, dir, ceiling)
	if err != nil || !ok {
		return nil, false, err
	}
	return gitRepo{root: root}, true, nil
}

type gitRepo struct{ root string }

func (g gitRepo) Root() string { return g.root }

// Head returns HEAD as a commit, with the checked-out branch as its label.
func (g gitRepo) Head(ctx context.Context) (Head, error) {
	var h Head
	if out, err := gitOut(ctx, g.root, nil, "rev-parse", "HEAD"); err == nil {
		h.Rev = protocol.Rev{Kind: protocol.RevCommit, ID: strings.TrimSpace(out)}
	} else if ctx.Err() != nil {
		return Head{}, err
	}
	if b, err := gitOut(ctx, g.root, nil, "symbolic-ref", "-q", "--short", "HEAD"); err == nil {
		h.Label = strings.TrimSpace(b)
	}
	return h, nil
}

// Base returns the merge base of head with the default branch.
func (g gitRepo) Base(ctx context.Context, head Head) (Base, error) {
	if head.Rev.ID == "" {
		return Base{}, fmt.Errorf("%w: HEAD has no commit yet", ErrNoBase)
	}
	display, revision, found := defaultBranchRef(ctx, g.root)
	if !found {
		if ctx.Err() != nil {
			return Base{}, ctx.Err()
		}
		return Base{}, fmt.Errorf("%w: no default branch found (checked origin/HEAD, origin/main, origin/master)", ErrNoBase)
	}
	mb, err := gitOut(ctx, g.root, nil, "merge-base", head.Rev.ID, revision)
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && ctx.Err() == nil && exitErr.ExitCode() == 1 {
			return Base{}, fmt.Errorf("%w: HEAD and %s share no common ancestor", ErrNoBase, display)
		}
		return Base{}, err
	}
	return Base{Rev: protocol.Rev{Kind: protocol.RevCommit, ID: strings.TrimSpace(mb)}, Ref: display}, nil
}

// Diff diffs the work tree against base, or against HEAD for a zero base.
// An unborn HEAD diffs against the empty tree.
func (g gitRepo) Diff(ctx context.Context, head Head, base Base, patchCap int) ([]protocol.ChangedFile, string, bool, error) {
	treeish := base.Rev.ID
	switch {
	case treeish != "":
	case head.Rev.ID == "":
		var err error
		if treeish, err = emptyTree(ctx, g.root); err != nil {
			return nil, "", false, err
		}
	default:
		treeish = "HEAD"
	}
	return changeSet(ctx, g.root, head.Rev.ID, treeish, patchCap)
}
