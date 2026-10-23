package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

func TestContractModelTool(t *testing.T) {
	model := func(kv ...any) harnesstest.ToolCall { return ftTool("model", ftArgs(kv...)) }
	runScenarios(t, []scenario{{
		name: "model_tool_reports_lists_and_switches_the_model",
		config: map[string]any{
			"aliases": map[string]any{"quick": "anthropic/claude-quick-1"},
			"providers": map[string]any{
				"codex":       map[string]any{"type": "openai", "api_key_env": "ANTHROPIC_API_KEY", "base_url": "http://127.0.0.1:1"},
				"claude-code": map[string]any{"type": "claude-code-cli", "binary_path": "claude"},
				"openai":      map[string]any{"api_key_env": "ANTHROPIC_API_KEY"},
				"gateway":     map[string]any{"type": "openai-compat", "api_key_env": "ANTHROPIC_API_KEY", "base_url": "http://127.0.0.1:1"},
			},
		},
		model: toolChain(
			model("action", "status"),
			model("action", "list"),
			model("action", "set", "model", "quick"),
			model("action", "set", "model", "nope/model-x"),
			model("action", "set", "model", "anthropic/claude-direct-2"),
			model("action", "set"),
			model("action", "set", "model", "bare"),
			model("action", "clear"),
			model("action", "status"),
		),
		actions: oneTurn,
	}, {
		name:    "native_api_without_credentials_is_unavailable",
		config:  map[string]any{"providers": map[string]any{"openai": map[string]any{"base_url": "http://127.0.0.1:1"}}},
		model:   toolChain(model("action", "list"), model("action", "status"), model("action", "set", "model", "openai/gpt-native-1"), model("action", "status")),
		actions: []action{providerModels{"openai", false}, create{as: "a"}, setModel{"a", "openai/gpt-native-1"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, getSession{as: "a"}},
	}, {
		name: "native_api_with_credentials_is_available",
		driver: func(t *testing.T, h host, modelURL string) driver {
			cfg := map[string]any{"providers": map[string]any{
				"anthropic": map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": modelURL},
				"openai":    map[string]any{"base_url": "http://127.0.0.1:1"},
			}}
			return inTreeEnv(nil, "", cfg, map[string]string{"OPENAI_API_KEY": "test-placeholder"})(t, h, modelURL)
		},
		model:   toolChain(model("action", "list"), model("action", "status")),
		actions: append([]action{providerModels{"openai", true}}, append(oneTurn, setModel{"a", "openai/gpt-native-1"})...),
	}, {
		name:    "native_api_configured_header_runs_without_key_env",
		entry:   map[string]any{"api_key_env": "HEADER_ONLY_KEY", "extra_headers": map[string]string{"X-API-Key": "e2e-dummy-key"}},
		model:   []harnesstest.Step{{Name: "hello", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{Text: "hello"}}},
		actions: oneTurn,
	}, {
		name:    "uncredentialed_evaluator_rejects_goal_before_another_turn",
		config:  map[string]any{"goal_evaluator_model": "openai/gpt-evaluator"},
		model:   toolChain(ftTool("goal", ftArgs("action", "set", "condition", "ship it"))),
		actions: append(append([]action{}, oneTurn...), updateGoal{as: "a", condition: "ship it"}),
	}, {
		name:    "model_tool_false_removes_the_model_tool",
		config:  map[string]any{"model_tool": false},
		model:   toolChain(),
		actions: oneTurn,
	}})
}

var credentialProviderRows = map[string]runtimeRow{
	"uncredentialed_evaluator_rejects_goal_before_another_turn": sameAsServe(),
	"native_api_configured_header_runs_without_key_env":         sameAsServe(),
	"native_api_without_credentials_is_unavailable":             sameAsServe(),
	"native_api_with_credentials_is_available":                  sameAsServe(),
}

type providerModels struct {
	name    string
	present bool
}

func (a providerModels) run(t *testing.T, r *run) {
	raw, err := json.Marshal(r.drv.Models(t).Body)
	if err != nil {
		t.Fatal(err)
	}
	var models []protocol.Model
	if err := json.Unmarshal(raw, &models); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range models {
		if strings.HasPrefix(model.ID, a.name+"/") {
			found = true
			break
		}
	}
	if found != a.present {
		t.Fatalf("provider %q in model list = %v, want %v", a.name, found, a.present)
	}
}

func TestContractTaskSpawnModelAndEffort(t *testing.T) {
	spawn := func(prompt string, kv ...any) harnesstest.ToolCall {
		return ftTool("task", ftArgs(append([]any{"agent", "general-purpose", "prompt", prompt}, kv...)...))
	}
	child := func(name string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText("child work " + name), Reply: harnesstest.Reply{Text: "child " + name + " done", Block: true}}
	}
	report := func(name string) harnesstest.Step {
		return harnesstest.Step{Name: "report " + name, Match: harnesstest.LastUserText("child " + name + " done"), Reply: harnesstest.Reply{Text: "parent saw " + name}}
	}
	steps := append(promptChain("delegate",
		spawn("child work x", "effort", "bogus"),
		spawn("child work x", "model", "nope/model-x"),
		spawn("child work x", "model", "bare"),
		spawn("child work x", "model", "quick"),
		spawn("child work A", "model", "anthropic/claude-child-3", "effort", "high"),
		spawn("child work B"),
	), child("A"), child("B"), report("A"), report("B"))
	runScenarios(t, []scenario{{
		name:       "task_spawn_runs_the_child_on_its_model_and_effort",
		concurrent: true,
		config: map[string]any{
			"aliases": map[string]any{"quick": "anthropic/claude-quick-1"},
			"providers": map[string]any{
				"openai": map[string]any{"api_key_env": "ANTHROPIC_API_KEY"},
			},
		},
		model: steps,
		actions: []action{
			create{as: "a"},
			setThinking{as: "a", level: "medium"},
			submit{as: "a", text: "delegate"},
			awaitRequests{n: 9},
			release{step: "A"},
			awaitRequests{n: 10},
			waitIdle{as: "a"},
			release{step: "B"},
			awaitRequests{n: 11},
			waitIdle{as: "a"},
			bindChild{as: "kidA", parent: "a", nth: 0, record: true},
			bindChild{as: "kidB", parent: "a", nth: 1, record: true},
			getSession{as: "kidA"},
			getSession{as: "kidB"},
		},
	}})
}
