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

// TestLiveEventTurnRestartForwarded proves Server.Publish forwards the engine
// EventTurnRestart marker onto the live SSE stream. A base-loop retry
// (engine/prompt_retry.go) emits it so a client drops the failed attempt's
// partial deltas before the retry re-streams them. The mapping test drives
// Publish directly with the exact event the engine emits — a real retry
// backoff would add a wall-clock wait the SSE harness cannot fake — and
// asserts the subscriber receives one live (Seq 0) turn.restart.
//
// Red-verify: delete the engine.EventTurnRestart case in Server.Publish and
// the event is dropped (Publish has no default), so want == 0 and this fails.
func TestLiveEventTurnRestartForwarded(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	h.srv.Publish(engine.Event{Type: engine.EventTurnRestart, SessionID: id})

	ev := sse.waitFor(t, engine.EventTurnRestart)
	if ev.SessionID != id {
		t.Errorf("turn.restart SessionID = %q, want %q", ev.SessionID, id)
	}
	if ev.Seq != 0 {
		t.Errorf("turn.restart has a seq (%d); it must be live, never durable", ev.Seq)
	}
}

// TestHandleEventDeliversEventPublishedBeforeHeadersFlush is the
// regression test for a real, pre-existing (not introduced by this
// branch — confirmed via git diff and this exact test's own CI failure
// history on main before this branch existed) production bug that a
// live CI hang caught: TestLiveEventTurnRestartForwarded blocked the
// full 10-minute go test timeout, waiting on an event that had already
// been published and silently dropped.
//
// handleEvent used to write and flush the response headers BEFORE
// registering its subscriber in s.subs. A caller that published an
// event immediately upon seeing the connection succeed (this test's own
// h.srv.Publish call, made directly and synchronously right after
// openSSE returns — no HTTP round trip involved, since Publish is an
// in-process call) could race ahead of that registration: fanoutLocked's
// delivery is a non-blocking send with no subscriber there yet to
// receive it, so the event vanished, and anything later blocked
// waiting for it (an SSE reader's channel receive) hung forever. Rare
// under a fast, idle local run (the server goroutine's own few
// post-flush instructions almost always win against the client's
// network round trip) but real and reproducible under CI-runner load
// (a descheduling pause between flush and registration is far more
// likely when many parallel -race packages are contending for CPU).
//
// This test forces the (now-safe) gap deterministically via
// sseRegisteredRace, publishing the event from INSIDE the handler
// itself, between subscriber registration and the header flush —
// exactly the ordering the fix guarantees — and confirms the event is
// still delivered rather than dropped.
func TestHandleEventDeliversEventPublishedBeforeHeadersFlush(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	h.srv.sseRegisteredRace = func() {
		h.srv.Publish(engine.Event{Type: engine.EventTurnRestart, SessionID: id})
	}

	sse := h.openSSE("?from=0", "")
	ev := sse.waitFor(t, engine.EventTurnRestart)
	if ev.SessionID != id {
		t.Errorf("turn.restart SessionID = %q, want %q", ev.SessionID, id)
	}
}
