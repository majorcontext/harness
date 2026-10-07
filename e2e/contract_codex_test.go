package e2e

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/mcp"
)

// codexScenario runs against the scripted Responses server, with the model
// configured as boxes configures the ChatGPT Codex lane: provider "codex" of
// type "openai".
type codexScenario struct {
	scenario
	opts      harnesstest.OpenAIOptions
	websocket bool
	// mcpSchema, when set, serves one MCP tool named "send" with this input
	// schema as the server "srv".
	mcpSchema string
	// mcpLoading is the mcp_tool_loading of the mcpSchema server; the default is eager.
	mcpLoading string
	// bareKey configures the lane under the built-in "openai" provider key
	// with no type, as a deployment that points "openai" at the endpoint.
	bareKey bool
	// alsoOpenAI configures the endpoint under provider "openai" as well as "codex".
	alsoOpenAI bool
}

const mcpToolSchemaWithRejectedKeywords = `{"type":"object","properties":{"email":{"type":"string","format":"email","pattern":"^a"},"tags":{"type":"array","items":{"type":"string","minLength":1}}}}`

func serveMCPTool(t *testing.T, schema string) string {
	t.Helper()
	reg := mcp.NewRegistry("srv", "1")
	reg.RegisterTool(mcp.Tool{Name: "send", Description: "send a message", InputSchema: json.RawMessage(schema)},
		func(context.Context, json.RawMessage) (mcp.CallToolResult, error) {
			return mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: "sent"}}}, nil
		})
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	return srv.URL
}

// codexAPIKey is the value startServeIn gives ANTHROPIC_API_KEY.
const codexAPIKey = "e2e-dummy-key"

