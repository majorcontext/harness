package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/prompt"
)

// putPlugins sends PUT /plugins with body, and records the name and state of each plugin in the answer.
type putPlugins struct {
	plugins []map[string]any
	body    any
	status  int
}

func (a putPlugins) run(t *testing.T, r *run) {
	t.Helper()
	body := a.body
	if body == nil {
		list := []any{}
		for _, p := range a.plugins {
			list = append(list, p)
		}
		body = map[string]any{"plugins": list}
	}
	res := r.drv.PutPlugins(t, body)
	if want := cmpStatus(a.status); res.Status != want {
		t.Fatalf("PUT /plugins = %d %v, want %d", res.Status, res.Body, want)
	}
	if res.Status != http.StatusOK {
		r.record(t, "put_plugins", "", res)
		return
	}
	states := []string{}
	list, _ := res.Body.(map[string]any)["plugins"].([]any)
	for _, e := range list {
		entry, _ := e.(map[string]any)
		for _, k := range []string{"name", "state", "tools", "hooks"} {
			if _, ok := entry[k]; !ok {
				t.Fatalf("PUT /plugins entry %v lacks %q", entry, k)
			}
		}
		states = append(states, entry["name"].(string)+" "+entry["state"].(string))
	}
	r.record(t, "put_plugins", "", callResult{Status: res.Status, Body: states})
}

// flat lists the parts of msgs in order, each with its role.
func flat(msgs []harnesstest.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, p := range m.Parts {
			out = append(out, m.Role+"|"+p.Kind+"|"+p.ToolName+"|"+p.Text)
		}
	}
	return out
}

func isTail(p harnesstest.Part) bool {
	return p.Kind == "text" && strings.HasPrefix(p.Text, prompt.EngineContextOpenTag)
}

// expectTail checks request req: its last block is an engine-context block that holds has and lacks lacks, its system
// prompt and its cache breakpoint hold no tail, and request prev without its own tail is a prefix of it.
type expectTail struct {
	req, prev  int
	has, lacks []string
}

func (a expectTail) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	if a.req < 1 || a.req > len(reqs) || a.prev < 1 || a.prev > len(reqs) {
		t.Fatalf("expectTail: requests %d and %d of %d", a.req, a.prev, len(reqs))
	}
	req, prev := reqs[a.req-1], reqs[a.prev-1]
	last := req.Messages[len(req.Messages)-1]
	tail := last.Parts[len(last.Parts)-1]
	if !isTail(tail) {
		t.Fatalf("request %d does not end with an engine-context block: %q", a.req, tail.Text)
	}
	for _, s := range a.has {
		if !strings.Contains(tail.Text, s) {
			t.Errorf("request %d tail lacks %q: %q", a.req, s, tail.Text)
		}
	}
	for _, s := range a.lacks {
		if strings.Contains(tail.Text, s) {
			t.Errorf("request %d tail holds %q: %q", a.req, s, tail.Text)
		}
	}
	for _, s := range append(a.has, a.lacks...) {
		if strings.Contains(req.System, s) {
			t.Errorf("request %d system prompt holds %q", a.req, s)
		}
	}
	if req.System != prev.System {
		t.Errorf("request %d system prompt differs from request %d", a.req, a.prev)
	}
	if got, want := req.CacheBreakpoints[len(req.CacheBreakpoints)-1], (harnesstest.CacheBreakpoint{Message: len(req.Messages) - 1, Block: len(last.Parts) - 2}); got != want {
		t.Errorf("request %d last cache breakpoint = %+v, want %+v (the last block before the tail)", a.req, got, want)
	}
	before := flat(prev.Messages)
	if pl := prev.Messages[len(prev.Messages)-1]; isTail(pl.Parts[len(pl.Parts)-1]) {
		before = before[:len(before)-1]
	}
	if got := flat(req.Messages); len(got) < len(before) || !slices.Equal(got[:len(before)], before) {
		t.Errorf("request %d without its tail is not a prefix of request %d:\n%q\n---\n%q", a.prev, a.req, before, got)
	}
}

// expectTools checks the tool names and the text of request req. sameAs, when set, is the request whose tools it equals.
type expectTools struct {
	req, sameAs int
	has, lacks  []string // tool names
	text        []string // substrings of the messages
}

