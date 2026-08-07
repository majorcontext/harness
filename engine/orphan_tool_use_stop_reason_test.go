package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestNonToolUseStopWithToolCallsAppendsSyntheticResult reproduces incident
// NEP-5272: a provider completes a turn NORMALLY (EventDone arrives, no
// stream error at all) but reports a stop reason other than StopToolUse
// while the assistant message nonetheless carries one or more ToolCall
// parts. This is exactly the bifrost/Bedrock wire shape captured from box
// hyper-lemon (session ses_01kze9vds5fxd89dtv4accqjcp): 44 tool calls, 43
// results, the orphaned call at wire index 91 immediately followed by a
// plain user message.
//
// Before the fix, Session.Prompt's `if stop != provider.StopToolUse {
// return asst, nil }` early return left that ToolCall in history with no
// following tool_result. Every later request replays it and Anthropic 400s
// with "tool_use ids were found without tool_result blocks immediately
// after" — permanently, since nothing in this session's own history ever
// removes it.
func TestNonToolUseStopWithToolCallsAppendsSyntheticResult(t *testing.T) {
	orphaned := toolCall("toolu_bdrk_01Rot", "bash", `{"command":"echo hi"}`)
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, orphaned),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "recovered"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})

	asst, err := s.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("first Prompt = %v, want success (the provider reported a normal end-of-turn stop)", err)
	}
	if asst == nil {
		t.Fatal("first Prompt returned nil message")
	}

	h := s.History()
	if len(h) != 3 {
		t.Fatalf("history len = %d, want 3 (user, assistant(tool_call), synthetic tool result): %+v", len(h), h)
	}
	if h[1].Role != message.RoleAssistant {
		t.Fatalf("h[1].Role = %s, want assistant", h[1].Role)
	}
	if h[2].Role != message.RoleTool {
		t.Fatalf("h[2].Role = %s, want tool (the synthetic result) -- this is the orphan: a tool_use with nothing after it", h[2].Role)
	}
	if len(h) > 2 {
		if len(h[2].Parts) != 1 {
			t.Fatalf("synthetic tool message parts = %d, want 1", len(h[2].Parts))
		}
		tr, ok := h[2].Parts[0].(*message.ToolResult)
		if !ok {
			t.Fatalf("h[2].Parts[0] = %T, want *message.ToolResult", h[2].Parts[0])
		}
		if tr.CallID != "toolu_bdrk_01Rot" {
			t.Errorf("synthetic ToolResult.CallID = %q, want %q", tr.CallID, "toolu_bdrk_01Rot")
		}
		if !tr.IsError {
			t.Error("synthetic ToolResult.IsError = false, want true")
		}
	}

	// toolExecCount must NOT have moved: the orphaned call never executed
	// (a non-tool_use stop reason must never trigger execution -- a
	// max_tokens stop can truncate ToolCall.Arguments, and executing
	// truncated arguments is dangerous).
	if got := s.toolExecutions(); got != 0 {
		t.Errorf("toolExecutions() = %d, want 0 (unexecuted call must never be executed)", got)
	}

	// The whole point: history must stay marshalable and the NEXT request
	// build must pair the tool_use with its result so the session recovers
	// instead of dying identically on every later request.
	if _, err := json.Marshal(h); err != nil {
		t.Fatalf("json.Marshal(History()) = %v, want success", err)
	}

	final, err := s.Prompt(context.Background(), "continue")
	if err != nil {
		t.Fatalf("second Prompt (subsequent turn) = %v, want success", err)
	}
	if final.Parts.Text() != "recovered" {
		t.Errorf("second Prompt final = %q, want %q", final.Parts.Text(), "recovered")
	}
	if len(prov.requests) < 2 {
		t.Fatalf("provider recorded %d requests, want at least 2", len(prov.requests))
	}
	secondReqMessages := prov.requests[1].Messages
	if len(secondReqMessages) != 4 {
		t.Fatalf("second request history = %d messages, want 4 (user, assistant(tool_call), synthetic tool result, continue)", len(secondReqMessages))
	}
	if _, err := json.Marshal(secondReqMessages); err != nil {
		t.Fatalf("json.Marshal(second request's Messages) = %v, want success", err)
	}
}

// TestNonToolUseStopWithMultipleToolCallsAllGetSyntheticResults covers a
// turn that carries more than one ToolCall part alongside a non-tool_use
// stop reason: every one of them must get its own synthetic result, in
// order, none silently dropped.
func TestNonToolUseStopWithMultipleToolCallsAllGetSyntheticResults(t *testing.T) {
	tc1 := toolCall("tc1", "bash", `{"command":"echo a"}`)
	tc2 := toolCall("tc2", "read_file", `{"path":"x"}`)
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopMaxTokens, tc1, tc2),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt = %v, want success", err)
	}

	h := s.History()
	if len(h) != 3 || h[2].Role != message.RoleTool {
		t.Fatalf("history = %+v, want [user, assistant, tool]", h)
	}
	if len(h[2].Parts) != 2 {
		t.Fatalf("synthetic tool message parts = %d, want 2 (one per unexecuted call)", len(h[2].Parts))
	}
	gotIDs := map[string]bool{}
	for _, p := range h[2].Parts {
		tr, ok := p.(*message.ToolResult)
		if !ok {
			t.Fatalf("part = %T, want *message.ToolResult", p)
		}
		if !tr.IsError {
			t.Errorf("ToolResult for %s: IsError = false, want true", tr.CallID)
		}
		gotIDs[tr.CallID] = true
	}
	if !gotIDs["tc1"] || !gotIDs["tc2"] {
		t.Errorf("synthetic result call IDs = %v, want both tc1 and tc2", gotIDs)
	}
	if got := s.toolExecutions(); got != 0 {
		t.Errorf("toolExecutions() = %d, want 0", got)
	}
}

// TestNonToolUseStopWithoutToolCallsIsUnaffected proves the fix is scoped:
// an ordinary turn ending with a non-tool_use stop reason and NO ToolCall
// parts at all (the overwhelmingly common case) behaves exactly as before
// -- no synthetic message appended.
func TestNonToolUseStopWithoutToolCallsIsUnaffected(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "just text"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt = %v, want success", err)
	}
	h := s.History()
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2 (user, assistant): %+v", len(h), h)
	}
	if h[1].Role != message.RoleAssistant {
		t.Fatalf("h[1].Role = %s, want assistant", h[1].Role)
	}
}
