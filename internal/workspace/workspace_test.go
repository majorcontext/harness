package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	return repoAt(t, t.TempDir())
}

func repoAt(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
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

var big = strings.Repeat("x\n", 3*1024*1024/2)

var changeRows = []struct {
	name  string
	scope string
	// at names the repository dir; dir is the dir that Changes gets, and
	// relative passes root relative to the working directory.
	at, dir  string
	relative bool
	setup    func(t *testing.T, dir string)
	files    string
	patch    []string
	check    func(t *testing.T, dir string, c protocol.WorkspaceChanges)
}{
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
	{name: "repository filters, hooks, and external diffs never run", scope: protocol.ScopeUncommitted,
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
			git(t, dir, "config", "diff.external", "touch "+filepath.Join(dir, "ran")+" #")
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
	{name: "a large file removed from the index is listed once", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			write(t, dir, "big.txt", big)
			git(t, dir, "add", "big.txt")
			git(t, dir, "commit", "-q", "-m", "big")
			git(t, dir, "rm", "-q", "--cached", "big.txt")
		},
		files: "big.txt deleted +0 -1572864"},
	{name: "a rename from a large old path is listed once", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			full := make([]byte, 2*1024*1024+100_000)
			_, _ = rand.NewChaCha8([32]byte{1}).Read(full)
			write(t, dir, "p.bin", string(full))
			git(t, dir, "add", "p.bin")
			git(t, dir, "commit", "-q", "-m", "p")
			git(t, dir, "rm", "-q", "--cached", "p.bin")
			write(t, dir, "q.bin", string(full[:2*1024*1024-50_000]))
		},
		files: "q.bin renamed +0 -0 from p.bin binary"},
	{name: "a missing index starts from HEAD", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			write(t, dir, "big.txt", big)
			git(t, dir, "add", "big.txt")
			git(t, dir, "commit", "-q", "-m", "big")
			write(t, dir, "seed.txt", "seed\nmore\n")
			if err := os.Remove(filepath.Join(dir, ".git", "index")); err != nil {
				t.Fatal(err)
			}
		},
		files: "seed.txt modified +1 -0"},
	{name: "a stale origin/HEAD falls through to origin/main", scope: protocol.ScopeBranch,
		setup: func(t *testing.T, dir string) {
			git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")
		},
		files: "",
		check: func(t *testing.T, _ string, c protocol.WorkspaceChanges) {
			if c.Base == nil || c.Base.Ref != "origin/main" {
				t.Errorf("base = %v, want origin/main", c.Base)
			}
		}},
	{name: "an unmerged file is listed", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			git(t, dir, "checkout", "-q", "-b", "side")
			write(t, dir, "seed.txt", "side\n")
			git(t, dir, "commit", "-q", "-am", "side")
			git(t, dir, "checkout", "-q", "main")
			write(t, dir, "seed.txt", "main\n")
			git(t, dir, "commit", "-q", "-am", "main")
			cmd := exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@example.com", "merge", "side")
			cmd.Dir = dir
			_ = cmd.Run()
		},
		files: "seed.txt modified +4 -0"},
	{name: "a split index gains no shared index", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			git(t, dir, "config", "core.splitIndex", "true")
			git(t, dir, "update-index", "--split-index")
			write(t, dir, "seed.txt", "seed\nmore\n")
			write(t, dir, "new.txt", "n\n")
		},
		files: "new.txt added +1 -0; seed.txt modified +1 -0",
		check: func(t *testing.T, dir string, _ protocol.WorkspaceChanges) {
			if shared, _ := filepath.Glob(filepath.Join(dir, ".git", "sharedindex.*")); len(shared) != 1 {
				t.Errorf("shared indexes = %q, want only the one that update-index wrote", shared)
			}
		}},
	{name: "a split index setting gains no shared index", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			past := time.Now().Add(-time.Hour)
			for _, f := range []string{"seed.txt", "gone.txt"} {
				if err := os.Chtimes(filepath.Join(dir, f), past, past); err != nil {
					t.Fatal(err)
				}
			}
			git(t, dir, "update-index", "-q", "--refresh")
			git(t, dir, "config", "core.splitIndex", "true")
			write(t, dir, "new.txt", "n\n")
		},
		files: "new.txt added +1 -0",
		check: func(t *testing.T, dir string, _ protocol.WorkspaceChanges) {
			if shared, _ := filepath.Glob(filepath.Join(dir, ".git", "sharedindex.*")); len(shared) != 0 {
				t.Errorf("shared indexes = %q, want none", shared)
			}
		}},
	{name: "a nested repository and ignored files are not listed", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			write(t, dir, ".gitignore", "*.log\n")
			git(t, dir, "add", ".gitignore")
			git(t, dir, "commit", "-q", "-m", "ignore")
			git(t, dir, "init", "-q", filepath.Join(dir, "x", "nested"))
			write(t, dir, "x/run.py", "r\n")
			write(t, dir, "x/out.log", "l\n")
		},
		files: "x/run.py added +1 -0"},
	{name: "a dirty submodule is not walked", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			git(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", repo(t), "sub")
			git(t, dir, "commit", "-q", "-m", "sub")
			write(t, dir, "sub/seed.txt", "seed\ndirty\n")
		},
		files: ""},
	{name: "objects resolve through the alternates of a shared clone", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) {
			alt := filepath.Join(t.TempDir(), "objects")
			if err := os.Rename(filepath.Join(dir, ".git", "objects"), alt); err != nil {
				t.Fatal(err)
			}
			write(t, dir, ".git/objects/info/alternates", alt+"\n")
			write(t, dir, "seed.txt", "seed\nmore\n")
		},
		files: "seed.txt modified +1 -0"},
	{name: "a colon in the repository path keeps its objects", scope: protocol.ScopeUncommitted, at: "my:repo",
		setup: func(t *testing.T, dir string) { write(t, dir, "new.txt", "n\n") },
		files: "new.txt added +1 -0"},
	{name: "a trailing space in the repository root is kept", scope: protocol.ScopeUncommitted, at: "repo ",
		setup: func(t *testing.T, dir string) { write(t, dir, "new.txt", "n\n") },
		files: "new.txt added +1 -0"},
	{name: "a subdirectory dir covers the whole repository", scope: protocol.ScopeUncommitted, dir: "sub",
		setup: func(t *testing.T, dir string) {
			write(t, dir, "sub/f.txt", "f\n")
			git(t, dir, "add", ".")
			git(t, dir, "commit", "-q", "-m", "sub")
			write(t, dir, "sub/f.txt", "f\nmore\n")
			write(t, dir, "root.txt", "root\n")
		},
		files: "root.txt added +1 -0; sub/f.txt modified +1 -0",
		patch: []string{"+more", "+root"}},
	{name: "a relative root takes a relative dir", scope: protocol.ScopeUncommitted, dir: "sub", relative: true,
		setup: func(t *testing.T, dir string) { write(t, dir, "sub/f.txt", "f\n") },
		files: "sub/f.txt added +1 -0"},
	{name: "a detached HEAD has no branch", scope: protocol.ScopeUncommitted,
		setup: func(t *testing.T, dir string) { git(t, dir, "checkout", "-q", "--detach") },
		files: "",
		check: func(t *testing.T, dir string, c protocol.WorkspaceChanges) {
			if c.Branch != "" || c.Head != git(t, dir, "rev-parse", "HEAD") {
				t.Errorf("changes = branch %q head %q, want no branch at HEAD", c.Branch, c.Head)
			}
		}},
}