func (a expectTools) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	if a.req < 1 || a.req > len(reqs) {
		t.Fatalf("expectTools: request %d of %d", a.req, len(reqs))
	}
	req := reqs[a.req-1]
	for _, n := range a.has {
		if !slices.Contains(req.Tools, n) {
			t.Errorf("request %d lacks tool %q: %v", a.req, n, req.Tools)
		}
	}
	for _, n := range a.lacks {
		if slices.Contains(req.Tools, n) {
			t.Errorf("request %d has tool %q", a.req, n)
		}
	}
	if a.sameAs > 0 && !slices.Equal(reqs[a.sameAs-1].Tools, req.Tools) {
		t.Errorf("request %d tools %v differ from request %d tools %v", a.req, req.Tools, a.sameAs, reqs[a.sameAs-1].Tools)
	}
	all := strings.Join(flat(req.Messages), "\n")
	for _, s := range a.text {
		if !strings.Contains(all, s) {
			t.Errorf("request %d messages lack %q:\n%s", a.req, s, all)
		}
	}
}

func TestContractPluginLiveSet(t *testing.T) {
	fixture := func(conf map[string]any) putPlugins {
		return putPlugins{plugins: []map[string]any{pluginEntry(t, "fixture", conf)}}
	}
	wrong := pluginEntry(t, "wrong", nil)
	wrong["config"].(map[string]any)["name"] = "fixture"
	tools := []string{"fixture_echo"}
	runScenarios(t, []scenario{
		{
			name:    "plugin_put_adds_a_plugin_and_its_before_hook_runs_on_the_next_tool_call",
			model:   afterFirstTurn(ftBash("echo block-me")),
			actions: liveTurns(fixture(nil)),
		},
		{
			name:    "plugin_put_with_one_bad_manifest_is_invalid_and_the_old_set_still_guards",
			config:  pluginConfig(t, nil),
			model:   afterFirstTurn(ftBash("echo block-me")),
			actions: liveTurns(putPlugins{plugins: []map[string]any{wrong}, status: http.StatusBadRequest}),
		},
		{
			name:    "plugin_put_with_a_tool_named_mcp_is_invalid_because_the_loader_has_that_name",
			config:  pluginConfig(t, nil),
			model:   afterFirstTurn(),
			actions: liveTurns(putPlugins{plugins: []map[string]any{pluginEntry(t, "fixture", map[string]any{"extra_tool": "mcp"})}, status: http.StatusBadRequest}),
		},
		{
			name:   "plugin_put_mid_session_moves_the_system_transform_segment_to_the_tail_of_the_next_call",
			config: pluginConfig(t, map[string]any{"segment": "SEG-A"}),
			model:  afterFirstTurn(),
			actions: append(liveTurns(fixture(map[string]any{"segment": "SEG-B"})),
				expectTail{req: 1, prev: 1, has: []string{"SEG-A"}, lacks: []string{"SEG-B"}},
				expectTail{req: 2, prev: 1, has: []string{"SEG-B"}, lacks: []string{"SEG-A"}},
			),
		},
		{
			name:   "plugin_put_adds_a_tool_that_is_deferred_named_in_the_notice_and_loaded_with_mcp_select",
			config: map[string]any{"plugins": []any{pluginEntry(t, "guard", nil)}},
			model: afterFirstTurn(
				mcpAction("action", "select", "tools", tools),
				ftTool("fixture_echo", ftArgs("text", "hi")),
			),
			actions: append(liveTurns(putPlugins{plugins: []map[string]any{pluginEntry(t, "guard", nil), pluginEntry(t, "fixture", nil)}}),
				expectTools{req: 2, sameAs: 1, lacks: tools, has: []string{"mcp"}, text: []string{"fixture_echo \u2014 Echo the text argument."}},
				expectTools{req: 3, has: tools},
				expectTools{req: 4, text: []string{"echo: hi"}},
			),
		},
		{
			name:   "plugin_put_removes_a_plugin_and_its_tool_answers_no_such_tool_available",
			config: pluginConfig(t, nil),
			model:  afterFirstTurn(ftTool("fixture_echo", ftArgs("text", "hi"))),
			actions: append(liveTurns(putPlugins{plugins: []map[string]any{}}),
				expectTools{req: 2, sameAs: 1, has: tools},
				expectTools{req: 3, text: []string{"no such tool available"}},
			),
		},
	})
}

// pluginLiveRows is the disposition of the rows of the live plugin set.
var pluginLiveRows = map[string]runtimeRow{
	"plugin_put_adds_a_plugin_and_its_before_hook_runs_on_the_next_tool_call":                sameAsServe(),
	"plugin_put_with_one_bad_manifest_is_invalid_and_the_old_set_still_guards":               sameAsServe(),
	"plugin_put_with_a_tool_named_mcp_is_invalid_because_the_loader_has_that_name":           sameAsServe(),
	"plugin_put_mid_session_moves_the_system_transform_segment_to_the_tail_of_the_next_call": sameAsServe(),
	"plugin_put_adds_a_tool_that_is_deferred_named_in_the_notice_and_loaded_with_mcp_select": sameAsServe(),
	"plugin_put_removes_a_plugin_and_its_tool_answers_no_such_tool_available":                sameAsServe(),
}
