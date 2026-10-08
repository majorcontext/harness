package e2e

import (
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

// onHosts runs fn as a subtest on each host: the serve binary always, and
// the runtime in process with runtimeEnv. It runs a row that sets no
// environment, so the rows of a host share the process.
func onHosts(t *testing.T, fn func(t *testing.T, h host)) {
	t.Helper()
	if os.Getenv(runtimeEnv) == "" {
		t.Parallel()
	}
	for _, h := range []host{serveHost, runtimeHost} {
		if h.runtime && os.Getenv(runtimeEnv) == "" {
			continue
		}
		t.Run(h.name(), func(t *testing.T) { fn(t, h) })
	}
}

// startOn opens h in workdir over a scripted model. extra replaces or adds
// top-level config keys.
func startOn(t *testing.T, h host, workdir string, extra map[string]any, steps ...harnesstest.Step) (*runtimeDriver, *harnesstest.Server) {
	t.Helper()
	fake := harnesstest.New(t, steps...)
	return h.openIn(t, writeGoalConfigWith(t, fake.URL(), scenarioConfig(extra)), workdir, nil).(*runtimeDriver), fake
}

func replyText(text string) harnesstest.Step {
	return harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: text}, Repeat: true}
}

// runTurn creates a session, runs one prompt to idle, and returns its id.
func runTurn(t *testing.T, d *runtimeDriver, text string) string {
	t.Helper()
	id := d.Create(t)
	d.Submit(t, id, text)
	d.WaitIdle(t, id)
	return id
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
