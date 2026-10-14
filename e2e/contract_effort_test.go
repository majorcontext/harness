package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func effortTurns(model string, levels ...string) []action {
	actions := []action{create{as: "a", model: model}}
	for _, l := range levels {
		actions = append(actions, setThinking{as: "a", level: l}, submit{as: "a", text: l}, waitIdle{as: "a"})
	}
	return actions
}

func effortSteps(levels ...string) []harnesstest.Step {
	var steps []harnesstest.Step
	for _, l := range levels {
		steps = append(steps, harnesstest.Step{Name: l, Match: harnesstest.LastUserText(l), Reply: harnesstest.Reply{Text: "ok " + l}})
	}
	return steps
}

func TestContractEffortLevels(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:    "anthropic_effort_field_carries_xhigh_and_max",
			model:   effortSteps("xhigh", "max"),
			actions: effortTurns("anthropic/claude-opus-5-5", "xhigh", "max"),
		},
		{
			name:    "anthropic_effort_field_clamps_xhigh_for_a_model_without_it",
			model:   effortSteps("xhigh", "max"),
			actions: effortTurns("anthropic/claude-sonnet-4-6", "xhigh", "max"),
		},
		{
			name:    "anthropic_budget_only_model_clamps_xhigh_and_max_to_the_high_budget",
			model:   effortSteps("xhigh", "max"),
			actions: effortTurns("anthropic/claude-haiku-4-5", "xhigh", "max"),
		},
		{
			name:    "unknown_model_clamps_xhigh_and_max_to_high",
			model:   effortSteps("xhigh", "max"),
			actions: effortTurns("anthropic/claude-unlisted-9", "xhigh", "max"),
		},
		{
			name: "thinking_command_accepts_max_and_names_the_clamp",
			actions: []action{
				create{as: "a", model: "anthropic/claude-haiku-4-5"},
				command{as: "a", text: "/thinking max"}, waitIdle{as: "a"},
				command{as: "a", text: "/model anthropic/claude-opus-5-5"}, waitIdle{as: "a"},
				command{as: "a", text: "/thinking max"}, waitIdle{as: "a"},
				commandRecords{as: "a"},
			},
		},
	})
}

func TestContractEffortLevelsChat(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:    "bifrost_clamps_xhigh_and_max_to_high",
			chat:    true,
			model:   effortSteps("xhigh", "max"),
			actions: effortTurns("", "xhigh", "max"),
		},
	})
}

func TestContractEffortLevelsCodex(t *testing.T) {
	t.Parallel()
	turn := func(model string, levels ...string) []action {
		actions := []action{setModel{as: "a", model: model}}
		for _, l := range levels {
			actions = append(actions, setThinking{as: "a", level: l}, submit{as: "a", text: l}, waitIdle{as: "a"})
		}
		return append(actions, recordWire{})
	}
	runCodexScenarios(t, []codexScenario{
		{
			scenario: scenario{
				name:    "codex_reasoning_effort_carries_xhigh_and_max",
				model:   effortSteps("xhigh", "max"),
				actions: codexSession(turn("codex/gpt-6-astra", "xhigh", "max")),
			},
		},
		{
			scenario: scenario{
				name:    "codex_clamps_a_level_that_the_model_does_not_list",
				model:   effortSteps("max", "minimal"),
				actions: codexSession(turn("codex/gpt-5.5", "max"), turn("codex/gpt-6-astra", "minimal")),
			},
		},
	})
}

func TestContractEffortLevelsClaudeCode(t *testing.T) {
	runScenarios(t, []scenario{{
		name:   "claudecode_effort_flag_carries_xhigh_and_max",
		driver: claudeLaneDriver("normal"),
		actions: []action{
			create{as: "a"},
			setThinking{as: "a", level: "xhigh"}, submit{as: "a", text: "one"}, waitIdle{as: "a"},
			setThinking{as: "a", level: "max"}, submit{as: "a", text: "two"}, waitIdle{as: "a"},
			claudeInvocations{as: "a"},
		},
	}})
}
