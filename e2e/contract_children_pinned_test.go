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
				submit{as: "a", text: "next"},
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

func TestContractChildReportPinnedAcrossTurns(t *testing.T) {
	summary := harnesstest.Step{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}}
	unused := summary
	unused.Repeat = true
	hello := harnesstest.Step{Name: "hello", Match: harnesstest.LastUserText("hello"), Reply: harnesstest.Reply{Text: "hi"}}
	call := func(id, tool string, input map[string]any) harnesstest.Reply {
		return harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: id, Name: tool, Input: input}}}
	}
	keepOne := map[string]any{"compaction_keep_turns": 1}
	runScenarios(t, []scenario{
		{
			name:       "child_report_to_a_busy_parent_after_the_cut_of_a_compaction",
			concurrent: true,
			config:     keepOne,
			model:      busyParentSteps(hello, summary),
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "hello"}, waitIdle{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 4},
				release{step: "child"},
				awaitRequests{n: 5},
				release{step: "ack"},
				waitIdle{as: "a"},
				compact{as: "a"},
				submit{as: "a", text: "next"}, waitIdle{as: "a"},
			},
		},
		{
			name:       "child_report_to_a_busy_parent_folded_with_a_long_kept_tail",
			concurrent: true,
			config:     keepOne,
			model: busyParentSteps(summary,
				harnesstest.Step{Name: "long1", Match: harnesstest.LastUserText("long"), Reply: call("toolu_g", "glob", map[string]any{"pattern": "*.none"})},
				harnesstest.Step{Name: "long2", Match: harnesstest.LastToolResult("glob"), Reply: call("toolu_r", "grep", map[string]any{"pattern": "zzzz"})},
				harnesstest.Step{Name: "long3", Match: harnesstest.LastToolResult("grep"), Reply: harnesstest.Reply{Text: "long done"}},
			),
			actions: slices.Concat(busyParent, []action{
				submit{as: "a", text: "long"}, waitIdle{as: "a"},
				compact{as: "a"},
				submit{as: "a", text: "next"}, waitIdle{as: "a"},
			}),
		},
		{
			name:       "child_report_to_a_busy_parent_stays_through_an_in_turn_compaction",
			concurrent: true,
			config:     keepOne,
			model: busyParentSteps(unused,
				harnesstest.Step{Name: "big1", Match: harnesstest.LastUserText("big"), Reply: call("toolu_g", "glob", map[string]any{"pattern": "*.none"})},
				harnesstest.Step{Name: "overflow", Match: harnesstest.LastToolResult("glob"), Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: harnesstest.ContextOverflowMessage}},
				harnesstest.Step{Name: "big2", Match: harnesstest.LastToolResult("glob"), Reply: call("toolu_r", "grep", map[string]any{"pattern": "zzzz"})},
				harnesstest.Step{Name: "big3", Match: harnesstest.LastToolResult("grep"), Reply: harnesstest.Reply{Text: "big done"}},
			),
			actions: slices.Concat(busyParent, []action{
				submit{as: "a", text: "big"}, waitIdle{as: "a"},
				submit{as: "a", text: "next"}, waitIdle{as: "a"},
			}),
		},
		{
			name:       "child_report_to_a_busy_parent_survives_a_restart",
			concurrent: true,
			model:      busyParentSteps(),
			actions:    slices.Concat(busyParent, []action{restart{}, submit{as: "a", text: "next"}, waitIdle{as: "a"}}),
		},
	})
}
