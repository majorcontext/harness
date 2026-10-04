package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractDurability(t *testing.T) {
	text := func(name, user, reply string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: reply}}
	}
	slow := harnesstest.Step{Name: "slow", Match: harnesstest.LastUserText("first"), Reply: harnesstest.Reply{Text: "partial", Block: true}}
	runScenarios(t, []scenario{
		{
			name:  "kill_mid_turn_then_continue",
			model: []harnesstest.Step{slow, text("again", "again", "ok")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				restart{kill: true},
				submit{as: "a", text: "again"},
				waitIdle{as: "a"},
			},
		},
		{
			name:  "history_survives_clean_restart",
			model: []harnesstest.Step{text("one", "one", "1"), text("two", "two", "2")},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"},
				waitIdle{as: "a"},
				restart{},
				submit{as: "a", text: "two"},
				waitIdle{as: "a"},
			},
		},
		{
			name: "usage_survives_a_mid_turn_restart",
			model: []harnesstest.Step{
				{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{Usage: harnesstest.Usage{Input: 100, Output: 10},
					ToolCalls: []harnesstest.ToolCall{{ID: "toolu_usage", Name: "bash", Input: map[string]any{"command": "echo tool"}}}}},
				{Name: "after", Match: harnesstest.LastToolResult("bash"), Repeat: true,
					Reply: harnesstest.Reply{Text: "done", Block: true, Usage: harnesstest.Usage{Input: 40, Output: 4}}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run"},
				awaitRequests{n: 2},
				restart{},
				release{step: "after"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
		{
			// Known defect, pinned: after SIGKILL the refolded queue entry is not dispatched.
			name:  "queued_input_survives_kill",
			model: []harnesstest.Step{slow},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				enqueue{as: "a", text: "second"},
				restart{kill: true},
				expectQueued{as: "a", texts: []string{"second"}},
			},
		},
	})
}