func TestChanges(t *testing.T) {
	for _, row := range changeRows {
		t.Run(row.name, func(t *testing.T) {
			dir := repo(t)
			if row.at != "" {
				dir = repoAt(t, filepath.Join(t.TempDir(), row.at))
			}
			row.setup(t, dir)
			root, want := dir, dir
			if row.relative {
				t.Chdir(filepath.Dir(dir))
				root = filepath.Base(dir)
			}
			if row.dir != "" {
				want = filepath.Join(dir, row.dir)
			}
			index := fileStamp(t, filepath.Join(dir, ".git", "index"))
			c, err := workspace.Changes(context.Background(), root, row.dir, row.scope)
			if err != nil {
				t.Fatal(err)
			}
			if got := fileStamp(t, filepath.Join(dir, ".git", "index")); got != index {
				t.Errorf("index = %s, want unchanged %s", got, index)
			}
			if c.Files == nil {
				t.Error("files = nil, want an empty list")
			}
			if got := files(c); got != row.files || c.Scope != row.scope || c.Dir != want {
				t.Errorf("changes = %q in %s scope %s, want %q in %s", got, c.Dir, c.Scope, row.files, want)
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
	if errors.Is(err, os.ErrNotExist) {
		return "missing"
	}
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(info.Size(), info.ModTime().UnixNano())
}

func TestChangesRefusals(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	unrelated := repoAt(t, filepath.Join(root, "unrelated"))
	git(t, unrelated, "checkout", "-q", "--orphan", "other")
	git(t, unrelated, "commit", "-q", "--allow-empty", "-m", "other")
	git(t, unrelated, "update-ref", "refs/remotes/origin/main", "HEAD")
	git(t, unrelated, "checkout", "-q", "main")
	outer := repo(t)
	inner := filepath.Join(outer, "har:ness", "root")
	write(t, inner, "x.txt", "x\n")
	filters := repo(t)
	f, err := os.OpenFile(filepath.Join(filters, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5000 {
		if _, err := fmt.Fprintf(f, "[filter \"d%05d\"]\n\tclean = cat\n", i); err != nil {
			t.Fatal(err)
		}
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
		{"symlink out of the root", root, "escape", "", nil, workspace.ErrInvalid, `invalid request: dir "` + root + `/escape" escapes every allowed workspace root`},
		{"a colon in the root does not reach an outer repository", inner, inner, protocol.ScopeUncommitted, nil, workspace.ErrNotRepo, `not_a_git_repo: "` + inner + `" is not a git work tree`},
		{"branch scope with no common ancestor", root, "unrelated", "", nil, workspace.ErrNoBase, "no_base: HEAD and origin/main share no common ancestor"},
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