func codexConfig(baseURL string, websocket bool, extra map[string]any) map[string]any {
	cfg := map[string]any{
		"model": "codex/gpt-5.6-sol",
		"providers": map[string]any{"codex": map[string]any{
			"type":                    "openai",
			"api_key_env":             "ANTHROPIC_API_KEY", // the only key serve receives
			"base_url":                baseURL + "/backend-api/codex",
			"responses_path":          "/responses",
			"use_websocket_transport": websocket,
			"omit_response_params":    []string{"max_output_tokens", "temperature", "top_p", "metadata"},
			"sanitize_tool_schemas":   true,
		}},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

// wireAction is an action that reads the Responses server, so it runs only
// under runCodexScenario.
type wireAction interface {
	runWire(t *testing.T, r *run, o *harnesstest.OpenAI)
}

// recordWire records the transport-level events of the Responses server under
// "calls.codex_wire".
type recordWire struct{}

func (recordWire) run(t *testing.T, _ *run) { t.Fatal("recordWire runs only under runCodexScenario") }
func (recordWire) runWire(t *testing.T, r *run, o *harnesstest.OpenAI) {
	raw, err := json.Marshal(map[string]any{"events": o.WireEvents()})
	if err != nil {
		t.Fatal(err)
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	r.record(t, "codex_wire", "", callResult{Status: 200, Body: body})
}

// getSessionUsage records GET /session/{id} without subscription_usage.captured_at,
// which is the harness clock at capture time.
type getSessionUsage struct{ as string }

func (getSessionUsage) run(t *testing.T, _ *run) {
	t.Fatal("getSessionUsage runs only under runCodexScenario")
}
func (a getSessionUsage) runWire(t *testing.T, r *run, _ *harnesstest.OpenAI) {
	res := r.drv.GetSession(t, r.id(t, a.as))
	if body, ok := res.Body.(map[string]any); ok {
		if usage, ok := body["subscription_usage"].(map[string]any); ok {
			delete(usage, "captured_at")
		}
	}
	r.record(t, "get_session", a.as, res)
}

func bareOpenAIKey(cfg map[string]any) map[string]any {
	providers := cfg["providers"].(map[string]any)
	entry := providers["codex"].(map[string]any)
	delete(entry, "type")
	cfg["model"] = "openai/gpt-5.6-sol"
	cfg["providers"] = map[string]any{"openai": entry}
	return cfg
}

func runCodexScenario(t *testing.T, sc codexScenario, h host) observation {
	t.Helper()
	sc.opts.APIKey = codexAPIKey
	o := harnesstest.NewOpenAI(t, sc.opts, sc.model...)
	extra := maps.Clone(sc.config)
	if sc.mcpSchema != "" {
		if extra == nil {
			extra = map[string]any{}
		}
		extra["mcp_tool_loading"] = cmp.Or(sc.mcpLoading, "eager")
		extra["mcp_servers"] = map[string]any{"srv": map[string]any{"url": serveMCPTool(t, sc.mcpSchema)}}
	}
	cfg := codexConfig(o.URL(), sc.websocket, extra)
	if sc.bareKey {
		cfg = bareOpenAIKey(cfg)
	}
	if sc.alsoOpenAI {
		providers := cfg["providers"].(map[string]any)
		providers["openai"] = providers["codex"]
	}
	r := &run{
		drv:    h.newDriver(t, o.URL(), cfg),
		fake:   o.Server,
		ids:    map[string]string{},
		noIdle: map[string]bool{},
		keys:   map[string]int{},
	}
	for _, a := range sc.actions {
		if w, ok := a.(wireAction); ok {
			w.runWire(t, r, o)
			continue
		}
		a.run(t, r)
	}
	sessions := map[string][]transcriptMessage{}
	for _, alias := range r.aliases {
		r.drv.WaitIdle(t, r.ids[alias])
		sessions[alias] = r.drv.Messages(t, r.ids[alias])
		for _, v := range messageViolations(sessions[alias]) {
			t.Errorf("session %s: %s", alias, v)
		}
	}
	for _, j := range r.drv.Journals(t) {
		for _, v := range journalViolations(j) {
			t.Errorf("journal: %s", v)
		}
	}
	return normalizeRun(o.Requests(), sessions, r.calls, r.ids, r.drv.Workdir())
}

func runCodexScenarios(t *testing.T, table []codexScenario) {
	t.Helper()
	skipShort(t)
	for _, h := range []host{serveHost, runtimeHost} {
		onHost(t, h, table, func(sc codexScenario) (string, bool) { return sc.name, false },
			func(t *testing.T, sc codexScenario) observation { return runCodexScenario(t, sc, h) })
	}
}

func codexText(s string) harnesstest.Reply { return harnesstest.Reply{Text: s} }

var codexHi = []harnesstest.Step{{Name: "reply", Reply: codexText("hi")}}

var codexToolSteps = []harnesstest.Step{
	{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
		{ID: "call_1", Name: "bash", Input: map[string]any{"command": "echo codex"}},
	}}},
	{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: codexText("ran")},
}

func codexTurn(as, prompt string) []action {
	return []action{submit{as: as, text: prompt}, waitIdle{as: as}}
}

func codexSession(rest ...[]action) []action {
	actions := []action{create{as: "a"}}
	for _, r := range rest {
		actions = append(actions, r...)
	}
	return actions
}

func codexReasoning() harnesstest.OpenAIOptions {
	return harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"call": {Reasoning: []string{"plan the call", "check the args"}}}}
}

func codexUsage(withBengalfox bool) harnesstest.OpenAIOptions {
	limits := &harnesstest.RateLimits{
		Plan:      "pro",
		Primary:   &harnesstest.RateWindow{UsedPercent: 42.5, WindowMinutes: 10080, ResetAt: 1_900_000_000},
		Secondary: &harnesstest.RateWindow{UsedPercent: 7, WindowMinutes: 300, ResetAt: 1_800_000_000},
	}
	if withBengalfox {
		limits.BengalfoxPrimary = &harnesstest.RateWindow{UsedPercent: 1.5, WindowMinutes: 300, ResetAt: 1_700_000_000}
	}
	return harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"reply": {RateLimits: limits}}}
}

func TestContractCodex(t *testing.T) {
	runCodexScenarios(t, slices.Concat(codexWebSocketRows(), codexWarmRows(), codexHTTPRows(), codexPluginRows(t)))
}

// recordPrewarmHas records, for each websocket prewarm of the Responses
// server, whether its instructions hold text.
type recordPrewarmHas struct{ contains string }

func (recordPrewarmHas) run(t *testing.T, _ *run) {
	t.Fatal("recordPrewarmHas runs only under runCodexScenario")
}
func (a recordPrewarmHas) runWire(t *testing.T, r *run, o *harnesstest.OpenAI) {
	got := []any{}
	for _, instructions := range o.PrewarmInstructions() {
		got = append(got, strings.Contains(instructions, a.contains))
	}
	r.record(t, "prewarm_instructions_have", "", callResult{Status: 200, Body: got})
}

