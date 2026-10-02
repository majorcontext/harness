package e2e

import (
	"testing"

	"github.com/majorcontext/harness/internal/fakemodel"
)

func TestContractTurns(t *testing.T) {
	bash := func(id, cmd string) fakemodel.ToolCall {
		return fakemodel.ToolCall{ID: id, Name: "bash", Input: map[string]any{"command": cmd}}
	}
	text := func(s string) fakemodel.Reply { return fakemodel.Reply{Text: s} }
	oneTurn := []action{create{as: "a"}, submit{as: "a", text: "run"}, waitIdle{as: "a"}}

	runScenarios(t, []scenario{
		{
			name:    "text_reply",
			model:   []fakemodel.Step{{Name: "reply", Reply: text("hi")}},
			actions: []action{create{as: "a"}, submit{as: "a", text: "hello"}, waitIdle{as: "a"}},
		},
		{
			name: "one_tool_round_trip",
			model: []fakemodel.Step{
				{Name: "call", Match: fakemodel.LastUserText("run"), Reply: fakemodel.Reply{ToolCalls: []fakemodel.ToolCall{bash("toolu_1", "echo contract")}}},
				{Name: "after", Match: fakemodel.LastToolResult("bash"), Reply: text("ran")},
			},
			actions: oneTurn,
		},
		{
			name: "two_tool_calls_one_turn",
			model: []fakemodel.Step{
				{Name: "calls", Match: fakemodel.LastUserText("run"), Reply: fakemodel.Reply{ToolCalls: []fakemodel.ToolCall{bash("toolu_1", "echo a"), bash("toolu_2", "echo b")}}},
				{Name: "after", Match: fakemodel.LastToolResult("bash"), Reply: text("both")},
			},
			actions: oneTurn,
		},
		{
			name: "tool_error_reaches_model",
			model: []fakemodel.Step{
				{Name: "call", Match: fakemodel.LastUserText("run"), Reply: fakemodel.Reply{ToolCalls: []fakemodel.ToolCall{{ID: "toolu_1", Name: "read_file", Input: map[string]any{"path": "/nonexistent-contract-dir/missing.txt"}}}}},
				{Name: "after", Match: fakemodel.LastToolResult("read_file"), Reply: text("missing")},
			},
			actions: oneTurn,
		},
		{
			name: "two_turns_keep_history",
			model: []fakemodel.Step{
				{Name: "one", Match: fakemodel.LastUserText("one"), Reply: text("1")},
				{Name: "two", Match: fakemodel.LastUserText("two"), Reply: text("2")},
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "one"}, waitIdle{as: "a"}, submit{as: "a", text: "two"}, waitIdle{as: "a"}},
		},
	})
}
