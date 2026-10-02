package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractChildren(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:       "task_child_result_reaches_parent",
			concurrent: true,
			model: []harnesstest.Step{
				{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
					ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
				}}}},
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "child done", Block: true}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				{Name: "parent", Match: harnesstest.LastUserText("child done"), Reply: harnesstest.Reply{Text: "parent done"}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				// The child holds its reply until the parent's ack request exists, so the
				// result cannot ride on that request.
				awaitRequests{n: 3},
				release{step: "child"},
				awaitRequests{n: 4},
				waitIdle{as: "a"},
			},
		},
	})
}