func codexPluginRows(t *testing.T) []codexScenario {
	return []codexScenario{{
		scenario: scenario{
			name:    "codex_ws_prewarm_carries_the_plugin_system_segment",
			config:  pluginConfig(t, nil),
			model:   codexHi,
			actions: codexSession(codexTurn("a", "hello"), []action{recordPrewarmHas{contains: fixtureSegment}, recordWire{}}),
		},
		websocket: true,
	}}
}

func codexWarmRows() []codexScenario {
	return []codexScenario{
		{
			// The first turn after a wake chains from the prewarm only when the
			// prewarm holds the tools that the turn holds, and the tool that the
			// log selected loads from the history that the warm-up reads.
			scenario: scenario{
				name: "codex_ws_restart_prewarms_with_a_tool_the_log_selected",
				model: []harnesstest.Step{
					{Name: "select", Match: harnesstest.LastUserText("select"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
						{ID: "call_1", Name: "mcp", Input: map[string]any{"action": "select", "tools": []string{"mcp__srv__send"}}},
					}}},
					{Name: "selected", Match: harnesstest.LastToolResult("mcp"), Reply: codexText("selected")},
					{Name: "again", Match: harnesstest.LastUserText("again"), Reply: codexText("again")},
				},
				actions: codexSession(codexTurn("a", "select"), []action{restart{}}, codexTurn("a", "again"), []action{recordWire{}}),
			},
			websocket:  true,
			mcpSchema:  mcpToolSchemaWithRejectedKeywords,
			mcpLoading: "lazy",
		},
	}
}

func codexWebSocketRows() []codexScenario {
	wire := []action{recordWire{}}
	twoSteps := []harnesstest.Step{
		{Name: "one", Match: harnesstest.LastUserText("one"), Reply: codexText("1")},
		{Name: "two", Match: harnesstest.LastUserText("two"), Reply: codexText("2")},
	}
	return []codexScenario{
		{
			scenario:  scenario{name: "codex_ws_prewarm_warms_first_turn", model: codexHi, actions: codexSession(codexTurn("a", "hello"), wire)},
			websocket: true,
		},
		{
			scenario: scenario{
				name:    "codex_ws_restart_warms_the_websocket_again",
				model:   twoSteps,
				actions: codexSession(codexTurn("a", "one"), []action{restart{}}, codexTurn("a", "two"), wire),
			},
			websocket: true,
		},
		{
			scenario:  scenario{name: "codex_ws_tool_round_trip", model: codexToolSteps, actions: codexSession(codexTurn("a", "run"), wire)},
			websocket: true,
		},
		{
			scenario:  scenario{name: "codex_ws_chains_two_turns", model: twoSteps, actions: codexSession(codexTurn("a", "one"), codexTurn("a", "two"), wire)},
			websocket: true,
		},
		{
			scenario:  scenario{name: "codex_ws_reasoning_chains_tool_round_trip", model: codexToolSteps, actions: codexSession(codexTurn("a", "run"), wire)},
			websocket: true,
			opts:      codexReasoning(),
		},
		{
			scenario: scenario{
				name:    "codex_ws_effort_sets_reasoning_effort",
				model:   codexHi,
				actions: codexSession([]action{setThinking{as: "a", level: "high"}}, codexTurn("a", "hello"), wire),
			},
			websocket: true,
		},
		{
			scenario:  scenario{name: "codex_ws_usage_frame_reaches_session", model: codexHi, actions: codexSession(codexTurn("a", "hello"), []action{getSessionUsage{as: "a"}, recordWire{}})},
			websocket: true,
			opts:      codexUsage(false),
		},
		{
			scenario:  scenario{name: "codex_ws_chain_miss_resends_full_history", model: twoSteps, actions: codexSession(codexTurn("a", "one"), codexTurn("a", "two"), wire)},
			websocket: true,
			opts:      harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"one": {Forget: true}}},
		},
		{
			scenario:  scenario{name: "codex_ws_uncoded_chain_miss_resends_full_history", model: twoSteps, actions: codexSession(codexTurn("a", "one"), codexTurn("a", "two"), wire)},
			websocket: true,
			opts:      harnesstest.OpenAIOptions{UncodedChainMiss: true, Replies: map[string]harnesstest.CodexReply{"one": {Forget: true}}},
		},
		{
			scenario: scenario{
				name: "codex_ws_drop_mid_turn_resends_full_history",
				model: []harnesstest.Step{
					twoSteps[0],
					{Name: "dropped", Match: harnesstest.LastUserText("two"), Reply: codexText("partial")},
					{Name: "retry", Match: harnesstest.LastUserText("two"), Reply: codexText("2")},
				},
				actions: codexSession(codexTurn("a", "one"), codexTurn("a", "two"), []action{getSession{as: "a"}, recordWire{}}),
			},
			websocket: true,
			opts:      harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"dropped": {Drop: true}}},
		},
		{
			scenario:  scenario{name: "codex_ws_refused_falls_back_to_http", model: codexHi, actions: codexSession(codexTurn("a", "hello"), wire)},
			websocket: true,
			opts:      harnesstest.OpenAIOptions{RefuseWebSocket: true},
		},
	}
}

