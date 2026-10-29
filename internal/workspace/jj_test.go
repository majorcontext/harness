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
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/majorcontext/harness/internal/testpoll"
	"github.com/majorcontext/harness/internal/workspace"
	"github.com/majorcontext/harness/internal/workspace/workspacetest"
	"github.com/majorcontext/harness/protocol"
)

// TestMain keeps every test off the home directory and the config of the
// user: jj keeps the config of a repository under the user config directory.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "harness-workspace-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	jjConfig := filepath.Join(home, "jj.toml")
	if err := os.WriteFile(jjConfig, nil, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for k, v := range map[string]string{"HOME": home, "XDG_CONFIG_HOME": home, "JJ_CONFIG": jjConfig,
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

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
// run the test binary with "-- ok" or "-- hold". Without the arguments it
// does nothing.
func TestChangesCommandHelper(t *testing.T) {
	args := os.Args
	if len(args) < 2 || args[len(args)-2] != "--" {
		return
	}
	mode, pidFile, _ := strings.Cut(args[len(args)-1], ":")
	switch mode {
	case "flood":
		// Write until the reader is gone, then wait to be killed.
		signal.Ignore(syscall.SIGPIPE)
		chunk := make([]byte, 1<<20)
		for {
			if _, err := os.Stdout.Write(chunk); err != nil {
				<-make(chan os.Signal)
			}
		}
	case "overflow":
		// A child floods stdout after this command has exited.
		flood := exec.Command(os.Args[0], "-test.run=^TestChangesCommandHelper$", "--", "flood")
		flood.Stdout = os.Stdout
		if err := flood.Start(); err != nil {
			os.Exit(4)
		}
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(flood.Process.Pid)), 0o600); err != nil {
			os.Exit(5)
		}
		os.Exit(0)
	}
	if mode == "hold" {
		// A grandchild keeps stdout open after this command exits. The engine
		// kills its process group.
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
	t.Run("an overflow of stdout kills a child that outlives the command", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("the engine kills the process group on unix only")
		}
		pidFile := filepath.Join(t.TempDir(), "pid")
		_, err := workspace.ChangesWith(context.Background(), engines, jjChangesCommandRepo(t, "overflow:"+pidFile), "", protocol.ScopeUncommitted)
		if !errors.Is(err, workspace.ErrTooManyChanges) {
			t.Fatalf("error = %v, want too_many_changes", err)
		}
		data, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(string(data))
		if err != nil {
			t.Fatal(err)
		}
		child, err := os.FindProcess(pid)
		if err != nil {
			t.Fatal(err)
		}
		testpoll.Until(t, 10*time.Second, "the child of the command to die", func() bool { return child.Signal(syscall.Signal(0)) != nil })
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

// jjFootprint hashes every file of the jj and git storage of dir, but the
// caches that any jj command writes while it loads the repository: its commit
// index and the refs/jj/keep refs of its git store.
func jjFootprint(t *testing.T, dir string) string {
	var out []string
	for _, sub := range []string{".jj", ".git"} {
		err := filepath.WalkDir(filepath.Join(dir, sub), func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() && !jjCache(filepath.ToSlash(p)) {
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
	return strings.Join(out, "\n") + "\n" + workspacetest.TreeFootprint(t, dir, ".jj", ".git")
}

func jjCache(path string) bool {
	return strings.Contains(path, "/.jj/repo/index/") || strings.Contains(path, "/refs/jj/keep/")
}

// conflictedHead makes a work tree whose working copy commit y is
// conflicted, with a conflicted executable file and a clean one.
func conflictedHead(t *testing.T) string {
	dir := jjRepo(t, true)
	for _, side := range []string{"x", "y"} {
		jj(t, dir, "new", "main", "-m", side)
		write(t, dir, "seed.txt", side+"\n")
		write(t, dir, "run.sh", side+"\n")
		if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, dir, "other.txt", "other\n")
	}
	jj(t, dir, "rebase", "-r", "@", "-d", `description(exact:"x\n")`)
	return dir
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
		Octopus: func(t *testing.T) string {
			dir := jjRepo(t, true, "--no-colocate")
			for _, d := range []string{"a", "b", "c"} {
				jj(t, dir, "new", "main", "-m", d)
			}
			jj(t, dir, "new", `description(exact:"a\n")`, `description(exact:"b\n")`, `description(exact:"c\n")`)
			return dir
		},
		ConflictedHead: func(t *testing.T) string {
			dir := conflictedHead(t)
			jj(t, dir, "new")
			return dir
		},
		ConflictedMerge: func(t *testing.T) string {
			dir := conflictedHead(t)
			jj(t, dir, "new", "main", "-m", "z")
			write(t, dir, "z.txt", "z\n")
			jj(t, dir, "new", `description(exact:"y\n")`, `description(exact:"z\n")`)
			return dir
		},
		ConflictedBase: func(t *testing.T) string {
			dir := conflictedHead(t)
			for _, side := range []string{"p1", "p2"} {
				jj(t, dir, "new", `description(exact:"y\n")`, "-m", side)
				content := map[string]string{"p1": "r", "p2": "x"}[side]
				write(t, dir, "seed.txt", content+"\n")
				write(t, dir, "run.sh", content+"\n")
			}
			jj(t, dir, "new", `description(exact:"p1\n")`, `description(exact:"p2\n")`)
			return dir
		},
		Ignored: func(t *testing.T) (string, string) {
			dir := jjRepo(t, true, "--no-colocate")
			write(t, dir, ".jj/repo/store/git/info/exclude", "*.log\n")
			return dir, "debug.log"
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
