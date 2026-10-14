// Package workspacetest holds the contract table of a workspace.Engine and
// Fake, an engine over a plain directory.
package workspacetest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	// Unborn returns a work tree whose head has no commit.
	Unborn func(t *testing.T) string
	// NoBase returns a work tree with a head and no base.
	NoBase func(t *testing.T) string
	// Outside returns a directory that is in no work tree.
	Outside func(t *testing.T) string
	// Operations returns the number of operations that the repository of dir
	// has recorded.
	Operations func(t *testing.T, dir string) int
	// Footprint returns a text that changes when the storage of the
	// repository of dir changes, such as its index and its objects.
	Footprint func(t *testing.T, dir string) string
	// Guard arms a trap in the repository of dir that fails any write to its
	// shared storage, and Footprint covers it. It may be nil.
	Guard func(t *testing.T, dir string)
	// SnapshotsEdits is true when the first read after an edit may record an
	// operation, because the engine snapshots the work tree on read.
	SnapshotsEdits bool
}

// Factory returns the Fixture of one row. Each row calls it once.
type Factory func(t *testing.T) Fixture

var rows = []struct {
	name string
	run  func(t *testing.T, fx Fixture)
}{
	{"a read after an edit reports the edit", readAfterEdit},
	{"two reads with no edit between them add no operation", readsAddNoOperation},
	{"an untracked new file is reported", untrackedReported},
	{"a deleted file is reported", deletedReported},
	{"a rename is reported as a rename or as a delete and an add", renameReported},
	{"a directory outside any repository answers not_a_repo", outsideIsNotARepo},
	{"an unborn head gives no head_rev and no error", unbornHead},
	{"a missing base gives no_base", missingBase},
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
	mustRead(t, fx, dir, protocol.ScopeUncommitted)
	afterEdit := fx.Operations(t, dir)
	if fx.SnapshotsEdits && afterEdit <= before {
		t.Errorf("operations after the first read of an edit = %d, want more than %d", afterEdit, before)
	}
	mustRead(t, fx, dir, protocol.ScopeBranch)
	if got := fx.Operations(t, dir); got != afterEdit {
		t.Errorf("operations after another read = %d, want %d", got, afterEdit)
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
	c := mustRead(t, fx, fx.Unborn(t), protocol.ScopeUncommitted)
	if c.HeadRev != (protocol.Rev{}) {
		t.Errorf("head_rev = %q, want none", c.HeadRev)
	}
}

func missingBase(t *testing.T, fx Fixture) {
	_, err := read(t, fx, fx.NoBase(t), protocol.ScopeBranch)
	if !errors.Is(err, workspace.ErrNoBase) {
		t.Errorf("error = %v, want no_base", err)
	}
}

func readsNeverWrite(t *testing.T, fx Fixture) {
	dir := fx.New(t)
	if fx.Guard != nil {
		fx.Guard(t, dir)
	}
	write(t, dir, "seed.txt", "changed\n")
	write(t, dir, "new.txt", "new\n")
	if fx.SnapshotsEdits {
		mustRead(t, fx, dir, protocol.ScopeUncommitted)
	}
	before := fx.Footprint(t, dir)
	for _, scope := range []string{protocol.ScopeUncommitted, protocol.ScopeBranch} {
		wantFiles(t, mustRead(t, fx, dir, scope), "new.txt added", "seed.txt modified")
		if after := fx.Footprint(t, dir); after != before {
			t.Errorf("the %s read wrote to the repository:\n%s\nwant\n%s", scope, after, before)
		}
	}
}
