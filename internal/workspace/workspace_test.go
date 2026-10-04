package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/internal/workspace"
	"github.com/majorcontext/harness/protocol"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repo commits seed.txt and gone.txt on main in a new directory, and
// records origin/main at that commit.
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "maintenance.auto", "false")
	write(t, dir, "seed.txt", "seed\n")
	write(t, dir, "gone.txt", "bye\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "init")
	git(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	return dir
}

func files(c protocol.WorkspaceChanges) string {
	var out []string
	for _, f := range c.Files {
		s := fmt.Sprintf("%s %s +%d -%d", f.Path, f.Status, f.Additions, f.Deletions)
		if f.OldPath != "" {
			s += " from " + f.OldPath
		}
		if f.Binary {
			s += " binary"
		}
		if f.Large {
			s += " large"
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

var changeRows = []struct {
	name  string
	scope string
	setup func(t *testing.T, dir string)
	files string
	patch []string
	check func(t *testing.T, dir string, c protocol.WorkspaceChanges)
}{
	{name: "uncommitted reports modified, deleted, and untracked files", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			write(t, dir, "seed.txt", "SEED!\n")
			write(t, dir, "new.txt", "n\n")
			if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
				t.Fatal(err)
			}
		},
		files: "gone.txt deleted +0 -1; new.txt added +1 -0; seed.txt modified +1 -1",
		patch: []string{"-seed\n+SEED!\n", "deleted file mode", "diff --git a/new.txt b/new.txt"},
		check: func(t *testing.T, dir string, c protocol.WorkspaceChanges) {
			if c.Branch != "main" || c.Head != git(t, dir, "rev-parse", "HEAD") || c.Base != nil || c.Truncated {
				t.Errorf("changes = branch %q head %q base %v truncated %v, want main at HEAD with no base", c.Branch, c.Head, c.Base, c.Truncated)
			}
			if st := git(t, dir, "status", "--porcelain"); !strings.Contains(st, "?? new.txt") {
				t.Errorf("status = %q, want new.txt still untracked", st)
			}
		}},
	{name: "branch diffs the work tree against the merge base", scope: protocol.ScopeBranch,
		setup: func(t *testing.T, dir string) {
			git(t, dir, "checkout", "-q", "-b", "feat")
			write(t, dir, "seed.txt", "SEED!\n")
			git(t, dir, "commit", "-q", "-am", "feat")
			write(t, dir, "new.txt", "n\n")
		},
		files: "new.txt added +1 -0; seed.txt modified +1 -1",
		check: func(t *testing.T, dir string, c protocol.WorkspaceChanges) {
			base := git(t, dir, "rev-parse", "origin/main")
			if c.Branch != "feat" || c.Base == nil || *c.Base != (protocol.BaseRef{Ref: "origin/main", SHA: base}) {
				t.Errorf("changes = branch %q base %v, want feat on origin/main at %s", c.Branch, c.Base, base)
			}
		}},
	{name: "a rename keeps the old path", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) { git(t, dir, "mv", "seed.txt", "moved.txt") },
		files: "moved.txt renamed +0 -0 from seed.txt"},
	{name: "a binary file has no counts", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) { write(t, dir, "b.bin", "\x00\x01\x02") },
		files: "b.bin added +0 -0 binary"},
	{name: "a large untracked file has no hunk", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			write(t, dir, "big.txt", strings.Repeat("x\n", 3*1024*1024/2))
			write(t, dir, "small.txt", "y\n")
		},
		files: "small.txt added +1 -0; big.txt added +0 -0 large",
		check: func(t *testing.T, _ string, c protocol.WorkspaceChanges) {
			if strings.Contains(c.Patch, "big.txt") {
				t.Errorf("patch has a hunk of the large file")
			}
		}},
	{name: "a glob-like file name is literal", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) { write(t, dir, "b*.txt", "g\n"); write(t, dir, "bx.txt", "x\n") },
		files: "b*.txt added +1 -0; bx.txt added +1 -0"},
	{name: "a patch over its cap ends at a whole file", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			write(t, dir, "a.txt", strings.Repeat("a\n", 300_000))
			write(t, dir, "b.txt", strings.Repeat("b\n", 300_000))
		},
		files: "a.txt added +300000 -0; b.txt added +300000 -0",
		check: func(t *testing.T, _ string, c protocol.WorkspaceChanges) {
			if !c.Truncated || !strings.HasPrefix(c.Patch, "diff --git a/a.txt") || strings.Contains(c.Patch, "b.txt") || !strings.HasSuffix(c.Patch, "+a\n") {
				t.Errorf("truncated %v, patch of %d bytes: want only a.txt, whole", c.Truncated, len(c.Patch))
			}
		}},
	{name: "repository filters and hooks never run", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			write(t, dir, ".gitattributes", "seed.txt filter=x=y\n")
			write(t, dir, ".githooks/post-index-change", "#!/bin/sh\ntouch "+filepath.Join(dir, "ran")+"\n")
			if err := os.Chmod(filepath.Join(dir, ".githooks/post-index-change"), 0o755); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "add", ".")
			git(t, dir, "commit", "-q", "-m", "attach")
			for _, k := range []string{"clean", "process"} {
				git(t, dir, "config", "filter.x=y."+k, "touch "+filepath.Join(dir, "ran")+" #")
			}
			git(t, dir, "config", "filter.x=y.required", "true")
			git(t, dir, "config", "core.hooksPath", ".githooks")
			write(t, dir, "seed.txt", "seed\nmore\n")
			write(t, dir, "new.txt", "n\n")
		},
		files: "new.txt added +1 -0; seed.txt modified +1 -0",
		patch: []string{"+more"},
		check: func(t *testing.T, dir string, _ protocol.WorkspaceChanges) {
			if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
				t.Error("a filter or hook of the repository ran")
			}
		}},
	{name: "an inherited GIT_DIR selects no other repository", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			outer := repo(t)
			write(t, outer, "outer.txt", "o\n")
			git(t, outer, "add", "outer.txt")
			t.Setenv("GIT_DIR", filepath.Join(outer, ".git"))
			t.Setenv("GIT_INDEX_FILE", filepath.Join(outer, ".git", "index"))
			t.Setenv("GIT_LITERAL_PATHSPECS", "1")
		},
		files: ""},
}

