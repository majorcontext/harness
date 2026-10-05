package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractModelTool(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	model := func(kv ...any) harnesstest.ToolCall { return ftTool("model", ftArgs(kv...)) }
	runScenarios(t, []scenario{{
		name: "model_tool_reports_lists_and_switches_the_model",
		config: map[string]any{
			"aliases": map[string]any{"quick": "anthropic/claude-quick-1"},
			"providers": map[string]any{
				"codex":       map[string]any{"type": "openai", "api_key_env": "ANTHROPIC_API_KEY", "base_url": "http://127.0.0.1:1"},
				"claude-code": map[string]any{"type": "claude-code-cli", "binary_path": "claude"},
				"openai":      map[string]any{"api_key_env": "ANTHROPIC_API_KEY"},
				"openrouter":  map[string]any{"api_key_env": "ANTHROPIC_API_KEY"},
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
		name: "model_tool_lists_the_registry_and_sets_native",
		config: map[string]any{
			"providers": map[string]any{
				"gateway": map[string]any{"type": "openai-compat", "api_key_env": "ANTHROPIC_API_KEY", "base_url": "http://127.0.0.1:1"},
			},
		},
		model:   toolChain(model("action", "list"), model("action", "set", "model", "openai/gpt-native-1"))[:2],
		actions: oneTurn,
	}, {
		name:    "model_tool_false_removes_the_model_tool",
		config:  map[string]any{"model_tool": false},
		model:   toolChain(),
		actions: oneTurn,
	}})
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
				"openai":     map[string]any{"api_key_env": "ANTHROPIC_API_KEY"},
				"openrouter": map[string]any{"api_key_env": "ANTHROPIC_API_KEY"},
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
