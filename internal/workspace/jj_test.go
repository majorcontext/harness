package workspace_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/internal/workspace"
	"github.com/majorcontext/harness/internal/workspace/workspacetest"
	"github.com/majorcontext/harness/protocol"
)

func TestJJEngineContract(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("jj is not on PATH, and CI needs it")
		}
		t.Skip("jj is not on PATH")
	}
	workspacetest.Run(t, jjFixture)
}

// TestChangesCommandHelper is the changes command of the tests below, which
// run the test binary with "-- ok" or "-- fail". Without the arguments it
// does nothing.
func TestChangesCommandHelper(t *testing.T) {
	args := os.Args
	if len(args) < 2 || args[len(args)-2] != "--" {
		return
	}
	switch args[len(args)-1] {
	case "fail":
		os.Exit(3)
	case "hold":
		// A grandchild keeps stdout open after this command exits.
		hold := exec.Command("sleep", "30")
		hold.Stdout = os.Stdout
		if err := hold.Start(); err != nil {
			os.Exit(4)
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(protocol.WorkspaceChanges{
		Files: []protocol.ChangedFile{{Path: "from-command.txt", Status: "added", Additions: 1}},
		Patch: strings.Join([]string{os.Getenv("HARNESS_CHANGES_SCOPE"), os.Getenv("HARNESS_CHANGES_BASE"), os.Getenv("HARNESS_CHANGES_PATCH_BYTES")}, " "),
	})
	os.Exit(0)
}

func jjChangesCommandRepo(t *testing.T, mode string) string {
	dir := jjRepo(t, true)
	argv, err := json.Marshal([]string{os.Args[0], "-test.run=^TestChangesCommandHelper$", "--", mode})
	if err != nil {
		t.Fatal(err)
	}
	jj(t, dir, "config", "set", "--repo", "harness.changes-command", string(argv))
	return dir
}

func TestJJChangesCommand(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("jj is not on PATH, and CI needs it")
		}
		t.Skip("jj is not on PATH")
	}
	engines := []workspace.Engine{workspace.JJEngine{}}
	t.Run("the answer is the output of the command", func(t *testing.T) {
		t.Parallel()
		dir := jjChangesCommandRepo(t, "ok")
		write(t, dir, "seed.txt", "changed\n")
		parent := jj(t, dir, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
		for scope, want := range map[string]string{
			protocol.ScopeUncommitted: "uncommitted " + parent + " 1048576",
			protocol.ScopeBranch:      "branch " + parent + " 1048576",
		} {
			c, err := workspace.ChangesWith(context.Background(), engines, dir, "", scope)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Files) != 1 || c.Files[0].Path != "from-command.txt" || c.Patch != want {
				t.Errorf("%s: files = %+v, patch = %q, want one file from the command and patch %q", scope, c.Files, c.Patch, want)
			}
		}
	})
	t.Run("a grandchild that holds stdout does not hold the request", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		c, err := workspace.ChangesWith(ctx, engines, jjChangesCommandRepo(t, "hold"), "", protocol.ScopeUncommitted)
		if err != nil || len(c.Files) != 1 {
			t.Errorf("files = %+v, error = %v, want the answer of the command", c.Files, err)
		}
		if ctx.Err() != nil {
			t.Error("the request waited for the grandchild")
		}
	})
	t.Run("a legacy repository config fails the request before any jj command", func(t *testing.T) {
		t.Parallel()
		dir := jjRepo(t, true)
		write(t, dir, ".jj/repo/config.toml", "")
		if _, err := workspace.ChangesWith(context.Background(), engines, dir, "", protocol.ScopeUncommitted); err == nil || errors.Is(err, workspace.ErrNotRepo) {
			t.Errorf("error = %v, want an internal failure", err)
		}
	})
	t.Run("a failing command fails the request", func(t *testing.T) {
		t.Parallel()
		_, err := workspace.ChangesWith(context.Background(), engines, jjChangesCommandRepo(t, "fail"), "", protocol.ScopeUncommitted)
		if err == nil || errors.Is(err, workspace.ErrNotRepo) || errors.Is(err, workspace.ErrNoBase) || errors.Is(err, workspace.ErrInvalid) {
			t.Errorf("error = %v, want an internal failure", err)
		}
	})
}

func jj(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("jj", append([]string{"--no-pager", "--color=never", "--quiet",
		"--config=user.name=t", "--config=user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jj %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// jjRepo makes a jj work tree whose head is main, with main@origin at the
// same commit when origin is true.
func jjRepo(t *testing.T, origin bool, initArgs ...string) string {
	t.Helper()
	dir := t.TempDir()
	jj(t, dir, append([]string{"git", "init"}, initArgs...)...)
	write(t, dir, "seed.txt", "seed\n")
	jj(t, dir, "commit", "-m", "init")
	jj(t, dir, "bookmark", "create", "main", "-r", "@-")
	if origin {
		remote := t.TempDir()
		git(t, remote, "init", "-q", "--bare")
		jj(t, dir, "git", "remote", "add", "origin", remote)
		jj(t, dir, "git", "push", "--bookmark", "main")
	}
	return dir
}

// jjFootprint hashes every file of the jj and git storage of dir.
func jjFootprint(t *testing.T, dir string) string {
	var out []string
	for _, sub := range []string{".jj", ".git"} {
		err := filepath.WalkDir(filepath.Join(dir, sub), func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				data, rerr := os.ReadFile(p)
				out = append(out, fmt.Sprintf("%s %x %v", p, sha256.Sum256(data), rerr))
			}
			if os.IsNotExist(err) {
				return nil
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return strings.Join(out, "\n")
}

func jjFixture(t *testing.T) workspacetest.Fixture {
	return workspacetest.Fixture{
		Engine: workspace.JJEngine{},
		New:    func(t *testing.T) string { return jjRepo(t, true) },
		Unborn: func(t *testing.T) string {
			dir := t.TempDir()
			jj(t, dir, "git", "init")
			return dir
		},
		Merge: func(t *testing.T) string {
			dir := jjRepo(t, true, "--no-colocate")
			jj(t, dir, "new", "main", "-m", "a")
			jj(t, dir, "new", "main", "-m", "b")
			jj(t, dir, "new", `description(exact:"a\n")`, `description(exact:"b\n")`)
			return dir
		},
		NoBase:  func(t *testing.T) string { return jjRepo(t, false) },
		Outside: func(t *testing.T) string { return t.TempDir() },
		Operations: func(t *testing.T, dir string) int {
			return len(strings.Fields(jj(t, dir, "--ignore-working-copy", "op", "log", "--no-graph", "-T", `id ++ "\n"`)))
		},
		Footprint: jjFootprint,
		Conflict: func(t *testing.T) (string, string) {
			dir := jjRepo(t, true)
			jj(t, dir, "new", "main", "-m", "x")
			write(t, dir, "seed.txt", "x\n")
			jj(t, dir, "new", "main", "-m", "y")
			write(t, dir, "seed.txt", "y\n")
			jj(t, dir, "rebase", "-r", "@", "-d", `description(exact:"x\n")`)
			return dir, "seed.txt"
		},
	}
}