func TestChanges(t *testing.T) {
	for _, row := range changeRows {
		t.Run(row.name, func(t *testing.T) {
			dir := repo(t)
			row.setup(t, dir)
			index := fileStamp(t, filepath.Join(dir, ".git", "index"))
			c, err := workspace.Changes(context.Background(), dir, "", row.scope)
			if err != nil {
				t.Fatal(err)
			}
			if got := fileStamp(t, filepath.Join(dir, ".git", "index")); got != index {
				t.Errorf("index = %s, want unchanged %s", got, index)
			}
			if got := files(c); got != row.files || c.Scope != row.scope || c.Dir != dir {
				t.Errorf("changes = %q in %s scope %s, want %q in %s", got, c.Dir, c.Scope, row.files, dir)
			}
			for _, want := range row.patch {
				if !strings.Contains(c.Patch, want) {
					t.Errorf("patch lacks %q:\n%s", want, c.Patch)
				}
			}
			if row.check != nil {
				row.check(t, dir, c)
			}
		})
	}
}

func fileStamp(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(info.Size(), info.ModTime().UnixNano())
}

func TestChangesRefusals(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "plain")
	unborn := filepath.Join(root, "unborn")
	nobase := filepath.Join(root, "nobase")
	for _, d := range []string{plain, unborn, nobase} {
		write(t, d, "x.txt", "x\n")
	}
	git(t, unborn, "init", "-q", "-b", "main")
	git(t, nobase, "init", "-q", "-b", "main")
	git(t, nobase, "add", ".")
	git(t, nobase, "commit", "-q", "-m", "only")
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	outer := repo(t)
	inner := filepath.Join(outer, "har:ness", "root")
	write(t, inner, "x.txt", "x\n")
	filters := repo(t)
	f, err := os.OpenFile(filepath.Join(filters, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5000 {
		fmt.Fprintf(f, "[filter \"d%05d\"]\n\tclean = cat\n", i)
	}
	_ = f.Close()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	rows := []struct {
		name, root, dir, scope string
		ctx                    context.Context
		err                    error
		msg                    string
	}{
		{"unknown scope", root, "", "bogus", nil, workspace.ErrInvalid, `invalid request: scope "bogus" must be "branch" or "uncommitted"`},
		{"dir outside the root", root, "/nonexistent", "", nil, workspace.ErrInvalid, `invalid request: workdir "/nonexistent" is not under an allowed workspace root`},
		{"missing dir", root, "missing", "", nil, workspace.ErrInvalid, `invalid request: dir "` + root + `/missing": stat ` + root + `/missing: no such file or directory`},
		{"symlink out of the root", root, "escape", "", nil, workspace.ErrInvalid, `invalid request: dir "` + root + `/escape" escapes every allowed workspace root`},
		{"not a work tree", root, "plain", protocol.ScopeUncommitted, nil, workspace.ErrNotRepo, `not_a_git_repo: "` + plain + `" is not a git work tree`},
		{"a colon in the root does not reach an outer repository", inner, inner, protocol.ScopeUncommitted, nil, workspace.ErrNotRepo, `not_a_git_repo: "` + inner + `" is not a git work tree`},
		{"branch scope with no commit", root, "unborn", "", nil, workspace.ErrNoBase, "no_base: HEAD has no commit yet"},
		{"branch scope with no default branch", root, "nobase", "", nil, workspace.ErrNoBase, "no_base: no default branch found (checked origin/HEAD, origin/main, origin/master)"},
		{"an ended deadline", filters, "", protocol.ScopeUncommitted, canceled, workspace.ErrTooManyChanges, "too_many_changes: request exceeded its time budget diffing a large change set"},
		{"a flood of filter drivers", filters, "", protocol.ScopeUncommitted, nil, workspace.ErrTooManyChanges, "too_many_changes: request exceeded its time budget diffing a large change set"},
	}
	for _, row := range rows {
		ctx := row.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		_, err := workspace.Changes(ctx, row.root, row.dir, row.scope)
		if !errors.Is(err, row.err) || err.Error() != row.msg {
			t.Errorf("%s: Changes = %v, want %v: %s", row.name, err, row.err, row.msg)
		}
	}
}

func TestChangesOfAnUnbornRepository(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "first.txt", "1\n")
	c, err := workspace.Changes(context.Background(), dir, "", protocol.ScopeUncommitted)
	if err != nil || c.Head != "" || c.Branch != "main" || files(c) != "first.txt added +1 -0" {
		t.Errorf("Changes = %+v, %v; want first.txt added on unborn main", c, err)
	}
}
