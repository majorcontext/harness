// Package workspacetest holds the contract table of a workspace.Engine and
// Fake, an engine over a plain directory.
package workspacetest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/internal/workspace"
	"github.com/majorcontext/harness/protocol"
)

// Fixture is one engine and the way to make and measure its work trees.
type Fixture struct {
	Engine workspace.Engine
	// New returns a work tree with a head and a base. Its seed.txt holds
	// "seed\n" in both, and it holds no other file.
	New func(t *testing.T) string
	// Unborn returns a work tree whose head has no commit. It may be nil for
	// an engine that has no such state.
	Unborn func(t *testing.T) string
	// Merge returns a work tree whose head has several parents, with a base.
	// It may be nil for an engine that has no such state.
	Merge func(t *testing.T) string
	// NoBase returns a work tree with a head and no base.
	NoBase func(t *testing.T) string
	// Outside returns a directory that is in no work tree.
	Outside func(t *testing.T) string
	// Operations returns the number of operations that the repository of dir
	// has recorded.
	Operations func(t *testing.T, dir string) int
	// Footprint returns a text that changes when a user could see a write
	// to the repository of dir: its index, references, objects, and
	// configuration. The state that an engine keeps for its own snapshots of
	// the work tree is not a footprint.
	Footprint func(t *testing.T, dir string) string
	// Guard arms a trap in the repository of dir that fails any write to its
	// shared storage, and Footprint covers it. It may be nil.
	Guard func(t *testing.T, dir string)
	// Conflict returns a work tree whose head records a conflict in the file
	// at the returned path. It may be nil for an engine that has no such
	// state.
	Conflict func(t *testing.T) (dir, path string)
}

// Factory returns the Fixture of one row. Each row calls it once.
type Factory func(t *testing.T) Fixture

var rows = []struct {
	name string
	run  func(t *testing.T, fx Fixture)
}{
	{"a read after an edit reports the edit", readAfterEdit},
	{"a read after an edit adds no operation", readsAddNoOperation},
	{"an untracked new file is reported", untrackedReported},
	{"a deleted file is reported", deletedReported},
	{"a rename is reported as a rename or as a delete and an add", renameReported},
	{"a directory outside any repository answers not_a_repo", outsideIsNotARepo},
	{"an unborn head gives no head_rev and no error", unbornHead},
	{"a head of several parents gives no head_rev and the edit alone", mergeHead},
	{"the patch is a git-format diff of the reported files", patchFormat},
	{"a missing base gives no_base", missingBase},
	{"head_commit is set exactly when head_rev is", headCommit},
	{"a file that the head records as a conflict is conflicted", conflictedReported},
	{"reads never write", readsNeverWrite},
}

// Run runs every row of the contract on the engine of f.
func Run(t *testing.T, f Factory) {
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t, f(t))
		})
	}
}

func read(t *testing.T, fx Fixture, dir, scope string) (protocol.WorkspaceChanges, error) {
	t.Helper()
	return workspace.ChangesWith(context.Background(), []workspace.Engine{fx.Engine}, dir, "", scope)
}

