package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var fsProbe struct {
	once sync.Once
	path string
	err  error
}

// buildFsProbe compiles e2e/testdata/fsprobe for js/wasm once and returns the module.
func buildFsProbe(t *testing.T) string {
	t.Helper()
	fsProbe.once.Do(func() {
		dir, err := os.MkdirTemp("", "harness-e2e-fsprobe")
		if err != nil {
			fsProbe.err = err
			return
		}
		fsProbe.path = filepath.Join(dir, "fsprobe.wasm")
		build := exec.Command("go", "build", "-o", fsProbe.path, "./testdata/fsprobe")
		build.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0")
		if msg, err := build.CombinedOutput(); err != nil {
			fsProbe.err = &buildError{err: err, out: string(msg)}
		}
	})
	if fsProbe.err != nil {
		t.Fatal(fsProbe.err)
	}
	return fsProbe.path
}

// fsRows adds the rows of the file system adapter: the Go probe on memory and on
// OPFS, and a Worker that ends after an fsync and a later write.
func (k *browserKit) fsRows() {
	k.add("the file system answers every call that Go makes", map[string]any{"kind": "fsprobe"}, func(t *testing.T, res browserResult, _ string) {
		k.noFailure(t, res)
		var r struct{ Lines []string }
		rowData(t, res, &r)
		ok := 0
		for _, l := range r.Lines {
			switch {
			case strings.HasPrefix(l, "ok "):
				ok++
			case strings.HasPrefix(l, "metric "):
				t.Log(l)
			default:
				t.Errorf("probe: %s", l)
			}
		}
		if ok < 40 {
			t.Errorf("%d checks passed, want at least 40: %v", ok, r.Lines)
		}
	})
	k.add("a Worker that ends after an fsync keeps the synced bytes and a prefix of the rest", map[string]any{"kind": "fskill"}, func(t *testing.T, res browserResult, _ string) {
		k.noFailure(t, res)
		var r struct {
			Written struct{ Durable int }
			Stored  struct {
				Size   int
				Prefix bool
			}
		}
		rowData(t, res, &r)
		if r.Stored.Size < r.Written.Durable || !r.Stored.Prefix {
			t.Errorf("stored %+v after durable %d bytes", r.Stored, r.Written.Durable)
		}
	})
}
