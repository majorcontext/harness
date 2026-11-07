package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

var stepBoundaryRows = map[string]runtimeRow{
	"tool_block_missing_id":              sameAsServe(),
	"tool_block_missing_name":            sameAsServe(),
	"tool_block_invalid_input":           sameAsServe(),
	"tool_block_text_then_tool_sse":      sameAsServe(),
	"tool_block_two_tools_sse":           sameAsServe(),
	"tool_block_tool_then_text_sse":      sameAsServe(),
	"tool_block_interleaved_sse":         sameAsServe(),
	"tool_block_text_then_tool":          runtimeOnly("embedded-subscriber-held"),
	"tool_block_two_tools":               runtimeOnly("embedded-subscriber-held"),
	"step_boundaries_text_and_next_turn": sameAsServe(),
	"step_boundaries_tool_only":          sameAsServe(),
	"step_boundaries_mixed":              sameAsServe(),
	"step_boundaries_retry":              sameAsServe(),
	"step_boundaries_pause":              sameAsServe(),
	"step_boundaries_failure":            sameAsServe(),
	"step_boundaries_cut_failure":        sameAsServe(),
	"step_boundaries_refusal_failure":    sameAsServe(),
	"step_boundaries_cut_then_failure":   sameAsServe(),
	"step_boundaries_prompt_stream":      runtimeOnly("embedded-subscriber-held"),
	"step_boundaries_overflow":           sameAsServe(),
	"step_boundaries_pending_question":   sameAsServe(),
	"step_boundaries_crash_recovery":     sameAsServe(),
	"step_boundaries_handoff_resume":     sameAsServe(),
}

type checkStepBoundaries struct{ want []string }

func (a checkStepBoundaries) run(t *testing.T, r *run) {
	t.Helper()
	page := r.drv.JournalPage(t, r.id(t, "a"), 0, 1000)
	var events []protocol.Event
	b, err := json.Marshal(page.Body.(map[string]any)["events"])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &events); err != nil {
		t.Fatal(err)
	}
	var got []string
	turn := ""
	open := false
	index := 0
	for _, e := range events {
		switch e.Kind {
		case "turn.started":
			var d struct {
				TurnID string `json:"turn_id"`
			}
			_ = json.Unmarshal(e.Data, &d)
			turn, index = d.TurnID, 0
		case "step.started", "step.ended":
			var d struct {
				TurnID       string `json:"turn_id"`
				Index        *int   `json:"index"`
				FinishReason string `json:"finish_reason"`
			}
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			if e.Ephemeral || d.TurnID != turn || d.Index == nil || *d.Index != index {
				t.Errorf("invalid %s: %+v (turn %q index %d)", e.Kind, d, turn, index)
			}
			if e.Kind == "step.started" {
				if open {
					t.Error("step started before prior step ended")
				}
				open = true
				got = append(got, fmt.Sprintf("start:%d", index))
			} else {
				if !open {
					t.Error("step ended without start")
				}
				open = false
				got = append(got, fmt.Sprintf("end:%d:%s", index, d.FinishReason))
				index++
			}
		case "item.completed":
			var d protocol.ItemCompleted
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			if d.Message.Role == "assistant" && len(d.Message.Parts) == 1 && d.Message.Parts[0].Text == "[harness: this turn was interrupted by a process restart and could not complete]" {
				continue
			}
			if d.Message.Role == "assistant" || d.Message.Role == "tool" {
				if !open {
					t.Errorf("%s item outside step", d.Message.Role)
				}
				got = append(got, string(d.Message.Role))
			}
		case "turn.ended":
			if open {
				t.Error("turn ended with open step")
			}
		}
	}
	if !slices.Equal(got, a.want) {
		t.Errorf("step boundaries = %q, want %q", got, a.want)
	}
}

