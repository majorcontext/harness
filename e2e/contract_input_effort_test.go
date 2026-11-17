package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

var inputEffortRows = map[string]runtimeRow{
	"input_effort_applies_to_the_turn_of_that_input_only":                sameAsServe(),
	"input_effort_beats_the_session_effort":                              sameAsServe(),
	"an_unknown_input_effort_is_invalid_request":                         sameAsServe(),
	"a_steer_that_sets_a_higher_effort_raises_the_turn_effort":           sameAsServe(),
	"a_steer_that_sets_no_effort_returns_the_turn_to_the_session_effort": sameAsServe(),
}

func TestContractInputEffort(t *testing.T) {
	const model = "anthropic/claude-opus-5-5"
	hold := harnesstest.Step{Name: "hold", Match: harnesstest.LastUserText("hold"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "read_file", Input: map[string]any{"path": "/nonexistent-contract-dir/missing.txt"}}}}}
	rest := harnesstest.Step{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true}
	steered := func(session, steer string) []action {
		actions := []action{create{as: "a", model: model}}
		if session != "" {
			actions = append(actions, setThinking{as: "a", level: session})
		}
		return append(actions,
			postInputEffort{as: "a", text: "hold", effort: "low"}, awaitRequests{n: 1},
			postInputEffort{as: "a", text: "one", effort: steer},
			release{step: "hold"}, waitIdle{as: "a"})
	}
	runScenarios(t, []scenario{
		{
			name:  "input_effort_applies_to_the_turn_of_that_input_only",
			model: []harnesstest.Step{rest},
			actions: []action{
				create{as: "a", model: model},
				postInputEffort{as: "a", text: "text", effort: "low"}, waitIdle{as: "a"},
				submit{as: "a", text: "console"}, waitIdle{as: "a"},
			},
		},
		{
			name:  "input_effort_beats_the_session_effort",
			model: []harnesstest.Step{rest},
			actions: []action{
				create{as: "a", model: model}, setThinking{as: "a", level: "high"},
				postInputEffort{as: "a", text: "text", effort: "low"}, waitIdle{as: "a"},
				submit{as: "a", text: "console"}, waitIdle{as: "a"},
			},
		},
		{
			name:    "an_unknown_input_effort_is_invalid_request",
			actions: []action{create{as: "a", model: model}, postInputEffort{as: "a", text: "text", effort: "ultra"}},
		},
		{
			name:    "a_steer_that_sets_a_higher_effort_raises_the_turn_effort",
			model:   []harnesstest.Step{hold, rest},
			actions: steered("", "medium"),
		},
		{
			name:    "a_steer_that_sets_no_effort_returns_the_turn_to_the_session_effort",
			model:   []harnesstest.Step{hold, rest},
			actions: steered("high", ""),
		},
	})
}
