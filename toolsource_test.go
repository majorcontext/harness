package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// counting is a Store that counts the reads of session records.
type counting struct {
	harness.Store
	reads atomic.Int64
}

func (c *counting) Read(ctx context.Context, id string, after uint64, limit int) ([]harness.Record, error) {
	c.reads.Add(1)
	return c.Store.Read(ctx, id, after, limit)
}

// pluginTurn starts one turn of a fake backend under the fixture plugin with
// config cfg, and returns the run of that turn and the reads of the store.
func pluginTurn(t *testing.T, ownsLoop bool, cfg string) (fakeRun, *counting) {
	t.Helper()
	st, f := &counting{Store: harness.NewMemStore()}, newFake()
	f.ownsLoop = ownsLoop
	r, err := harness.NewWithBackend(harness.Options{Store: st, Config: config.Config{Plugins: pluginFixture(t, cfg)}}, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	s, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "fake/model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(bg, text("a", "go")); err != nil {
		t.Fatal(err)
	}
	run := <-f.runs
	t.Cleanup(func() { close(run.items) })
	return run, st
}

func TestSystemTransformRunsOnlyForABackendThatReadsThePrompt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ownsLoop bool
	}{
		{"a harness loop sends the segments of the plugin", false},
		{"a backend that owns the loop gets no segment", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, _ := pluginTurn(t, tc.ownsLoop, `{"segment":"SEGMENT"}`)
			if got := strings.Contains(run.req.Instructions, "SEGMENT"); got == tc.ownsLoop {
				t.Errorf("instructions %q hold the segment = %v, want %v", run.req.Instructions, got, !tc.ownsLoop)
			}
		})
	}
}

func TestAPluginReadsTheHistoryOfARunningSessionFromTheActor(t *testing.T) {
	run, st := pluginTurn(t, false, `{"recall":true}`)
	if !strings.Contains(run.req.Instructions, "LAST-USER: go") {
		t.Fatalf("instructions %q do not show that the plugin read the history", run.req.Instructions)
	}
	if n := st.reads.Load(); n != 0 {
		t.Errorf("store reads for the history of a running session = %d, want 0", n)
	}
}

func TestAProfileKeepsThePluginToolsOfItsList(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := "---\nname: mixed\ndescription: Echoes and reads.\ntools: fixture_echo, ls\n---\n\nUse both tools.\n"
	if err := os.WriteFile(filepath.Join(dir, ".agents", "mixed.md"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	task := harnesstest.ToolCall{ID: "call_1", Name: "task", Input: map[string]any{"agent": "mixed", "prompt": "child work"}}
	r, s := pluginRuntime(t, dir, pluginFixture(t, `{}`), nil,
		harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{task}}},
		harnesstest.Step{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "child done"}},
		harnesstest.Step{Name: "waiting", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
		harnesstest.Step{Name: "report", Match: harnesstest.LastUserText("A background task you started has finished."), Reply: harnesstest.Reply{Text: "ok"}})
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Submit(bg, text("a", "delegate")); err != nil {
		t.Fatal(err)
	}
	ended := 0
	for e, err := range sess.Events(bg, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			if ended++; ended == 2 {
				break
			}
		}
	}
	for _, req := range s.Requests() {
		if strings.Contains(req.LastUserText(), "child work") {
			if want := []string{"fixture_echo", "ls"}; !slices.Equal(req.Tools, want) {
				t.Errorf("child tools %q, want %q", req.Tools, want)
			}
			return
		}
	}
	t.Error("no request of the child")
}