func TestContractStepBoundaries(t *testing.T) {
	tool := harnesstest.ToolCall{ID: "toolu_1", Name: "no_such_tool", Input: map[string]any{}}
	answer := harnesstest.Step{Name: "answer", Reply: harnesstest.Reply{Text: "done"}}
	rows := []scenario{
		{name: "step_boundaries_text_and_next_turn", model: []harnesstest.Step{{Name: "first", Reply: harnesstest.Reply{Text: "first"}}, answer}, actions: []action{create{as: "a"}, submit{as: "a", text: "one"}, waitIdle{as: "a"}, submit{as: "a", text: "two"}, waitIdle{as: "a"}, checkStepBoundaries{[]string{"start:0", "assistant", "end:0:end_turn", "start:0", "assistant", "end:0:end_turn"}}}},
		{name: "step_boundaries_tool_only", model: []harnesstest.Step{{Name: "tool", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{tool}, StopReason: "tool_use"}}, answer}, actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, checkStepBoundaries{[]string{"start:0", "assistant", "tool", "end:0:tool_use", "start:1", "assistant", "end:1:end_turn"}}}},
		{name: "step_boundaries_mixed", model: []harnesstest.Step{{Name: "mixed", Reply: harnesstest.Reply{Text: "calling", ToolCalls: []harnesstest.ToolCall{tool}, StopReason: "tool_use"}}, answer}, actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, checkStepBoundaries{[]string{"start:0", "assistant", "tool", "end:0:tool_use", "start:1", "assistant", "end:1:end_turn"}}}},
		{name: "step_boundaries_retry", config: map[string]any{"stream_max_retries": 1}, model: []harnesstest.Step{{Name: "cut", Reply: harnesstest.Reply{Text: "partial", StopReason: "max_tokens"}}, answer}, actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, checkStepBoundaries{[]string{"start:0", "assistant", "end:0:end_turn"}}}},
		{name: "step_boundaries_pause", config: searchOn, model: []harnesstest.Step{{Name: "pause", Reply: pauseLeg("srvtoolu_1", false)}, answer}, actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, checkStepBoundaries{[]string{"start:0", "assistant", "end:0:end_turn"}}}},
		{name: "step_boundaries_cut_failure", config: map[string]any{"stream_max_retries": 0}, model: []harnesstest.Step{{Name: "cut", Reply: harnesstest.Reply{Text: "partial", StopReason: "max_tokens"}}}, actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, checkStepBoundaries{[]string{"start:0", "end:0:max_tokens"}}}},
		{name: "step_boundaries_refusal_failure", config: map[string]any{"stream_max_retries": 0}, model: []harnesstest.Step{{Name: "refusal", Reply: harnesstest.Reply{Text: "no", StopReason: "refusal"}}}, actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, checkStepBoundaries{[]string{"start:0", "end:0:refusal"}}}},
		{name: "step_boundaries_cut_then_failure", config: map[string]any{"stream_max_retries": 1}, model: []harnesstest.Step{{Name: "cut", Reply: harnesstest.Reply{Text: "partial", StopReason: "max_tokens"}}, {Name: "failure", Reply: harnesstest.Reply{HTTPStatus: 400}}}, actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, checkStepBoundaries{[]string{"start:0", "end:0:"}}}},
		{name: "step_boundaries_failure", model: []harnesstest.Step{{Name: "failure", Reply: harnesstest.Reply{HTTPStatus: 400}}}, actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, checkStepBoundaries{[]string{"start:0", "end:0:"}}}},
		{name: "step_boundaries_pending_question", driver: askLane(true), model: askSteps(), actions: []action{create{as: "a"}, submit{as: "a", text: "pick a db"}, waitIdle{as: "a"}, checkStepBoundaries{[]string{"start:0", "assistant", "end:0:tool_use"}}, answerQuestion{as: "a", callID: "toolu_ask", answers: map[string]string{"Which database?": "SQLite"}}, waitIdle{as: "a"}}},
		{name: "step_boundaries_crash_recovery", model: []harnesstest.Step{{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "toolu_slow", Name: "bash", Input: map[string]any{"command": "touch tool-running; sleep 30"}}}}}}, actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, awaitFile{path: "tool-running"}, awaitCall{as: "a", callID: "toolu_slow"}, restart{kill: true}, waitIdle{as: "a"}, checkStepBoundaries{[]string{"start:0", "assistant", "tool", "end:0:"}}}},
		{name: "step_boundaries_handoff_resume", model: []harnesstest.Step{
			{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "toolu_1", Name: "bash", Input: map[string]any{"command": "echo tool"}}}}},
			{Name: "after", Match: harnesstest.LastToolResult("bash"), Repeat: true, Reply: harnesstest.Reply{Text: "done", Block: true}},
		}, actions: []action{create{as: "a"}, submit{as: "a", text: "run"}, awaitRequests{n: 2}, restart{}, release{step: "after"}, waitIdle{as: "a"}, checkStepBoundaries{[]string{"start:0", "assistant", "tool", "end:0:tool_use", "start:1", "end:1:", "start:2", "assistant", "end:2:end_turn"}}}},
		{name: "step_boundaries_prompt_stream", model: []harnesstest.Step{{Name: "tool", Reply: harnesstest.Reply{Text: "calling", ToolCalls: []harnesstest.ToolCall{tool}, StopReason: "tool_use"}}, answer}, actions: []action{create{as: "a"}, stepBoundaryStream{}}},
		{name: "step_boundaries_overflow", config: map[string]any{"compaction_keep_turns": 1}, model: []harnesstest.Step{
			{Name: "one", Match: harnesstest.LastUserText("one"), Reply: harnesstest.Reply{Text: "first"}},
			{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: "second"}},
			{Name: "overflow", Match: harnesstest.LastUserText("three"), Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: harnesstest.ContextOverflowMessage}},
			{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}},
			{Name: "three", Match: harnesstest.LastUserText("three"), Reply: harnesstest.Reply{Text: "done"}},
		}, actions: []action{create{as: "a"}, submit{as: "a", text: "one"}, waitIdle{as: "a"}, submit{as: "a", text: "two"}, waitIdle{as: "a"}, submit{as: "a", text: "three"}, waitIdle{as: "a"}, checkStepBoundaries{[]string{"start:0", "assistant", "end:0:end_turn", "start:0", "assistant", "end:0:end_turn", "start:0", "assistant", "end:0:end_turn"}}}},
	}
	runScenarios(t, rows)
}
