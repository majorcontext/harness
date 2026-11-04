package e2e

import (
	"archive/zip"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type checkpointDependencyVersioned struct{ as string }

func (a checkpointDependencyVersioned) run(t *testing.T, r *run) {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	proxy := filepath.Join(dir, "proxy")
	for _, module := range []string{"github.com/majorcontext/harness", "example.com/checkpoint-fork"} {
		for _, version := range []string{"v0.0.1", "v0.0.2"} {
			writeCheckpointModule(t, root, proxy, module, version)
		}
	}
	cacheCmd := exec.CommandContext(t.Context(), "go", "env", "GOMODCACHE")
	cache, err := cacheCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	localProxy := (&url.URL{Scheme: "file", Path: proxy}).String()
	cacheProxy := (&url.URL{Scheme: "file", Path: filepath.Join(strings.TrimSpace(string(cache)), "cache", "download")}).String()
	env := append(os.Environ(), "GOWORK=off", "GOSUMDB=off", "GOFLAGS=-modcacherw", "GOPROXY="+localProxy+","+cacheProxy, "GOMODCACHE="+filepath.Join(dir, "modcache"))
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	base := strings.Replace(string(mod), "module github.com/majorcontext/harness", "module example.com/embedder", 1)
	writeCheckpointFile(t, filepath.Join(dir, "main.go"), []byte(checkpointVersionedProbe))
	writeCheckpointFile(t, filepath.Join(dir, "go.sum"), sum)
	checkpoint := filepath.Join(dir, "checkpoint.json")
	builds := []struct{ version, replacement, mode string }{
		{"v0.0.1", "", "initial"},
		{"v0.0.1", "example.com/checkpoint-fork v0.0.1", "invalidate"},
		{"v0.0.2", "example.com/checkpoint-fork v0.0.1", "reuse"},
		{"v0.0.1", "example.com/checkpoint-fork v0.0.2", "invalidate"},
		{"v0.0.2", "", "invalidate"},
	}
	for i, b := range builds {
		content := base + "\nrequire github.com/majorcontext/harness " + b.version + "\n"
		if b.replacement != "" {
			content += "replace github.com/majorcontext/harness => " + b.replacement + "\n"
		}
		writeCheckpointFile(t, filepath.Join(dir, "go.mod"), []byte(content))
		binary := filepath.Join(dir, fmt.Sprintf("probe-%d", i))
		runCheckpointCommand(t, dir, env, "go", "build", "-mod=mod", "-buildvcs=false", "-o", binary, ".")
		runCheckpointCommand(t, dir, env, binary, r.drv.storeDir(), r.id(t, a.as), checkpoint, b.mode)
		if i == 0 || i == 1 {
			runCheckpointCommand(t, dir, env, binary, r.drv.storeDir(), r.id(t, a.as), checkpoint, "reuse")
		}
	}
}

func writeCheckpointFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func runCheckpointCommand(t *testing.T, dir string, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), name, args...)
	cmd.Dir, cmd.Env = dir, env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func writeCheckpointModule(t *testing.T, root, proxy, module, version string) {
	t.Helper()
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	mod = []byte(strings.Replace(string(mod), "module github.com/majorcontext/harness", "module "+module, 1))
	base := filepath.Join(proxy, module, "@v", version)
	writeCheckpointFile(t, base+".mod", mod)
	writeCheckpointFile(t, base+".info", []byte(fmt.Sprintf(`{"Version":%q,"Time":"2020-01-01T00:00:00Z"}`, version)))
	f, err := os.Create(base + ".zip")
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "e2e" || d.Name() == "examples" || d.Name() == "sdk" || d.Name() == "cmd") {
				return filepath.SkipDir
			}
			return nil
		}
		if rel != "go.mod" && (!strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go")) {
			return nil
		}
		data := mod
		if rel != "go.mod" {
			data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		w, err := z.Create(module + "@" + version + "/" + filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

const checkpointVersionedProbe = `package main
import (
 "context"
 "encoding/json"
 "fmt"
 "os"
 "github.com/majorcontext/harness"
 "github.com/majorcontext/harness/store"
)
type checkpoint struct { Seq uint64; Data []byte }
type tracked struct { harness.Store; path string; loads, saves int; fromStart bool }
func (s *tracked) Read(ctx context.Context, id string, after uint64, limit int) ([]harness.Record, error) {
 if after == 0 { s.fromStart = true }; return s.Store.Read(ctx, id, after, limit)
}
func (s *tracked) LoadCheckpoint(context.Context, string) (uint64, []byte, error) {
 s.loads++; b, err := os.ReadFile(s.path); if os.IsNotExist(err) { return 0, nil, nil }; if err != nil { return 0, nil, err }
 var c checkpoint; err = json.Unmarshal(b, &c); return c.Seq, c.Data, err
}
func (s *tracked) SaveCheckpoint(_ context.Context, _ string, seq uint64, data []byte) error {
 s.saves++; b, err := json.Marshal(checkpoint{seq, data}); if err != nil { return err }; return os.WriteFile(s.path, b, 0600)
}
func main() {
 s := &tracked{Store: store.NewDiskStore(os.Args[1]), path: os.Args[3]}
 if _, err := harness.OpenView(context.Background(), s, os.Args[2]); err != nil { panic(err) }
 mode := os.Args[4]; replay := mode != "reuse"; saves := 0; if replay { saves = 1 }
 if s.loads != 1 || s.saves != saves || s.fromStart != replay { panic(fmt.Sprintf("%s: loads=%d saves=%d replay=%v", mode, s.loads, s.saves, s.fromStart)) }
}
`
