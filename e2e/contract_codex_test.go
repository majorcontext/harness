package e2e

import (
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// codexScenario runs against the scripted Responses server, with the model
// configured as boxes configures the ChatGPT Codex lane: provider "codex" of
// type "openai".
type codexScenario struct {
	scenario
	opts      harnesstest.OpenAIOptions
	websocket bool
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
	raw, err := json.Marshal(map[string]any{"events": o.WireRequests()})
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

func runCodexScenario(t *testing.T, sc codexScenario) observation {
	t.Helper()
	sc.opts.APIKey = codexAPIKey
	o := harnesstest.NewOpenAI(t, sc.opts, sc.model...)
	r := &run{
		drv:    newHTTPDriverWith(t, o.URL(), codexConfig(o.URL(), sc.websocket, sc.config)),
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
	for _, v := range journalViolations(r.drv.Events(t)) {
		t.Errorf("journal: %s", v)
	}
	return normalizeRun(o.Requests(), sessions, r.calls, r.ids, r.drv.Workdir())
}

func runCodexScenarios(t *testing.T, table []codexScenario) {
	t.Helper()
	skipShort(t)
	for _, sc := range table {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			compareGolden(t, sc.name, runCodexScenario(t, sc))
		})
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
	runCodexScenarios(t, append(codexWebSocketRows(), codexHTTPRows()...))
}

func codexWebSocketRows() []codexScenario {
	wire := []action{recordWire{}}
	twoTurns := []harnesstest.Step{
		{Name: "one", Match: harnesstest.LastUserText("one"), Reply: codexText("1")},
		{Name: "two", Match: harnesstest.LastUserText("two"), Reply: codexText("2")},
	}
	return []codexScenario{
		{
			scenario:  scenario{name: "codex_ws_prewarm_warms_first_turn", model: codexHi, actions: codexSession(codexTurn("a", "hello"), wire)},
			websocket: true,
		},
		{
			scenario:  scenario{name: "codex_ws_tool_round_trip", model: codexToolSteps, actions: codexSession(codexTurn("a", "run"), wire)},
			websocket: true,
		},
		{
			scenario:  scenario{name: "codex_ws_chains_two_turns", model: twoTurns, actions: codexSession(codexTurn("a", "one"), codexTurn("a", "two"), wire)},
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
			scenario:  scenario{name: "codex_ws_chain_miss_resends_full_history", model: twoTurns, actions: codexSession(codexTurn("a", "one"), codexTurn("a", "two"), wire)},
			websocket: true,
			opts:      harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"one": {Forget: true}}},
		},
		{
			scenario:  scenario{name: "codex_ws_uncoded_chain_miss_resends_full_history", model: twoTurns, actions: codexSession(codexTurn("a", "one"), codexTurn("a", "two"), wire)},
			websocket: true,
			opts:      harnesstest.OpenAIOptions{UncodedChainMiss: true, Replies: map[string]harnesstest.CodexReply{"one": {Forget: true}}},
		},
		{
			scenario: scenario{
				name: "codex_ws_drop_mid_turn_resends_full_history",
				model: []harnesstest.Step{
					twoTurns[0],
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
	return []codexScenario{
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
