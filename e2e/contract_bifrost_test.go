package e2e

import (
	"maps"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

const (
	bifrostModel     = "fireworks/accounts/fireworks/routers/firerouter"
	bifrostEvalModel = "vendor/eval-model"
)

// scenarioFake starts the scripted model server a scenario names and returns
// the config that points the serve process at it.
func scenarioFake(t *testing.T, sc scenario) (*harnesstest.Server, map[string]any) {
	t.Helper()
	if !sc.chat {
		return harnesstest.New(t, sc.model...), sc.config
	}
	fake := harnesstest.NewChat(t, sc.model...)
	config := map[string]any{
		"model":                "bifrost/" + bifrostModel,
		"goal_evaluator_model": "bifrost/" + bifrostEvalModel,
		"providers": map[string]any{
			"bifrost": map[string]any{
				"type":          "openai-compat",
				"api_key_env":   "ANTHROPIC_API_KEY",
				"base_url":      fake.URL() + "/v1",
				"extra_headers": map[string]string{"X-Tag": "e2e"},
			},
		},
	}
	maps.Copy(config, sc.config)
	return fake, config
}

// expectGatewayHeaders fails unless every model request carried the bearer
// key and the configured extra header.
type expectGatewayHeaders struct{}

func (expectGatewayHeaders) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	if len(reqs) == 0 {
		t.Fatal("no model requests reached the gateway")
	}
	for i, req := range reqs {
		if got := req.Header.Get("Authorization"); got != "Bearer e2e-dummy-key" {
			t.Errorf("request %d Authorization = %q, want the configured key as a bearer token", i+1, got)
		}
		if got := req.Header.Get("X-Tag"); got != "e2e" {
			t.Errorf("request %d X-Tag = %q, want e2e", i+1, got)
		}
	}
}

func TestContractBifrost(t *testing.T) {
	bash := func(id, cmd string) harnesstest.ToolCall {
		return harnesstest.ToolCall{ID: id, Name: "bash", Input: map[string]any{"command": cmd}}
	}
	oneTurn := []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}}
	runScenarios(t, []scenario{
		{
			name:    "bifrost_text_reply",
			chat:    true,
			model:   []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: "hi"}}},
			actions: append(append([]action{}, oneTurn...), expectGatewayHeaders{}),
		},
		{
			name: "bifrost_tool_round_trip",
			chat: true,
			model: []harnesstest.Step{
				{Name: "call", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{bash("call_1", "echo bifrost")}}},
				{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "ran"}},
			},
			actions: oneTurn,
		},
		{
			name: "bifrost_two_tool_calls_one_turn",
			chat: true,
			model: []harnesstest.Step{
				{Name: "calls", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{bash("call_1", "echo a"), bash("call_2", "echo b")}}},
				{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "both"}},
			},
			actions: oneTurn,
		},
		{
			// The chat wire has no error flag on a tool message, so the error rides as a text prefix.
			name: "bifrost_tool_error_marker",
			chat: true,
			model: []harnesstest.Step{
				{Name: "call", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "read_file", Input: map[string]any{"path": "/nonexistent-contract-dir/missing.txt"}}}}},
				{Name: "after", Match: harnesstest.LastToolResult("read_file"), Reply: harnesstest.Reply{Text: "missing"}},
			},
			actions: oneTurn,
		},
		{
			name: "bifrost_reasoning_and_effort",
			chat: true,
			model: []harnesstest.Step{
				{Name: "unset", Match: harnesstest.LastUserText("one"), Reply: harnesstest.Reply{Reasoning: "weighing it", Text: "1"}},
				{Name: "high", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Reasoning: "weighing more", Text: "2"}},
				{Name: "off", Match: harnesstest.LastUserText("three"), Reply: harnesstest.Reply{Text: "3"}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"},
				waitIdle{as: "a"},
				setThinking{as: "a", level: "high"},
				submit{as: "a", text: "two"},
				waitIdle{as: "a"},
				setThinking{as: "a", level: "off"},
				submit{as: "a", text: "three"},
				waitIdle{as: "a"},
			},
		},
	})
}

func TestContractBifrostProviderErrors(t *testing.T) {
	ok := harnesstest.Step{Name: "ok", Reply: harnesstest.Reply{Text: "recovered"}}
	oneTurn := []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}}
	runScenarios(t, []scenario{
		{
			name: "bifrost_429_then_ok",
			chat: true,
			model: []harnesstest.Step{
				{Name: "limited", Reply: harnesstest.Reply{HTTPStatus: 429, RetryAfter: "1", ErrorMessage: "slow down"}},
				ok,
			},
			actions: append(append([]action{}, oneTurn...), getSession{as: "a"}),
		},
		{
			// The message carries no token counts, so only the structural error code classifies it.
			name: "bifrost_context_overflow",
			chat: true,
			model: []harnesstest.Step{
				{Name: "overflow", Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: harnesstest.ContextOverflowMessage, ErrorCode: "context_length_exceeded"}},
			},
			actions: append(append([]action{}, oneTurn...), getSession{as: "a"}),
		},
		{
			name: "bifrost_max_tokens_continuation",
			chat: true,
			model: []harnesstest.Step{
				{Name: "cut", Reply: harnesstest.Reply{Text: "first half ", StopReason: "max_tokens"}},
				{Name: "rest", Reply: harnesstest.Reply{Text: "second half"}},
			},
			actions: append(append([]action{}, oneTurn...), getSession{as: "a"}),
		},
	})
}

func TestContractBifrostGoal(t *testing.T) {
	armed := func(g setGoal) []action {
		return []action{create{as: "a"}, g, waitIdle{as: "a"}}
	}
	runScenarios(t, []scenario{
		{
			name: "bifrost_goal_met_first_turn",
			chat: true,
			model: []harnesstest.Step{
				agentStep("work", "done", false),
				evaluatorStep("judge", "MET: said done", false),
			},
			actions: armed(setGoal{as: "a", condition: "say done", maxTurns: 3}),
		},
		{
			name: "bifrost_goal_not_met_then_met",
			chat: true,
			model: []harnesstest.Step{
				agentStep("try", "try", false),
				evaluatorStep("judge1", "NOT MET: say done", false),
				agentStep("finish", "done", false),
				evaluatorStep("judge2", "MET: said done", false),
			},
			actions: armed(setGoal{as: "a", condition: "say done", maxTurns: 3}),
		},
	})
}
