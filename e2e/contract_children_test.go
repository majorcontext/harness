package e2e

import (
	"testing"

	"github.com/majorcontext/harness/internal/fakemodel"
)

func TestContractChildren(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "task_child_result_reaches_parent",
			model: []fakemodel.Step{
				{Name: "delegate", Match: fakemodel.LastUserText("delegate"), Reply: fakemodel.Reply{ToolCalls: []fakemodel.ToolCall{{
					ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
				}}}},
				{Name: "child", Match: fakemodel.LastUserText("child work"), Reply: fakemodel.Reply{Text: "child done"}},
				{Name: "ack", Match: fakemodel.LastToolResult("task"), Reply: fakemodel.Reply{Text: "waiting"}},
				{Name: "parent", Match: fakemodel.LastUserText("child done"), Reply: fakemodel.Reply{Text: "parent done"}},
			},
			actions: []action{create{"a"}, submit{"a", "delegate"}, awaitRequests{n: 4}, waitIdle{"a"}},
		},
	})
}
