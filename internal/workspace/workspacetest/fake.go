package workspacetest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/workspace"
	"github.com/majorcontext/harness/protocol"
)

const stateFile = ".fake-workspace.json"

// Fake is an engine over a plain directory. Its revisions are
// "change:<n>", never a hash. The operation counter n rises when the files
// have changed since the last read, and a read of unchanged files adds none.
// It has no storage other than one state file in the root.
type Fake struct{}

type state struct {
	Ops      int
	Digest   string
	Unborn   bool
	NoBase   bool
	Baseline map[string]string
}

// FakeOptions shapes the work tree that Init makes.
type FakeOptions struct {
	// Unborn makes a head with no commit and an empty baseline.
	Unborn bool
	// NoBase makes a work tree whose base is missing.
	NoBase bool
}

// Init makes dir a Fake work tree whose baseline is the files that dir holds.
func Init(dir string, o FakeOptions) error {
	files, err := readFiles(dir)
	if err != nil {
		return err
	}
	st := state{Ops: 1, Digest: digest(files), Unborn: o.Unborn, NoBase: o.NoBase, Baseline: files}
	if o.Unborn {
		st.Baseline = map[string]string{}
	}
	return save(dir, st)
}

// Operations returns the operation counter of the Fake work tree at dir.
func Operations(dir string) (int, error) {
	st, err := load(dir)
	return st.Ops, err
}

// Name returns "fake".
func (Fake) Name() string { return "fake" }

// Open finds the nearest directory from dir up to, and not including,
// ceiling that holds the state file.
func (Fake) Open(_ context.Context, dir, ceiling string) (workspace.Repo, bool, error) {
	for cur := dir; cur != ceiling; cur = filepath.Dir(cur) {
		if _, err := os.Stat(filepath.Join(cur, stateFile)); err == nil {
			return repo{root: cur}, true, nil
		}
		if filepath.Dir(cur) == cur {
			break
		}
	}
	return nil, false, nil
}

type repo struct{ root string }

func (r repo) Root() string { return r.root }

func (r repo) Head(context.Context) (workspace.Head, error) {
	st, err := load(r.root)
	if err != nil {
		return workspace.Head{}, err
	}
	files, err := readFiles(r.root)
	if err != nil {
		return workspace.Head{}, err
	}
	if d := digest(files); d != st.Digest {
		st.Ops++
		st.Digest = d
		if err := save(r.root, st); err != nil {
			return workspace.Head{}, err
		}
	}
	if st.Unborn {
		return workspace.Head{}, nil
	}
	return workspace.Head{Rev: change(st.Ops)}, nil
}

func (r repo) Base(context.Context, workspace.Head) (workspace.Base, error) {
	st, err := load(r.root)
	if err != nil {
		return workspace.Base{}, err
	}
	if st.NoBase || st.Unborn {
		return workspace.Base{}, fmt.Errorf("%w: the fake work tree has no base", workspace.ErrNoBase)
	}
	return workspace.Base{Rev: change(0), Ref: "root"}, nil
}

func (r repo) Diff(_ context.Context, _ workspace.Head, _ workspace.Base, _ int) ([]protocol.ChangedFile, string, bool, error) {
	st, err := load(r.root)
	if err != nil {
		return nil, "", false, err
	}
	now, err := readFiles(r.root)
	if err != nil {
		return nil, "", false, err
	}
	files := []protocol.ChangedFile{}
	var patch strings.Builder
	for _, p := range sortedKeys(now, st.Baseline) {
		old, had := st.Baseline[p]
		cur, has := now[p]
		var f protocol.ChangedFile
		switch {
		case had && has && old == cur:
			continue
		case !had:
			f = protocol.ChangedFile{Path: p, Status: "added", Additions: lines(cur)}
		case !has:
			f = protocol.ChangedFile{Path: p, Status: "deleted", Deletions: lines(old)}
		default:
			f = protocol.ChangedFile{Path: p, Status: "modified", Additions: lines(cur), Deletions: lines(old)}
		}
		files = append(files, f)
		_, _ = fmt.Fprintf(&patch, "diff --fake %s\n", p)
	}
	return files, patch.String(), false, nil
}

func change(n int) protocol.Rev {
	return protocol.Rev{Kind: protocol.RevChange, ID: fmt.Sprint(n)}
}

func lines(s string) int { return strings.Count(s, "\n") }

func sortedKeys(a, b map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]string{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	slices.Sort(out)
	return out
}

func digest(files map[string]string) string {
	h := sha256.New()
	for _, p := range sortedKeys(files, nil) {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00", p, files[p])
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func readFiles(root string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == stateFile {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		files[filepath.ToSlash(rel)] = string(data)
		return err
	})
	return files, err
}

func load(root string) (state, error) {
	var st state
	data, err := os.ReadFile(filepath.Join(root, stateFile))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(data, &st)
}

func save(root string, st state) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, stateFile), data, 0o644)
}
