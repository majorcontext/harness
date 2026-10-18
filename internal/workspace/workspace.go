// Package workspace reads the work tree of a directory through an Engine.
// It never reaches a runtime or a session.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/majorcontext/harness/protocol"
)

var (
	// ErrInvalid reports a bad scope or a dir outside the root.
	ErrInvalid = errors.New("invalid request")
	// ErrNotRepo reports a dir that is in no work tree of a known VCS.
	ErrNotRepo = errors.New("not_a_repo")
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

// engineList is the ordered list of engines. The first engine that opens a
// directory answers. Only Changes names it.
var engineList = []Engine{GitEngine{}}

// Engine reads one kind of work tree.
type Engine interface {
	// Name is the value of the engine field of the answer.
	Name() string
	// Open returns the repository that holds dir below ceiling, or false
	// when dir is in no work tree of this kind.
	Open(ctx context.Context, dir, ceiling string) (Repo, bool, error)
}

// Repo is an open work tree. It exposes revisions and file lists, never a
// storage path.
type Repo interface {
	// Root is the root directory of the work tree.
	Root() string
	// Head returns the revision that the working-copy edits are relative to.
	// Its Rev is the zero Rev when that revision is a merge of several parents
	// or has no commit; the edits are then against the automatic merge of the
	// parents, or the empty tree.
	Head(ctx context.Context) (Head, error)
	// Base returns the base of the branch scope. It wraps ErrNoBase when the
	// work tree has no base; any other error is a failure of the engine.
	Base(ctx context.Context, head Head) (Base, error)
	// Diff lists the changed files and the patch against base. A zero base
	// means the head. The patch is a git-format unified diff and ends at the
	// last whole file within patchCap.
	Diff(ctx context.Context, head Head, base Base, patchCap int) (files []protocol.ChangedFile, patch string, truncated bool, err error)
}

// Head is the head of a work tree. Label is an optional name, such as a branch.
type Head struct {
	Rev   protocol.Rev
	Label string
}

// Base is the base of the branch scope. Ref is its display name.
type Base struct {
	Rev protocol.Rev
	Ref string
}

// Changes diffs the work tree of dir by scope: "branch" (the default)
// against the base of the repository, or "uncommitted" against its head.
// Untracked files count as added. An empty dir is root itself, and the
// engine may find its repository above root. Any other dir, relative to root,
// must stay under root after symlinks, and so must its repository.
func Changes(ctx context.Context, root, dir, scope string) (protocol.WorkspaceChanges, error) {
	return ChangesWith(ctx, engineList, root, dir, scope)
}

// ChangesWith is Changes over engines, which it tries in order. A contract
// test passes the engine under test.
func ChangesWith(ctx context.Context, engines []Engine, root, dir, scope string) (protocol.WorkspaceChanges, error) {
	if scope == "" {
		scope = protocol.ScopeBranch
	}
	if scope != protocol.ScopeBranch && scope != protocol.ScopeUncommitted {
		return protocol.WorkspaceChanges{}, fmt.Errorf("%w: scope %q must be \"branch\" or \"uncommitted\"", ErrInvalid, scope)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return protocol.WorkspaceChanges{}, err
	}
	abs, real, ceiling, err := resolve(root, dir)
	if err != nil {
		return protocol.WorkspaceChanges{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	c, err := changes(ctx, engines, root, abs, real, ceiling, scope, dir != "")
	if ctx.Err() != nil {
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

func changes(ctx context.Context, engines []Engine, root, dir, real, ceiling, scope string, explicit bool) (protocol.WorkspaceChanges, error) {
	var eng Engine
	var repo Repo
	ok := false
	for _, e := range engines {
		r, found, err := e.Open(ctx, real, ceiling)
		if err != nil {
			return protocol.WorkspaceChanges{}, err
		}
		if found {
			eng, repo, ok = e, r, true
			break
		}
	}
	// An engine can find a repository above the ceiling, so check the
	// repository root too.
	if ok && explicit {
		_, _, err := within(root, repo.Root())
		ok = err == nil
	}
	if !ok {
		return protocol.WorkspaceChanges{}, fmt.Errorf("%w: %q is not a work tree of a known VCS", ErrNotRepo, dir)
	}
	head, err := repo.Head(ctx)
	if err != nil {
		return protocol.WorkspaceChanges{}, err
	}
	c := protocol.WorkspaceChanges{Dir: dir, Scope: scope, Engine: eng.Name(), Branch: head.Label, HeadRev: head.Rev}
	var base Base
	if scope == protocol.ScopeBranch {
		if base, err = repo.Base(ctx, head); err != nil {
			return protocol.WorkspaceChanges{}, err
		}
		c.Base = &protocol.BaseRef{Ref: base.Ref, Rev: base.Rev}
	}
	c.Files, c.Patch, c.Truncated, err = repo.Diff(ctx, head, base, maxPatch)
	return c, err
}
