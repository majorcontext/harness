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
			name:    "anthropic_adaptive_thinking_raises_max_tokens_for_the_level",
			model:   effortSteps("high", "max"),
			actions: effortTurns("anthropic/claude-opus-5-5", "high", "max"),
		},
		{
			name:    "bedrock_mantle_claude_id_sends_the_effort_field",
			model:   effortSteps("max"),
			actions: effortTurns("anthropic/bedrock_mantle/anthropic.claude-opus-5-5-v1:0", "max"),
		},
		{
			name:    "thinking_off_on_a_model_that_lists_off_sends_no_thinking",
			model:   effortSteps("off"),
			actions: effortTurns("anthropic/claude-sonnet-5", "off"),
		},
		{
			name: "thinking_off_names_a_model_that_always_thinks",
			actions: []action{
				create{as: "a", model: "anthropic/claude-opus-5-5"},
				command{as: "a", text: "/thinking off"}, awaitCommands{as: "a"},
				commandRecords{as: "a"},
			},
		},
		{
			name: "thinking_command_accepts_max_and_names_the_clamp",
			actions: []action{
				create{as: "a", model: "anthropic/claude-haiku-4-5"},
				command{as: "a", text: "/thinking max"}, awaitCommands{as: "a"},
				command{as: "a", text: "/model anthropic/claude-opus-5-5"}, awaitCommands{as: "a"},
				command{as: "a", text: "/thinking max"}, awaitCommands{as: "a"},
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

func TestContractEffortMinimalClampsUp(t *testing.T) {
	t.Parallel()
	runCodexScenarios(t, []codexScenario{{
		scenario: scenario{
			name:  "codex_minimal_on_a_model_without_it_clamps_up_to_low",
			model: effortSteps("minimal"),
			actions: codexSession([]action{
				setModel{as: "a", model: "codex/gpt-5.5"},
				command{as: "a", text: "/thinking minimal"}, awaitCommands{as: "a"},
				commandRecords{as: "a"},
				submit{as: "a", text: "minimal"}, waitIdle{as: "a"}, recordWire{},
			}),
		},
	}})
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
