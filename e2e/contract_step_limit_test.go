package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

var stepLimitRows = map[string]runtimeRow{
	"max_steps_ends_the_turn_after_that_many_model_requests":   sameAsServe(),
	"max_steps_leaves_a_steer_input_for_the_next_turn":         sameAsServe(),
	"claudecode_max_steps_passes_max_turns_and_maps_the_limit": sameAsServe(),
}

func TestContractStepLimit(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:  "max_steps_ends_the_turn_after_that_many_model_requests",
			model: toolChain(ftBash("echo one"), ftTool("no_such_tool", map[string]any{}))[:2],
			actions: []action{
				create{as: "a", maxSteps: 2}, submit{as: "a", text: "go"}, waitIdle{as: "a"},
				recordLastTurn{as: "a"},
			},
		},
		{
			name: "max_steps_leaves_a_steer_input_for_the_next_turn",
			model: []harnesstest.Step{
				{Name: "call1", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "toolu_1", Name: "bash", Input: map[string]any{"command": "echo one"}}}}},
				{Name: "call2", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{{ID: "toolu_2", Name: "bash", Input: map[string]any{"command": "echo two"}}}}},
				{Name: "next", Match: harnesstest.LastUserText("steer"), Reply: harnesstest.Reply{Text: "done"}},
			},
			actions: []action{
				create{as: "a", maxSteps: 2}, submit{as: "a", text: "run"}, awaitRequests{n: 2},
				submit{as: "a", text: "steer"}, release{step: "call2"}, waitIdle{as: "a"},
				recordLastTurn{as: "a"},
			},
		},
		{
			name:   "claudecode_max_steps_passes_max_turns_and_maps_the_limit",
			driver: claudeLaneDriver("max_turns"),
			actions: []action{
				create{as: "a", maxSteps: 2}, submit{as: "a", text: "run it"}, waitIdle{as: "a"},
				recordLastTurn{as: "a"}, claudeInvocations{as: "a"},
			},
		},
	})
}