func codexHTTPRows() []codexScenario {
	turn := func(name, user, reply string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText(user), Reply: codexText(reply)}
	}
	return []codexScenario{
		{
			scenario: scenario{
				name:  "codex_settings_switch_to_another_provider_keeps_the_history",
				model: []harnesstest.Step{turn("hi", "hi", "hello"), turn("again", "again", "ok")},
				actions: codexSession(codexTurn("a", "hi"),
					[]action{setModel{as: "a", model: "openai/gpt-5"}, setThinking{as: "a", level: "high"}},
					codexTurn("a", "again"), []action{recordWire{}}),
			},
			opts:       harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"hi": {Reasoning: []string{"plan"}}}},
			alsoOpenAI: true,
		},
		{
			scenario: scenario{
				name: "codex_http_truncated_and_empty_responses_are_retried",
				model: []harnesstest.Step{
					turn("dropped", "one", "partial"), turn("one", "one", "1"),
					{Name: "empty", Match: harnesstest.LastUserText("two")}, turn("two", "two", "2"),
					{Name: "plan_only", Match: harnesstest.LastUserText("three")}, turn("three", "three", "3"),
				},
				actions: codexSession(codexTurn("a", "one"), codexTurn("a", "two"), codexTurn("a", "three"), []action{getSession{as: "a"}}),
			},
			opts: harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{
				"dropped":   {Drop: true},
				"plan_only": {Reasoning: []string{"plan"}},
			}},
		},
		{
			scenario: scenario{
				name:    "codex_service_tier_reaches_the_request",
				model:   codexHi,
				actions: codexSession([]action{setServiceTier{as: "a", tier: "priority"}}, codexTurn("a", "hello"), []action{recordWire{}}),
			},
		},
		{
			scenario:  scenario{name: "codex_http_mcp_tool_schema_is_sanitized", model: codexHi, actions: codexSession(codexTurn("a", "hello"), []action{recordWire{}})},
			mcpSchema: mcpToolSchemaWithRejectedKeywords,
		},
		{
			scenario: scenario{name: "openai_key_http_sse_text_turn", model: codexHi, actions: codexSession(codexTurn("a", "hello"), []action{recordWire{}})},
			bareKey:  true,
		},
		{scenario: scenario{name: "codex_http_sse_text_turn", model: codexHi, actions: codexSession(codexTurn("a", "hello"), []action{recordWire{}})}},
		{scenario: scenario{name: "codex_http_sse_tool_round_trip_resends_history", model: codexToolSteps, actions: codexSession(codexTurn("a", "run"), []action{recordWire{}})}},
		{
			scenario: scenario{name: "codex_http_reasoning_replays_on_tool_round_trip", model: codexToolSteps, actions: codexSession(codexTurn("a", "run"), []action{recordWire{}})},
			opts:     codexReasoning(),
		},
		{
			scenario: scenario{name: "codex_http_usage_headers_reach_session", model: codexHi, actions: codexSession(codexTurn("a", "hello"), []action{getSessionUsage{as: "a"}})},
			opts:     codexUsage(true),
		},
	}
}
