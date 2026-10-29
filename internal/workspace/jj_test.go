package workspace_test

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/internal/workspace"
	"github.com/majorcontext/harness/internal/workspace/workspacetest"
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
