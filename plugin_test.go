package harness_test

import (
	"cmp"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// pluginFixture builds the wire-level plugin of the contract suite.
func pluginFixture(t *testing.T, cfg string) []config.PluginSpec {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pluginfixture")
	if out, err := exec.Command("go", "build", "-o", bin, "./harnesstest/pluginfixture").CombinedOutput(); err != nil {
		t.Fatalf("go build pluginfixture: %v\n%s", err, out)
	}
	return []config.PluginSpec{{Name: "fixture", Command: []string{bin}, Config: []byte(cfg)}}
}

func pluginRuntime(t *testing.T, plugins []config.PluginSpec, tools []harness.Tool, steps ...harnesstest.Step) (*harness.Runtime, *harnesstest.OpenAI) {
	t.Helper()
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, steps...)
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Tools: tools, Config: config.Config{
		Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
			BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}},
			"claude-code": {Type: config.TypeClaudeCodeCLI}},
		Plugins: plugins}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	return r, s
}

func TestPluginToolsAndHooks(t *testing.T) {
	call := func(id, name string, in map[string]any) harnesstest.ToolCall {
		return harnesstest.ToolCall{ID: id, Name: name, Input: in}
	}
	r, s := pluginRuntime(t, pluginFixture(t, `{"segment":"SEGMENT","recall":true}`), []harness.Tool{newProbe("bash", false), newProbe("write_file", false)},
		harnesstest.Step{Name: "calls", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
			call("call_1", "bash", map[string]any{"command": "echo rewrite-me"}),
			call("call_2", "bash", map[string]any{"command": "echo block-me"}),
			call("call_3", "fixture_echo", map[string]any{"text": "hi"}),
			call("call_4", "write_file", map[string]any{"path": "a.txt"}),
			call("call_5", "fixture_report", map[string]any{"edits": 1})}}},
		harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("fixture_report"), Reply: harnesstest.Reply{Text: "done"}},
		harnesstest.Step{Name: "events", Match: harnesstest.LastUserText("events"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
			call("call_6", "fixture_report", map[string]any{"events": 13})}}},
		harnesstest.Step{Name: "seen", Match: harnesstest.LastToolResult("fixture_report"), Reply: harnesstest.Reply{Text: "seen"}})
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	converse(t, sess, "go", "events")
	reqs := s.Requests()
	if len(reqs) != 4 {
		t.Fatalf("got %d requests, want 4", len(reqs))
	}
	for i, req := range reqs[:2] {
		if req.System != "SEGMENT\n\nLAST-USER: go" {
			t.Errorf("request %d system prompt %q: want the system.transform segments", i, req.System)
		}
	}
	tools := []string{"bash", "fixture_config", "fixture_crash", "fixture_echo", "fixture_fail", "fixture_report", "write_file"}
	if !slices.Equal(reqs[0].Tools, tools) {
		t.Errorf("tools %q, want %q", reqs[0].Tools, tools)
	}
	var got []string
	for _, m := range reqs[1].Messages {
		for _, p := range m.Parts {
			if p.Kind == "tool_result" {
				got = append(got, p.ToolName+": "+p.Text)
			}
		}
	}
	want := []string{
		`bash: after-hook saw: bash ran {"command":"echo rewritten"}`,
		`bash: [tool error] blocked by fixture`,
		`fixture_echo: echo: hi`,
		`write_file: write_file ran {"path":"a.txt"}`,
		`fixture_report: {"after_args":{"bash":{"command":"echo rewritten"},"fixture_echo":{"text":"hi"},"write_file":{"path":"a.txt"}},` +
			`"edited":["a.txt"],"session_ids_match":{"after":true,"event":true,"system_transform":true}}`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("tool results:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	events := `["session.status busy","tool.execute.start bash","tool.execute.end bash","tool.execute.start fixture_echo","tool.execute.end fixture_echo",` +
		`"tool.execute.start write_file","tool.execute.end write_file","file.edited absolute","tool.execute.start fixture_report","tool.execute.end fixture_report",` +
		`"session.status idle","session.status busy","tool.execute.start fixture_report"]`
	if got := lastText(reqs[3]); got != events {
		t.Errorf("events that the plugin saw:\n%s\nwant:\n%s", got, events)
	}
}

func TestPluginSeesTheModelOfEachCall(t *testing.T) {
	r, s := pluginRuntime(t, pluginFixture(t, `{"model":true}`), nil,
		harnesstest.Step{Name: "a", Match: harnesstest.LastUserText("a"), Reply: harnesstest.Reply{Text: "ok"}},
		harnesstest.Step{Name: "b", Match: harnesstest.LastUserText("b"), Reply: harnesstest.Reply{Text: "ok"}})
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	converse(t, sess, "a")
	changed := "codex/gpt-5-mini"
	if _, err := sess.Update(bg, protocol.SettingsPatch{Model: &changed}); err != nil {
		t.Fatal(err)
	}
	watch(t, sess, sess.View().HeadSeq-1, sess.View().HeadSeq, text("b", "b"), false)
	var got []string
	for _, req := range s.Requests() {
		got = append(got, req.System)
	}
	if want := []string{"MODEL: codex/gpt-5", "MODEL: codex/gpt-5-mini"}; !slices.Equal(got, want) {
		t.Errorf("system prompts %q, want %q", got, want)
	}
}

func TestCreateStartsThePlugins(t *testing.T) {
	fixture := pluginFixture(t, `{}`)
	for _, tc := range []struct {
		name    string
		plugins []config.PluginSpec
		tools   []harness.Tool
		model   string
		allowed []string
		want    string
	}{
		{name: "a missing executable fails", plugins: []config.PluginSpec{{Name: "gone", Command: []string{"/nonexistent/plugin"}}}, want: "plugin gone"},
		{name: "a manifest of another name fails", plugins: []config.PluginSpec{{Name: "other", Command: fixture[0].Command}}, want: `manifest name "fixture"`},
		{name: "a tool name that an embedder tool has fails", plugins: fixture, tools: []harness.Tool{newProbe("fixture_echo", false)}, want: `tool name "fixture_echo"`},
		{name: "a plugin tool with the name of a built-in tool fails", plugins: []config.PluginSpec{{Name: "fixture", Command: fixture[0].Command, Config: []byte(`{"extra_tool":"Read"}`)}},
			model: "claude-code/opus", want: `built-in tool`},
		{name: "a backend that owns the loop allows a plugin tool", plugins: fixture, model: "claude-code/opus", allowed: []string{"fixture_echo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := pluginRuntime(t, tc.plugins, tc.tools)
			_, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: cmp.Or(tc.model, "codex/gpt-5"), AllowedTools: tc.allowed})
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("Create = %v, want an error that names %q", err, tc.want)
			}
		})
	}
}

