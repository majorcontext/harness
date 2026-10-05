package e2e

import (
	"slices"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// busyParentSteps is the model script of a parent whose child ends while the
// parent runs a tool, so the report joins the parent's turn. more run before
// the catch-all reply.
func busyParentSteps(more ...harnesstest.Step) []harnesstest.Step {
	steps := []harnesstest.Step{
		{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
			{ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"}},
		}}},
		{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{
			{ID: "toolu_2", Name: "ls", Input: map[string]any{"path": "."}},
		}}},
		{Name: "child_done", Match: harnesstest.LastToolResult("ls"), Reply: harnesstest.Reply{Text: "child done"}},
		{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{
			{ID: "toolu_bash", Name: "bash", Input: map[string]any{"command": "sleep 0.3"}},
		}}},
	}
	return append(slices.Concat(steps, more), harnesstest.Step{Name: "after", Reply: harnesstest.Reply{Text: "parent done"}, Repeat: true})
}

// busyParent runs busyParentSteps until the report has joined the parent's
// turn and the parent is idle.
var busyParent = []action{
	create{as: "a"},
	submit{as: "a", text: "delegate"},
	awaitRequests{n: 3},
	release{step: "child"},
	awaitRequests{n: 4},
	release{step: "ack"},
	waitIdle{as: "a"},
}

func TestContractChildReportPinned(t *testing.T) {
	summary := harnesstest.Step{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}}
	grand := harnesstest.ToolCall{ID: "toolu_grand", Name: "task", Input: spawn("general-purpose", "grand work")}
	runScenarios(t, []scenario{
		{
			name:       "child_report_to_a_busy_parent_keeps_its_slot_in_the_next_turn",
			concurrent: true,
			model:      busyParentSteps(),
			actions:    slices.Concat(busyParent, []action{submit{as: "a", text: "next"}, waitIdle{as: "a"}}),
		},
		{
			name:       "child_report_to_a_busy_parent_stays_through_a_compaction",
			concurrent: true,
			config:     map[string]any{"compaction_keep_turns": 1},
			model:      busyParentSteps(summary),
			actions: slices.Concat(busyParent, []action{
				submit{as: "a", text: "next"}, waitIdle{as: "a"},
				compact{as: "a"},
				submit{as: "a", text: "third"}, waitIdle{as: "a"},
			}),
		},
		{
			name:       "prompt_and_child_report_in_one_drain_are_two_messages",
			concurrent: true,
			model:      busyParentSteps(),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				release{step: "child"},
				awaitRequests{n: 4},
				enqueue{as: "a", text: "also this"},
				release{step: "ack"},
				waitIdle{as: "a"},
			},
		},
		{
			// The child spawns a grandchild whose report joins the busy child.
			// The log of the child that the root reads holds no segment.
			name:       "task_log_of_a_child_shows_no_pinned_report",
			concurrent: true,
			model: []harnesstest.Step{
				taskStep("delegate", userStarts("delegate"), fixed(spawn("general-purpose", "child work"))),
				{Name: "child", Match: userStarts("child work"), Reply: harnesstest.Reply{Text: "spawning", Block: true, ToolCalls: []harnesstest.ToolCall{grand}}},
				{Name: "ack", Match: matchAll(rootStarts("delegate"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Text: "waiting"}},
				{Name: "grand", Match: userStarts("grand work"), Reply: harnesstest.Reply{Text: "grand done", Block: true}},
				{Name: "kid_ack", Match: matchAll(rootStarts("child work"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_bash", Name: "bash", Input: map[string]any{"command": "echo busy; sleep 0.3"}},
				}}},
				taskStep("log", userStarts("log"), onKid(func(kid string) []map[string]any { return []map[string]any{onSession("log", kid)} })),
				{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				release{step: "child"},
				awaitRequests{n: 5},
				bindChild{as: "kid", parent: "a", record: true},
				bindChild{as: "grand", parent: "kid", record: true},
				release{step: "grand"},
				release{step: "kid_ack"},
				awaitRequests{n: 7},
				waitIdle{as: "a"},
				submit{as: "a", text: "log"},
				waitIdle{as: "a"},
			},
		},
	})
}
