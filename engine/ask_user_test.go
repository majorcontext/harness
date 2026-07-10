package engine

import (
	"context"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestAskUserResolvesSynchronouslyAndEndsTurn is the red-first test for
// docs/design/question-tool.md §1-§2: ask_user is a built-in tool that
// resolves the tool call with a real, non-error result the instant it runs,
// and its side effect is to end the current turn — regardless of the
// model's stop reason — so a second model call the provider script has
// queued must never happen.
func TestAskUserResolvesSynchronouslyAndEndsTurn(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse,
			toolCall("tc1", "ask_user", `{"questions":[{"question":"Which environment?","options":["staging","prod"]}]}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "must never run"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})

	final, err := s.Prompt(context.Background(), "please ask")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if final == nil {
		t.Fatal("final message is nil")
	}
	if prov.call != 1 {
		t.Fatalf("provider called %d times, want exactly 1 (turn must end at ask_user)", prov.call)
	}

	h := s.History()
	if len(h) != 3 {
		t.Fatalf("history len = %d, want 3 (user, assistant tool_call, tool result): %+v", len(h), h)
	}
	if h[2].Role != message.RoleTool {
		t.Fatalf("h[2].Role = %s, want tool", h[2].Role)
	}
	tr, ok := h[2].Parts[0].(*message.ToolResult)
	if !ok {
		t.Fatalf("h[2].Parts[0] = %+v, want *message.ToolResult", h[2].Parts[0])
	}
	if tr.IsError {
		t.Error("ask_user tool result IsError = true, want false — always fully resolved (design doc §2)")
	}
	if tr.CallID != "tc1" {
		t.Errorf("tr.CallID = %q, want tc1", tr.CallID)
	}
	want := "1 question(s) recorded (call tc1); waiting for the user's answer as the next prompt."
	if got := tr.Content.Text(); got != want {
		t.Errorf("tr.Content.Text() = %q, want %q", got, want)
	}
}

// TestAskUserSetsAwaitingQuestion proves s.awaitingQuestion is set the
// instant ask_user runs, exposed via AwaitingQuestion (mirroring ActiveGoal).
func TestAskUserSetsAwaitingQuestion(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse,
			toolCall("tc1", "ask_user", `{"questions":[{"question":"Which environment?"}]}`)),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	if _, err := s.Prompt(context.Background(), "ask"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	callID, ok := s.AwaitingQuestion()
	if !ok || callID != "tc1" {
		t.Fatalf("AwaitingQuestion() = (%q, %v), want (tc1, true)", callID, ok)
	}
}

// TestAskUserPersistsJournalRecord proves question.asked is durably
// journaled, keyed on the tool call's CallID, mirroring recGoalSet.
func TestAskUserPersistsJournalRecord(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse,
			toolCall("tc1", "ask_user", `{"questions":[{"question":"Which environment?"}]}`)),
	}}
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	})
	if _, err := s.Prompt(context.Background(), "ask"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	reloaded, err := LoadSession(Config{Providers: provider.Registry{"test": prov}, SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	callID, ok := reloaded.AwaitingQuestion()
	if !ok || callID != "tc1" {
		t.Fatalf("reloaded AwaitingQuestion() = (%q, %v), want (tc1, true) — question.asked must be durable", callID, ok)
	}
}

// TestAskUserBatchCountsAllQuestions proves a batch call records every
// question, not just the first, and the tool result's wording pluralizes.
func TestAskUserBatchCountsAllQuestions(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse,
			toolCall("tc1", "ask_user", `{"questions":[{"question":"Q1"},{"question":"Q2","options":["a","b"],"multi":true}]}`)),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	if _, err := s.Prompt(context.Background(), "ask"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	h := s.History()
	tr := h[2].Parts[0].(*message.ToolResult)
	want := "2 question(s) recorded (call tc1); waiting for the user's answer as the next prompt."
	if got := tr.Content.Text(); got != want {
		t.Errorf("tr.Content.Text() = %q, want %q", got, want)
	}
}

// TestAskUserOtherToolCallsInSameRoundStillExecute proves that ask_user only
// skips the loop-continuation — other tool calls batched into the same
// assistant message still execute and get real results first (design doc
// §2: "Other tool calls batched into the same assistant message still
// execute and get real results first; only the loop-continuation is
// skipped.").
func TestAskUserOtherToolCallsInSameRoundStillExecute(t *testing.T) {
	msg := &message.Message{
		ID:   "msg_a",
		Role: message.RoleAssistant,
		Parts: message.Parts{
			toolCall("tc0", "read_file", `{"path":"/nonexistent-xyz"}`),
			toolCall("tc1", "ask_user", `{"questions":[{"question":"ok?"}]}`),
		},
	}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		{{Type: provider.EventDone, Message: msg, StopReason: provider.StopToolUse}},
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	h := s.History()
	toolMsg := h[2]
	if len(toolMsg.Parts) != 2 {
		t.Fatalf("tool result message parts = %d, want 2", len(toolMsg.Parts))
	}
	r0 := toolMsg.Parts[0].(*message.ToolResult)
	if r0.CallID != "tc0" || !r0.IsError {
		t.Errorf("read_file result = %+v, want an error result for tc0 (nonexistent file)", r0)
	}
	r1 := toolMsg.Parts[1].(*message.ToolResult)
	if r1.CallID != "tc1" || r1.IsError {
		t.Errorf("ask_user result = %+v, want a resolved (non-error) result for tc1", r1)
	}
}
