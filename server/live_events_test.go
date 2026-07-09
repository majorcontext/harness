package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestLiveEventsTextReasoningToolStartToolEnd drives a real Prompt through a
// scripted provider whose first turn emits a text delta, a reasoning delta,
// another text delta, and then a tool_use stop with a bash tool call, and
// whose second turn finishes the reply after the tool result comes back.
// Before this test, Server.Publish's EventTextDelta / EventReasoningDelta /
// EventToolStart / EventToolEnd branches were 0-hit: nothing drove those
// engine events through Publish into the SSE fan-out. It asserts an SSE
// subscriber receives all four live event kinds, in order, with the right
// payloads, and that they are live (Seq == 0) rather than durable.
func TestLiveEventsTextReasoningToolStartToolEnd(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		{
			{Type: provider.EventTextDelta, Text: "Thinking"},
			{Type: provider.EventReasoningDelta, Text: "because reasons"},
			{Type: provider.EventTextDelta, Text: " it through"},
			{
				Type: provider.EventDone,
				Message: &message.Message{
					ID:   "m_tool",
					Role: message.RoleAssistant,
					Parts: message.Parts{
						&message.Text{Text: "Thinking it through"},
						&message.ToolCall{CallID: "call_1", Name: "bash", Arguments: json.RawMessage(`{"command":"echo live-events"}`)},
					},
				},
				StopReason: provider.StopToolUse,
			},
		},
		{
			{Type: provider.EventTextDelta, Text: "done"},
			{
				Type:       provider.EventDone,
				Message:    &message.Message{ID: "m_final", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "done"}}},
				StopReason: provider.StopEndTurn,
			},
		},
	}}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	evs := sse.collectUntilIdle(t)

	var live []Event
	for _, ev := range evs {
		switch ev.Type {
		case engine.EventTextDelta, engine.EventReasoningDelta, engine.EventToolStart, engine.EventToolEnd:
			live = append(live, ev)
		}
	}

	want := []string{
		engine.EventTextDelta,
		engine.EventReasoningDelta,
		engine.EventTextDelta,
		engine.EventToolStart,
		engine.EventToolEnd,
		engine.EventTextDelta,
	}
	if len(live) != len(want) {
		t.Fatalf("live events = %d, want %d: %+v", len(live), len(want), live)
	}
	for i, typ := range want {
		if live[i].Type != typ {
			t.Fatalf("live[%d].Type = %q, want %q (all: %+v)", i, live[i].Type, typ, live)
		}
		if live[i].Seq != 0 {
			t.Errorf("live[%d] (%s) has a seq (%d); live events must not be durable", i, typ, live[i].Seq)
		}
		if live[i].SessionID != id {
			t.Errorf("live[%d].SessionID = %q, want %q", i, live[i].SessionID, id)
		}
	}

	if live[0].Text != "Thinking" {
		t.Errorf("first text.delta = %q", live[0].Text)
	}
	if live[1].Text != "because reasons" {
		t.Errorf("reasoning.delta = %q", live[1].Text)
	}
	if live[2].Text != " it through" {
		t.Errorf("second text.delta = %q", live[2].Text)
	}

	toolStart, toolEnd := live[3], live[4]
	if toolStart.ToolCall == nil || toolStart.ToolCall.Name != "bash" || toolStart.ToolCall.CallID != "call_1" {
		t.Errorf("tool.start ToolCall = %+v", toolStart.ToolCall)
	}
	if toolEnd.ToolCall == nil || toolEnd.ToolCall.CallID != "call_1" {
		t.Errorf("tool.end ToolCall = %+v", toolEnd.ToolCall)
	}
	if toolEnd.IsError {
		t.Errorf("tool.end IsError = true, want false for a successful echo")
	}
	if got := toolEnd.Output.Text(); !strings.Contains(got, "live-events") {
		t.Errorf("tool.end Output = %q, want it to contain the echoed text", got)
	}
	if live[5].Text != "done" {
		t.Errorf("final text.delta = %q", live[5].Text)
	}
}
