package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractChildren(t *testing.T) {
	delegate := harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
		ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
	}}}}
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
		{
			name:       "child_report_reaches_a_busy_parent_at_the_tool_boundary",
			concurrent: true,
			model: []harnesstest.Step{
				delegate,
				// The child holds its tool call until the parent's ack request exists, so
				// the child ends while the parent runs a tool.
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_2", Name: "ls", Input: map[string]any{"path": "."}},
				}}},
				{Name: "child_done", Match: harnesstest.LastToolResult("ls"), Reply: harnesstest.Reply{Text: "child done"}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_bash", Name: "bash", Input: map[string]any{"command": "sleep 0.3"}},
				}}},
				{Name: "after", Reply: harnesstest.Reply{Text: "parent done"}, Repeat: true},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				release{step: "child"},
				awaitRequests{n: 4},
				release{step: "ack"},
				waitIdle{as: "a"},
			},
		},
		{
			name:       "child_error_delivered",
			concurrent: true,
			model: []harnesstest.Step{
				delegate,
				// The child holds its tool call until the parent's ack request exists, so
				// the failure cannot ride on that request.
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{{
					ID: "toolu_2", Name: "ls", Input: map[string]any{"path": "."},
				}}}},
				{Name: "child_error", Match: harnesstest.LastToolResult("ls"), Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: "child request rejected"}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				{Name: "parent", Match: harnesstest.LastUserText("A background task"), Reply: harnesstest.Reply{Text: "parent done"}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				release{step: "child"},
				awaitRequests{n: 5},
				waitIdle{as: "a"},
				bindChild{as: "kid", parent: "a", record: true},
				getSession{as: "kid"},
				getSession{as: "a"},
			},
		},
		{
			name:       "child_usage_limit_delivered",
			concurrent: true,
			model: []harnesstest.Step{
				delegate,
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{{
					ID: "toolu_2", Name: "ls", Input: map[string]any{"path": "."},
				}}}},
				{Name: "child_wall", Match: harnesstest.LastToolResult("ls"), Reply: harnesstest.Reply{HTTPStatus: 429, ErrorMessage: usageLimitMessage}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				{Name: "parent", Match: harnesstest.LastUserText("A background task"), Reply: harnesstest.Reply{Text: "parent done"}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				release{step: "child"},
				awaitRequests{n: 5},
				waitIdle{as: "a"},
				bindChild{as: "kid", parent: "a", record: true},
				getSession{as: "kid"},
				getSession{as: "a"},
			},
		},
	})
}

func TestContractChildrenControl(t *testing.T) {
	delegate := harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
		ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
	}}}}
	runScenarios(t, []scenario{
		{
			// Known defect, pinned: after SIGKILL the running child reloads idle and the parent loses its children.
			name:       "child_crash_recovered",
			concurrent: true,
			model: []harnesstest.Step{
				delegate,
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "partial", Block: true}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				{Name: "rest", Reply: harnesstest.Reply{Text: "rest"}, Repeat: true},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				waitIdle{as: "a"},
				bindChild{as: "kid", parent: "a", record: true},
				getSession{as: "kid"},
				restart{kill: true},
				getSession{as: "kid"},
				waitIdle{as: "kid"},
				getSession{as: "a"},
			},
		},
		{
			// Known defect, pinned: cancel_tree marks an idle root canceled and drops the child's queued send.
			name:       "send_to_child_and_cancel_tree",
			concurrent: true,
			model: []harnesstest.Step{
				delegate,
				{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "partial", Block: true}},
				{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				{Name: "rest", Reply: harnesstest.Reply{Text: "rest"}, Repeat: true},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "delegate"},
				awaitRequests{n: 3},
				waitIdle{as: "a"},
				bindChild{as: "kid", parent: "a", record: true},
				sendToSession{as: "kid", text: "more"},
				cancelTree{as: "a"},
				waitIdle{as: "kid"},
				getSession{as: "kid"},
				getSession{as: "a"},
				sendToSession{as: "kid", text: "again"},
				waitIdle{as: "kid"},
				getSession{as: "kid"},
			},
		},
	})
}
