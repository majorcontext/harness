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
		sessDir:  t.TempDir(),
		workDir:  workdir,
		config:   writeGoalConfigWith(t, fake.URL(), cfg),
		enqSeq:   map[string]int64{},
		typedSeq: map[string]int64{},
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

// bodyOf is the decoded JSON object of a recorded call.
func bodyOf(t *testing.T, res callResult) map[string]any {
	t.Helper()
	obj, ok := res.Body.(map[string]any)
	if !ok {
		t.Fatalf("body = %#v, want a JSON object (status %d)", res.Body, res.Status)
	}
	return obj
}

// requestSystem is the system prompt segments of the newest model request of a session.
func requestSystem(t *testing.T, d *httpDriver, id string) []string {
	t.Helper()
	res := d.call(t, http.MethodGet, "/session/"+id+"/request", nil)
	if res.Status != http.StatusOK {
		t.Fatalf("GET /session/%s/request = %d %v", id, res.Status, res.Body)
	}
	var out []string
	for _, seg := range bodyOf(t, res)["system"].([]any) {
		out = append(out, seg.(string))
	}
	return out
}

// toolResults is the content of every tool result of a session, in order.
func toolResults(msgs []transcriptMessage) []transcriptPart {
	var out []transcriptPart
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == "tool_result" {
				out = append(out, p)
			}
		}
	}
	return out
}
