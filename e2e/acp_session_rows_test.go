package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func acpRequests(t *testing.T, c *acpConn, n int) []harnesstest.Request {
	t.Helper()
	reqs := c.fake.Requests()
	if len(reqs) != n {
		t.Fatalf("model requests = %d, want %d", len(reqs), n)
	}
	return reqs
}

func acpRepeatText(text string) []harnesstest.Step {
	return []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: text}, Repeat: true}}
}

func acpBlockedText(text string) []harnesstest.Step {
	return []harnesstest.Step{{Name: "slow", Reply: harnesstest.Reply{Text: text, Block: true}}}
}

var acpSessionScenarios = []acpScenario{
	{
		name:  "prompt_streams_text_then_ends_the_turn",
		cite:  "`session/prompt` runs one turn and sends the text of the model as `agent_message_chunk` updates before it answers `end_turn`.",
		model: []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: "hello from the model"}}},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "hi", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if got := acpRequests(t, c, 1)[0].LastUserText(); !strings.HasPrefix(got, "hi") {
					t.Errorf("the model saw the prompt %q, want %q", got, "hi")
				}
			}),
		},
	},
	{
		name:  "cancel_ends_the_running_prompt_cancelled",
		cite:  "After `session/cancel`, the prompt that runs answers `cancelled`, after every update that it has pending.",
		model: acpBlockedText("partial"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPromptStart{as: "a", text: "go"},
			acpAwaitText{text: "partial"},
			acpCancel{as: "a"},
			acpResult{id: "a", stop: "cancelled"},
		},
	},
	{
		name:     "cancel_right_after_the_prompt_is_not_lost",
		cite:     "A `session/cancel` that follows a `session/prompt` on the wire reaches that prompt.",
		model:    []harnesstest.Step{{Name: "slow", Reply: harnesstest.Reply{Block: true}, Repeat: true}},
		wireOnly: true,
		steps: []acpStep{
			acpNew{as: "a"},
			acpPromptStart{as: "a", text: "go"},
			acpCancel{as: "a"},
			acpResult{id: "a", stop: "cancelled"},
		},
	},
	{
		name:  "failed_turn_answers_an_internal_error",
		cite:  "A turn that fails answers `-32603` with `data.code` `internal` and `data.error_class`; the message is the masked error of the turn.",
		model: []harnesstest.Step{{Name: "bad", Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: "scripted bad request"}}},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "go", code: acpInternal, class: "permanent", dataCode: "internal"},
		},
	},
	{
		name:  "cancel_request_interrupts_the_turn_and_answers_32800",
		cite:  "`$/cancel_request` for a prompt also interrupts its turn.",
		model: acpBlockedText("partial"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPromptStart{as: "a", text: "go"},
			acpAwaitText{text: "partial"},
			acpCancelRequest{id: "a"},
			acpResult{id: "a", code: acpRequestCanceled},
			acpCheck(func(t *testing.T, c *acpConn) {
				if !c.fake.AwaitCanceled("slow", waitBound) {
					t.Error("the model request stayed open after $/cancel_request")
				}
			}),
		},
	},
	{
		name: "goal_turn_without_a_prompt_sends_updates",
		cite: "A turn that starts with no `session/prompt` open, such as a goal turn, sends its updates as notifications.",
		model: append([]harnesstest.Step{evaluatorStep("judge", "MET: ok", true)}, append(goalChain(
			ftArgs("action", "set", "condition", "say done"),
		), agentStep("condition", "goal turn text", true))...),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "go", stop: "end_turn"},
			acpAwaitText{text: "goal turn text"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if !c.fake.AwaitRequests(4, waitBound) {
					t.Fatalf("model requests = %d, want 4: the turn, its answer, the goal turn, and the evaluator", len(c.fake.Requests()))
				}
			}),
		},
	},
	{
		name:  "ownership_lost_fails_a_later_prompt",
		cite:  "A prompt for a session that another owner took answers `-32603` with `data.code` `session_not_owned`.",
		model: acpRepeatText("x"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpTakeoverStep{as: "a"},
			acpPrompt{as: "a", text: "go", code: acpInternal, dataCode: "session_not_owned"},
		},
	},
	{
		name:  "ownership_lost_fails_the_open_prompt",
		cite:  "When another owner takes the session, the prompt that runs answers the same error.",
		model: acpBlockedText("partial"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPromptStart{as: "a", text: "go"},
			acpAwaitText{text: "partial"},
			acpTakeoverStep{as: "a"},
			acpCheck(func(t *testing.T, c *acpConn) { c.fake.Release("slow") }),
			acpResult{id: "a", code: acpInternal, dataCode: "session_not_owned"},
		},
	},
	{
		name:  "end_of_input_during_a_turn_ends_the_prompt",
		cite:  "At the end of the input, the agent releases its sessions first, so a prompt that runs answers `-32603` with `data.code` `session_not_owned`.",
		model: acpBlockedText("partial"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPromptStart{as: "a", text: "go"},
			acpAwaitText{text: "partial"},
			acpCloseInput{},
			acpResult{id: "a", code: acpInternal, dataCode: "session_not_owned"},
		},
	},
	{
		name:  "second_cwd_uses_its_own_project_config",
		cite:  "A `session/new` with another `cwd` runs on another runtime, which uses the project config of that `cwd`.",
		model: acpRepeatText("ok"),
		dirs:  map[string]map[string]string{"second": {".harness.json": `{"append_system_prompt":["SECOND-PROJECT-MARK"]}`}},
		steps: []acpStep{
			acpNew{as: "a"},
			acpNew{as: "b", cwd: "second"},
			acpPrompt{as: "a", text: "one", stop: "end_turn"},
			acpPrompt{as: "b", text: "two", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				reqs := acpRequests(t, c, 2)
				if strings.Contains(reqs[0].System, "SECOND-PROJECT-MARK") || !strings.Contains(reqs[1].System, "SECOND-PROJECT-MARK") {
					t.Errorf("the project config of the second cwd reached the wrong session\nfirst: %s\nsecond: %s", reqs[0].System, reqs[1].System)
				}
			}),
		},
	},
	{
		name: "mcp_server_in_session_new_reaches_the_model",
		cite: "The `mcpServers` of `session/new` are the MCP servers of that session only.",
		model: []harnesstest.Step{
			{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "mcp__weather__forecast", Input: map[string]any{"city": "Oslo"}}}}},
			{Name: "done", Reply: harnesstest.Reply{Text: "ok"}},
		},
		steps: []acpStep{
			acpNew{as: "a", weather: true},
			acpPrompt{as: "a", text: "forecast", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				reqs := acpRequests(t, c, 2)
				if !requestHas(reqs[0], "mcp__weather__forecast") {
					t.Errorf("tools of the first request = %v, want the session server tools", reqs[0].Tools)
				}
				if got := requestText(reqs[1]); !strings.Contains(got, "Oslo: 3C, snow") {
					t.Errorf("the second request does not hold the tool result: %s", got)
				}
			}),
		},
	},
	{
		name: "repeated_mcp_server_name_is_invalid",
		cite: "A name that two `mcpServers` entries share is `-32602`.",
		steps: []acpStep{
			acpNew{as: "a", code: acpInvalidParams, mcp: []map[string]any{
				{"name": "x", "command": "/bin/true", "args": []any{}, "env": []any{}},
				{"name": "x", "type": "http", "url": "https://example.com/mcp", "headers": []any{}},
			}},
		},
	},
	{
		name:    "default_tool_set_narrows_the_tools_of_a_session",
		cite:    "`Config.AllowedTools`, when set, is the `allowed_tools` of every session that `session/new` creates.",
		model:   []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: "ok"}}},
		allowed: []string{"read_file", "grep"},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "hi", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if got := acpRequests(t, c, 1)[0].Tools; !slices.Equal(got, []string{"grep", "read_file"}) {
					t.Errorf("tools of the request = %v, want grep and read_file", got)
				}
			}),
		},
	},
	{
		name:  "native_adapter_offers_every_tool",
		cite:  "`harness acp` leaves `Config.AllowedTools` unset, so a session has every tool.",
		model: []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: "ok"}}},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "hi", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if got := acpRequests(t, c, 1)[0]; !requestHas(got, "bash") || !requestHas(got, "read_file") {
					t.Errorf("tools of the request = %v, want every tool", got.Tools)
				}
			}),
		},
	},
	{
		name: "relative_cwd_is_invalid",
		cite: "A `cwd` that is not an absolute path of a directory is `-32602`.",
		steps: []acpStep{
			acpNew{as: "a", rawCWD: "relative/dir", code: acpInvalidParams},
			acpNew{as: "b", rawCWD: "/nonexistent/acp/dir", code: acpInvalidParams},
		},
	},
	{
		name: "prompt_for_an_unknown_session_is_not_found",
		cite: "A prompt for a session that the connection does not hold answers `-32002`.",
		steps: []acpStep{
			acpPromptStart{id: "x", sid: "ses_unknown", text: "hi"},
			acpResult{id: "x", code: acpSessionNotFound, dataCode: "session_not_found"},
		},
	},
	{
		name: "prompt_with_an_image_block_is_invalid",
		cite: "A prompt block other than `text` and `resource_link` is `-32602`.",
		steps: []acpStep{
			acpNew{as: "a"},
			acpPromptStart{as: "a", blocks: []any{map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"}}},
			acpResult{id: "a", code: acpInvalidParams},
		},
	},
}
