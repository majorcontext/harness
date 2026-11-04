package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type checkpointDependencyLocal struct{ as string }

func (a checkpointDependencyLocal) run(t *testing.T, r *run) {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":  strings.Replace(string(mod), "module github.com/majorcontext/harness", "module example.com/embedder", 1) + fmt.Sprintf("\nrequire github.com/majorcontext/harness v0.0.0\nreplace github.com/majorcontext/harness => %q\n", root),
		"go.sum":  string(sum),
		"main.go": checkpointDependencyProbe,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("go", "build", "-mod=mod", "-buildvcs=false", "-o", filepath.Join(t.TempDir(), "warm"), ".")
	run("git", "init", "-q")
	run("git", "add", ".")
	run("git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "fixture")
	binary := filepath.Join(t.TempDir(), "probe")
	run("go", "build", "-buildvcs=true", "-o", binary, ".")
	run(binary, r.drv.storeDir(), r.id(t, a.as))
}

const checkpointDependencyProbe = `package main
import (
 "context"
 "fmt"
 "os"
 "github.com/majorcontext/harness"
 "github.com/majorcontext/harness/store"
)
type tracked struct { harness.Store; loads, saves int }
func (s *tracked) LoadCheckpoint(context.Context, string) (uint64, []byte, error) { s.loads++; return 0, nil, nil }
func (s *tracked) SaveCheckpoint(context.Context, string, uint64, []byte) error { s.saves++; return nil }
func main() {
 s := &tracked{Store: store.NewDiskStore(os.Args[1])}
 if _, err := harness.OpenView(context.Background(), s, os.Args[2]); err != nil { panic(err) }
 if s.loads != 0 || s.saves != 0 { panic(fmt.Sprintf("local replacement called checkpoint: loads=%d saves=%d", s.loads, s.saves)) }
}
`
