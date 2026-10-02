package e2e

import (
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// runtimeWorkdir is a symlink-resolved temp dir, so a path that the model
// names equals the path that serve reports for its own working directory.
func runtimeWorkdir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// startRuntime starts one serve process in workdir over a scripted model.
// extra replaces or adds top-level config keys.
func startRuntime(t *testing.T, workdir string, extra map[string]any, steps ...harnesstest.Step) (*httpDriver, *harnesstest.Server) {
	t.Helper()
	fake := harnesstest.New(t, steps...)
	cfg := map[string]any{"context_window_tokens": 1_000_000}
	maps.Copy(cfg, extra)
	d := &httpDriver{
		sessDir: t.TempDir(),
		workDir: workdir,
		config:  writeGoalConfigWith(t, fake.URL(), cfg),
		enqSeq:  map[string]int64{},
	}
	d.p = startServeIn(t, d.sessDir, d.config, d.workDir)
	return d, fake
}

// patchConfig rewrites the JSON config file at path, so the next serve start reads the change.
func patchConfig(t *testing.T, path string, edit func(cfg map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	edit(cfg)
	raw, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// runTurn creates a session, runs one prompt to idle, and returns its id.
func runTurn(t *testing.T, d *httpDriver, text string) string {
	t.Helper()
	id := d.Create(t)
	d.Submit(t, id, text)
	d.WaitIdle(t, id)
	return id
}

// eventTip is the seq of the newest durable event of the serve process.
func eventTip(t *testing.T, d *httpDriver) int64 {
	t.Helper()
	res := d.call(t, http.MethodGet, "/event/tip", nil)
	n, err := res.Body.(map[string]any)["seq"].(json.Number).Int64()
	if err != nil {
		t.Fatalf("decode tip: %v", err)
	}
	return n
}
