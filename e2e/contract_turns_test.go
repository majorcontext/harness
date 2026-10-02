package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractTurns(t *testing.T) {
	bash := func(id, cmd string) harnesstest.ToolCall {
		return harnesstest.ToolCall{ID: id, Name: "bash", Input: map[string]any{"command": cmd}}
	}
	text := func(s string) harnesstest.Reply { return harnesstest.Reply{Text: s} }
	oneTurn := []action{create{as: "a"}, submit{as: "a", text: "run"}, waitIdle{as: "a"}}

	runScenarios(t, []scenario{
		{
			name:    "text_reply",
			model:   []harnesstest.Step{{Name: "reply", Reply: text("hi")}},
			actions: []action{create{as: "a"}, submit{as: "a", text: "hello"}, waitIdle{as: "a"}},
		},
		{
			name: "one_tool_round_trip",
			model: []harnesstest.Step{
				{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{bash("toolu_1", "echo contract")}}},
				{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: text("ran")},
			},
			actions: oneTurn,
		},
		{
			name: "two_tool_calls_one_turn",
			model: []harnesstest.Step{
				{Name: "calls", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{bash("toolu_1", "echo a"), bash("toolu_2", "echo b")}}},
				{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: text("both")},
			},
			actions: oneTurn,
		},
		{
			name: "tool_error_reaches_model",
			model: []harnesstest.Step{
				{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "toolu_1", Name: "read_file", Input: map[string]any{"path": "/nonexistent-contract-dir/missing.txt"}}}}},
				{Name: "after", Match: harnesstest.LastToolResult("read_file"), Reply: text("missing")},
			},
			actions: oneTurn,
		},
		{
			name: "two_turns_keep_history",
			model: []harnesstest.Step{
				{Name: "one", Match: harnesstest.LastUserText("one"), Reply: text("1")},
				{Name: "two", Match: harnesstest.LastUserText("two"), Reply: text("2")},
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "one"}, waitIdle{as: "a"}, submit{as: "a", text: "two"}, waitIdle{as: "a"}},
		},
	})
}
