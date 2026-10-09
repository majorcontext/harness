package e2e

import (
	"slices"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// grandchildren is the model script of a root that spawns a child, which
// spawns a grandchild that keeps running. The child settles when the scenario
// releases the step child, and the root then reports and goes idle.
func grandchildren(more ...harnesstest.Step) []harnesstest.Step {
	grand := harnesstest.ToolCall{ID: "toolu_grand", Name: "task", Input: spawn("general-purpose", "grand work")}
	return delegation("general-purpose", harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{grand}},
		slices.Concat([]harnesstest.Step{
			{Name: "grand", Match: userStarts("grand work"), Reply: childRuns},
			{Name: "kid_ack", Match: matchAll(rootStarts("child work"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Text: "waiting"}},
		}, more)...)
}

// tree runs grandchildren to the point where the grandchild runs and the root
// is idle, and binds the child as kid and the grandchild as grand.
var tree = []action{
	create{as: "a"},
	submit{as: "a", text: "delegate"},
	awaitRequests{n: 3},
	release{step: "child"},
	awaitRequests{n: 6},
	waitIdle{as: "a"},
	bindChild{as: "kid", parent: "a", record: true},
	bindChild{as: "grand", parent: "kid", record: true},
}

func TestContractTaskTree(t *testing.T) {
	childrenOf := func(r harnesstest.Request) []map[string]any {
		return []map[string]any{onSession("status", resultID(r, childrenIDPattern))}
	}
	runScenarios(t, []scenario{
		{
			name:       "task_tree_reaches_a_grandchild",
			concurrent: true,
			model: grandchildren(
				taskStep("status", userStarts("status"), onKid(func(kid string) []map[string]any { return []map[string]any{onSession("status", kid)} })),
				taskStep("status_grand", lastResultHas(`"children":["ses_`), childrenOf)),
			actions: slices.Concat(tree, []action{
				submit{as: "a", text: "status"},
				waitIdle{as: "a"},
				cancelTree{as: "a"},
				waitIdle{as: "grand"},
			}),
		},
		{
			name:       "task_cancel_of_a_child_stops_the_grandchild",
			concurrent: true,
			model: grandchildren(
				busyTaskStep("cancel", userStarts("cancel"), onKid(func(kid string) []map[string]any { return []map[string]any{onSession("cancel", kid)} }))),
			actions: slices.Concat(tree, []action{
				submit{as: "a", text: "cancel"},
				waitIdle{as: "a"},
				waitIdle{as: "grand"},
			}),
		},
		{
			name:       "task_cancel_of_a_grandchild_reports_to_the_nearest_live_ancestor",
			concurrent: true,
			model: grandchildren(
				taskStep("look", userStarts("cancel grand"), onKid(func(kid string) []map[string]any { return []map[string]any{onSession("status", kid)} })),
				busyTaskStep("cancel_grand", lastResultHas(`"children":["ses_`), func(r harnesstest.Request) []map[string]any {
					return []map[string]any{onSession("cancel", resultID(r, childrenIDPattern))}
				})),
			actions: slices.Concat(tree, []action{
				submit{as: "a", text: "cancel grand"},
				waitIdle{as: "a"},
				waitIdle{as: "grand"},
			}),
		},
		{
			name:       "task_tree_interrupt_stops_the_grandchild",
			concurrent: true,
			model:      grandchildren(),
			actions:    slices.Concat(tree, []action{cancelTree{as: "a"}, waitIdle{as: "grand"}, getSession{as: "kid"}, getSession{as: "grand"}}),
		},
	})
}
