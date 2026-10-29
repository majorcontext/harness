package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/majorcontext/harness/protocol"
)

const engineJJ = "jj"

// JJEngine reads a Jujutsu work tree.
type JJEngine struct{}

// Name returns "jj".
func (JJEngine) Name() string { return engineJJ }

// Open finds the nearest directory from dir up to, and not including,
// ceiling that holds a .jj directory. Without jj on PATH it finds none.
func (JJEngine) Open(ctx context.Context, dir, ceiling string) (Repo, bool, error) {
	for cur := dir; cur != ceiling; cur = filepath.Dir(cur) {
		if info, err := os.Stat(filepath.Join(cur, ".jj")); err == nil && info.IsDir() {
			if _, err := exec.LookPath("jj"); err != nil {
				return nil, false, nil
			}
			r, err := openJJ(ctx, cur)
			return r, err == nil, err
		}
		if filepath.Dir(cur) == cur {
			break
		}
	}
	return nil, false, nil
}

func (r jjRepo) Root() string { return r.root }

// Head returns the one parent of @ as a change, with its git commit ID. The
// label is the first local bookmark of the nearest ancestor that has one.
func (r jjRepo) Head(ctx context.Context) (Head, error) {
	parents, err := r.parents(ctx)
	if err != nil {
		return Head{}, err
	}
	var h Head
	if len(parents) == 1 && !parents[0].root {
		h.Rev = protocol.Rev{Kind: protocol.RevChange, ID: parents[0].change}
		h.Commit = parents[0].commit
	}
	if len(parents) > 0 {
		names, err := r.lines(ctx, "log", "-r", "latest(::@- & bookmarks())", "--no-graph", "-T", localBookmarks)
		if err != nil {
			return Head{}, err
		}
		sort.Strings(names)
		if len(names) > 0 {
			h.Label = names[0]
		}
	}
	return h, nil
}

// Base returns the fork point of the parents of @ and trunk(), so several
// parents give the base of their automatic merge.
func (r jjRepo) Base(ctx context.Context, _ Head) (Base, error) {
	rows, err := r.lines(ctx, "log", "-r", "latest(fork_point(parents(@) | trunk()))", "--no-graph", "-T", commitRow)
	if err != nil {
		return Base{}, err
	}
	fork := parseJJCommits(rows)
	if len(fork) != 1 || fork[0].root {
		return Base{}, fmt.Errorf("%w: trunk() has no common ancestor with the head", ErrNoBase)
	}
	trunk, err := r.lines(ctx, "log", "-r", "latest(trunk())", "--no-graph", "-T", trunkNames)
	if err != nil {
		return Base{}, err
	}
	sort.Strings(trunk)
	ref := "trunk()"
	if len(trunk) > 0 {
		ref = trunk[0][2:]
	}
	return Base{Rev: protocol.Rev{Kind: protocol.RevCommit, ID: fork[0].commit}, Ref: ref}, nil
}

// Diff diffs the files on disk against base, or against the head for a zero
// base. A file that @ records as a conflict is conflicted.
func (r jjRepo) Diff(ctx context.Context, _ Head, base Base, patchCap int) ([]protocol.ChangedFile, string, bool, error) {
	gitDir, err := r.gitDir()
	if err != nil {
		return nil, "", false, err
	}
	src := diffSource{root: r.root, gitDir: gitDir, skip: ".jj", seed: func(ctx context.Context, env []string) (string, error) {
		return r.seedTree(ctx, env, base.Rev.ID)
	}}
	files, patch, truncated, err := changeSet(ctx, src, "", "", patchCap)
	if err != nil {
		return nil, "", false, err
	}
	conflicts, err := r.conflicts(ctx)
	if err != nil {
		return nil, "", false, err
	}
	for _, c := range conflicts {
		for i := range files {
			if files[i].Path == c {
				files[i].Status = "conflicted"
			}
		}
	}
	return files, patch, truncated, nil
}
