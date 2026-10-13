package e2e

import (
	"github.com/majorcontext/harness/harnesstest"
)

func acpToolCall(name string, in map[string]any) []harnesstest.Step {
	return []harnesstest.Step{
		{Name: "call", Match: assistantTurns(0), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "toolu_1", Name: name, Input: in}}}},
		{Name: "done", Match: assistantTurns(1), Reply: harnesstest.Reply{Text: "done"}},
	}
}

var acpToolScenarios = []acpScenario{
	{
		name:  "tool_call_reports_kind_location_and_result",
		cite:  "A tool call of the model is a `tool_call` update.",
		model: acpToolCall("read_file", map[string]any{"path": "a.txt", "offset": 2}),
		files: map[string]string{"a.txt": "one\ntwo\nthree\n"},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "read it", stop: "end_turn"},
		},
	},
	{
		name:  "tool_kind_comes_from_the_tool_name",
		cite:  "The `kind` of a tool comes from a table of tool names:",
		model: acpToolCall("bash", map[string]any{"command": "echo hi"}),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "run it", stop: "end_turn"},
		},
	},
	{
		name:  "file_tool_adds_the_absolute_path_as_a_location",
		cite:  "A file tool adds `locations` to its `tool_call`:",
		model: acpToolCall("write_file", map[string]any{"path": "sub/new.txt", "content": "hello\n"}),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "write it", stop: "end_turn"},
		},
	},
	{
		name:  "edit_file_reports_a_diff",
		cite:  "An `edit_file` call adds one `diff` content item",
		model: acpToolCall("edit_file", map[string]any{"path": "a.txt", "old_string": "two", "new_string": "2", "replace_all": true}),
		files: map[string]string{"a.txt": "one\ntwo\nthree\n"},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "edit it", stop: "end_turn"},
		},
	},
	{
		name:  "write_file_reports_a_diff_with_no_old_text",
		cite:  "A `write_file` call adds a `diff` with `oldText` null",
		model: acpToolCall("write_file", map[string]any{"path": "b.txt", "content": "fresh\n"}),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "write it", stop: "end_turn"},
		},
	},
	{
		name:  "failed_tool_result_reports_failed_with_its_text",
		cite:  "Its status is `completed`, or `failed` when the result is an error.",
		model: acpToolCall("read_file", map[string]any{"path": "missing.txt"}),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "read it", stop: "end_turn"},
		},
	},
	{
		name:  "tool_result_image_is_image_content",
		cite:  "Each image of the result follows as an `image` content item.",
		model: acpToolCall("read_file", map[string]any{"path": "a.png"}),
		files: map[string]string{"a.png": string(rowAttachments()[0].data)},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "look", stop: "end_turn"},
		},
	},
	{
		name: "cancel_during_a_tool_fails_the_call_before_the_answer",
		cite: "A tool call that the cancel of a turn cut short has an error result in the log, so its update is `failed`, and it reaches the client before the `cancelled` answer.",
		model: []harnesstest.Step{
			{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "toolu_1", Name: "bash", Input: map[string]any{"command": "sleep 300"}}}}},
		},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPromptStart{as: "a", text: "go"},
			acpAwaitUpdate{kind: "tool_call", status: "in_progress"},
			acpCancel{as: "a"},
			acpResult{id: "a", stop: "cancelled"},
		},
	},
}
