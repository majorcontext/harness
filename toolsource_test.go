package harness_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// awaitTurns waits until session s has ended n turns.
func awaitTurns(t *testing.T, s *harness.Session, n int) {
	t.Helper()
	for e, err := range s.Events(bg, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			if n--; n == 0 {
				return
			}
		}
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
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{},
		harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{task}}},
		harnesstest.Step{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "child done"}},
		harnesstest.Step{Name: "waiting", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
		harnesstest.Step{Name: "report", Match: harnesstest.LastUserText("A background task you started has finished."), Reply: harnesstest.Reply{Text: "ok"}})
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), WorkDir: dir, Config: config.Config{
		Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
			BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}},
		Plugins: pluginFixture(t, `{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Submit(bg, text("a", "delegate")); err != nil {
		t.Fatal(err)
	}
	awaitTurns(t, sess, 2)
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

func TestSystemTransformRunsOnlyForABackendThatReadsThePrompt(t *testing.T) {
	plugins := pluginFixture(t, `{"segment":"SEGMENT"}`)
	for _, tc := range []struct {
		name     string
		ownsLoop bool
		want     bool
	}{
		{"a harness loop sends the segments of the plugin", false, true},
		{"a backend that owns the loop gets no segment", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.ownsLoop = tc.ownsLoop
			r, err := harness.NewWithBackend(harness.Options{Store: harness.NewMemStore(), Config: config.Config{Plugins: plugins}}, f)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeRuntime(t, r) })
			sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "fake/model"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sess.Submit(bg, text("a", "hi")); err != nil {
				t.Fatal(err)
			}
			run := <-f.runs
			close(run.items)
			awaitTurns(t, sess, 1)
			if got := strings.Contains(run.req.Instructions, "SEGMENT"); got != tc.want {
				t.Errorf("instructions %q hold the segment = %v, want %v", run.req.Instructions, got, tc.want)
			}
		})
	}
}
