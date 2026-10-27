package e2e

import "testing"

var stepLimitRows = map[string]runtimeRow{
	"max_steps_ends_the_turn_after_that_many_model_requests":   sameAsServe(),
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
			name:   "claudecode_max_steps_passes_max_turns_and_maps_the_limit",
			driver: claudeLaneDriver("max_turns"),
			actions: []action{
				create{as: "a", maxSteps: 2}, submit{as: "a", text: "run it"}, waitIdle{as: "a"},
				recordLastTurn{as: "a"}, claudeInvocations{as: "a"},
			},
		},
	})
}
