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
		{
			// Known defect, pinned: after SIGKILL the queued input never runs.
			name:  "queued_input_runs_after_kill",
			model: []harnesstest.Step{slow, {Name: "next", Match: harnesstest.LastUserText("second"), Reply: harnesstest.Reply{Text: "done"}, Repeat: true}},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first"},
				awaitRequests{n: 1},
				enqueue{as: "a", text: "second"},
				restart{kill: true},
				waitIdle{as: "a"},
			},
		},
	})
}

func TestContractDurabilityUsage(t *testing.T) {
	usageSteps := []harnesstest.Step{
		{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{Usage: harnesstest.Usage{Input: 100, Output: 10},
			ToolCalls: []harnesstest.ToolCall{{ID: "toolu_usage", Name: "bash", Input: map[string]any{"command": "echo tool"}}}}},
		{Name: "after", Match: harnesstest.LastToolResult("bash"), Repeat: true,
			Reply: harnesstest.Reply{Text: "done", Block: true, Usage: harnesstest.Usage{Input: 40, Output: 4}}},
	}
	runScenarios(t, []scenario{
		{
			name:  "usage_survives_a_mid_turn_restart",
			model: usageSteps,
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
			name:  "usage_survives_a_kill_mid_turn",
			model: usageSteps,
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run"},
				awaitRequests{n: 2},
				restart{kill: true},
				getSession{as: "a"},
			},
		},
	})
}

func TestContractDurabilityHandoff(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "restart_lets_a_running_tool_finish_and_cuts_the_next_call",
			model: []harnesstest.Step{
				{Name: "calls", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_slow", Name: "bash", Input: map[string]any{"command": "touch tool-running; sleep 1"}},
					{ID: "toolu_next", Name: "bash", Input: map[string]any{"command": "echo two"}},
				}}},
				{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "done"}, Repeat: true},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "run"},
				awaitFile{path: "tool-running"},
				restart{},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
	})
}