func TestCreateAfterCloseDoesNotProbeThePlugins(t *testing.T) {
	r, _ := pluginRuntime(t, []config.PluginSpec{{Name: "gone", Command: []string{"/nonexistent/plugin"}}}, nil)
	closeRuntime(t, r)
	if _, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"}); !errors.Is(err, harness.ErrDraining) {
		t.Errorf("Create after Close = %v, want ErrDraining", err)
	}
}

func TestViewListsEachPluginWithItsState(t *testing.T) {
	r, _ := pluginRuntime(t, pluginFixture(t, `{}`), nil,
		harnesstest.Step{Name: "echo", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
			{ID: "call_1", Name: "fixture_echo", Input: map[string]any{"text": "hi"}}}}},
		harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("fixture_echo"), Reply: harnesstest.Reply{Text: "done"}})
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.Plugin{Name: "fixture", State: "not-spawned",
		Tools: []string{"fixture_echo", "fixture_fail", "fixture_config", "fixture_crash", "fixture_report"},
		Hooks: []string{"system.transform", "tool.execute.before", "tool.execute.after", "event"}}
	check := func(when string, state string) {
		t.Helper()
		want.State = state
		if got := sess.View().Plugins; len(got) != 1 || !slices.Equal(got[0].Tools, want.Tools) || !slices.Equal(got[0].Hooks, want.Hooks) || got[0].Name != want.Name || got[0].State != state {
			t.Errorf("%s: View().Plugins = %+v, want [%+v]", when, got, want)
		}
	}
	check("before a plugin runs", "not-spawned")
	converse(t, sess, "go")
	check("after a plugin tool call", "running")
	page, err := r.List(bg, protocol.ListSessions{})
	if err != nil || len(page.Sessions) != 1 || len(page.Sessions[0].Plugins) != 1 {
		t.Errorf("List = %+v, %v; want the session with its plugin", page, err)
	}
}