func mustRead(t *testing.T, fx Fixture, dir, scope string) protocol.WorkspaceChanges {
	t.Helper()
	c, err := read(t, fx, dir, scope)
	if err != nil {
		t.Fatalf("read of %s scope: %v", scope, err)
	}
	return c
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func summary(c protocol.WorkspaceChanges) []string {
	var out []string
	for _, f := range c.Files {
		out = append(out, f.Path+" "+f.Status)
	}
	slices.Sort(out)
	return out
}

func wantFiles(t *testing.T, c protocol.WorkspaceChanges, want ...string) {
	t.Helper()
	if got := summary(c); !slices.Equal(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
}

func readAfterEdit(t *testing.T, fx Fixture) {
	dir := fx.New(t)
	wantFiles(t, mustRead(t, fx, dir, protocol.ScopeUncommitted))
	write(t, dir, "seed.txt", "changed\n")
	wantFiles(t, mustRead(t, fx, dir, protocol.ScopeUncommitted), "seed.txt modified")
}

func readsAddNoOperation(t *testing.T, fx Fixture) {
	dir := fx.New(t)
	before := fx.Operations(t, dir)
	write(t, dir, "seed.txt", "changed\n")
	for _, scope := range []string{protocol.ScopeUncommitted, protocol.ScopeBranch, protocol.ScopeUncommitted} {
		mustRead(t, fx, dir, scope)
		if got := fx.Operations(t, dir); got != before {
			t.Errorf("operations after a %s read = %d, want %d", scope, got, before)
		}
	}
}

func untrackedReported(t *testing.T, fx Fixture) {
	dir := fx.New(t)
	write(t, dir, "new.txt", "new\n")
	wantFiles(t, mustRead(t, fx, dir, protocol.ScopeUncommitted), "new.txt added")
}

func deletedReported(t *testing.T, fx Fixture) {
	dir := fx.New(t)
	if err := os.Remove(filepath.Join(dir, "seed.txt")); err != nil {
		t.Fatal(err)
	}
	wantFiles(t, mustRead(t, fx, dir, protocol.ScopeUncommitted), "seed.txt deleted")
}

func renameReported(t *testing.T, fx Fixture) {
	dir := fx.New(t)
	if err := os.Rename(filepath.Join(dir, "seed.txt"), filepath.Join(dir, "moved.txt")); err != nil {
		t.Fatal(err)
	}
	c := mustRead(t, fx, dir, protocol.ScopeUncommitted)
	asRename := len(c.Files) == 1 && c.Files[0].Path == "moved.txt" && c.Files[0].Status == "renamed" && c.Files[0].OldPath == "seed.txt"
	asPair := slices.Equal(summary(c), []string{"moved.txt added", "seed.txt deleted"})
	if !asRename && !asPair {
		t.Errorf("files = %q, want a rename of seed.txt to moved.txt, or its delete and add", summary(c))
	}
}

func outsideIsNotARepo(t *testing.T, fx Fixture) {
	_, err := read(t, fx, fx.Outside(t), protocol.ScopeUncommitted)
	if !errors.Is(err, workspace.ErrNotRepo) {
		t.Errorf("error = %v, want not_a_repo", err)
	}
}

func unbornHead(t *testing.T, fx Fixture) {
	if fx.Unborn == nil {
		t.Skip("engine has no unborn head")
	}
	c := mustRead(t, fx, fx.Unborn(t), protocol.ScopeUncommitted)
	if c.HeadRev != (protocol.Rev{}) {
		t.Errorf("head_rev = %q, want none", c.HeadRev)
	}
}

func mergeHead(t *testing.T, fx Fixture) {
	if fx.Merge == nil {
		t.Skip("engine has no head of several parents")
	}
	dir := fx.Merge(t)
	write(t, dir, "seed.txt", "changed\n")
	c := mustRead(t, fx, dir, protocol.ScopeUncommitted)
	wantFiles(t, c, "seed.txt modified")
	if c.HeadRev != (protocol.Rev{}) {
		t.Errorf("head_rev = %q, want none", c.HeadRev)
	}
	b := mustRead(t, fx, dir, protocol.ScopeBranch)
	if b.HeadRev != (protocol.Rev{}) || b.Base == nil || b.Base.Rev == (protocol.Rev{}) {
		t.Errorf("branch scope: head_rev = %q, base = %+v; want no head_rev and a base revision", b.HeadRev, b.Base)
	}
}

func patchFormat(t *testing.T, fx Fixture) {
	dir := fx.New(t)
	write(t, dir, "seed.txt", "changed\n")
	write(t, dir, "new.txt", "new\n")
	c := mustRead(t, fx, dir, protocol.ScopeUncommitted)
	var got, want []string
	for _, line := range strings.Split(c.Patch, "\n") {
		if p, ok := strings.CutPrefix(line, "diff --git "); ok {
			got = append(got, p)
		}
	}
	for _, f := range c.Files {
		want = append(want, "a/"+f.Path+" b/"+f.Path)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("patch headers = %q, want %q", got, want)
	}
}

func missingBase(t *testing.T, fx Fixture) {
	_, err := read(t, fx, fx.NoBase(t), protocol.ScopeBranch)
	if !errors.Is(err, workspace.ErrNoBase) {
		t.Errorf("error = %v, want no_base", err)
	}
}

func readsNeverWrite(t *testing.T, fx Fixture) {
	t.Run("with no lock held", func(t *testing.T) { readsNeverWriteWith(t, fx, false) })
	if fx.Guard != nil {
		t.Run("with the lock held", func(t *testing.T) { readsNeverWriteWith(t, fx, true) })
	}
}

func readsNeverWriteWith(t *testing.T, fx Fixture, guard bool) {
	dir := fx.New(t)
	if guard {
		fx.Guard(t, dir)
	}
	write(t, dir, "seed.txt", "changed\n")
	write(t, dir, "new.txt", "new\n")
	before := fx.Footprint(t, dir)
	for _, scope := range []string{protocol.ScopeUncommitted, protocol.ScopeBranch} {
		wantFiles(t, mustRead(t, fx, dir, scope), "new.txt added", "seed.txt modified")
		if after := fx.Footprint(t, dir); after != before {
			t.Errorf("the %s read wrote to the repository:\n%s\nwant\n%s", scope, after, before)
		}
	}
}

func headCommit(t *testing.T, fx Fixture) {
	check := func(name string, dir string) {
		c := mustRead(t, fx, dir, protocol.ScopeUncommitted)
		if has := c.HeadRev != (protocol.Rev{}); has != (c.HeadCommit != "") {
			t.Errorf("%s: head_rev = %q, head_commit = %q; want both set or both empty", name, c.HeadRev, c.HeadCommit)
		}
	}
	dir := fx.New(t)
	check("head", dir)
	if fx.Unborn != nil {
		check("unborn head", fx.Unborn(t))
	}
	if fx.Merge != nil {
		check("head of several parents", fx.Merge(t))
	}
	if c := mustRead(t, fx, dir, protocol.ScopeUncommitted); c.HeadCommit == "" {
		t.Error("head_commit is empty for a head with a commit")
	}
}

func conflictedReported(t *testing.T, fx Fixture) {
	if fx.Conflict == nil {
		t.Skip("engine has no conflict state")
	}
	dir, path := fx.Conflict(t)
	for _, f := range mustRead(t, fx, dir, protocol.ScopeUncommitted).Files {
		if f.Path == path {
			if f.Status != "conflicted" {
				t.Errorf("status of %s = %q, want conflicted", path, f.Status)
			}
			return
		}
	}
	t.Errorf("files do not list %s", path)
}
