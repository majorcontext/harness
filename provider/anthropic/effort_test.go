package anthropic

import (
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestEffortEnablesThinking: a non-off effort level sets a thinking block with
// the mapped budget, bumps max_tokens above the budget, and drops temperature
// and top_p (the API rejects both while thinking is enabled).
func TestEffortEnablesThinking(t *testing.T) {
	temp := 0.7
	topP := 0.9
	cases := []struct {
		effort     message.Effort
		wantBudget int
		wantMax    int
	}{
		{message.EffortMinimal, 1024, 5120}, // 4096 < 1024+4096 -> 5120
		{message.EffortLow, 4096, 8192},     // 4096 < 4096+4096 -> 8192
		{message.EffortMedium, 8192, 12288}, // 8192+4096
		{message.EffortHigh, 16384, 20480},  // 16384+4096
	}
	for _, c := range cases {
		req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
		req.MaxTokens = 4096
		req.Temperature = &temp
		req.TopP = &topP
		req.Effort = c.effort
		out := mustTranscode(t, req)
		if out.Thinking == nil {
			t.Fatalf("effort %q: thinking block absent", c.effort)
		}
		if out.Thinking.Type != "enabled" {
			t.Errorf("effort %q: thinking type = %q, want enabled", c.effort, out.Thinking.Type)
		}
		if out.Thinking.BudgetTokens != c.wantBudget {
			t.Errorf("effort %q: budget = %d, want %d", c.effort, out.Thinking.BudgetTokens, c.wantBudget)
		}
		if out.MaxTokens != c.wantMax {
			t.Errorf("effort %q: max_tokens = %d, want %d", c.effort, out.MaxTokens, c.wantMax)
		}
		if out.MaxTokens <= out.Thinking.BudgetTokens {
			t.Errorf("effort %q: max_tokens %d must exceed budget %d", c.effort, out.MaxTokens, out.Thinking.BudgetTokens)
		}
		if out.Temperature != nil {
			t.Errorf("effort %q: temperature must be dropped with thinking on", c.effort)
		}
		if out.TopP != nil {
			t.Errorf("effort %q: top_p must be dropped with thinking on", c.effort)
		}
	}
}

// TestEffortMinimalBumpsMax: minimal budget 1024 with a small max_tokens still
// bumps max_tokens above the budget (assert the fix works at the floor too).
func TestEffortMinimalBumpsMax(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.MaxTokens = 1024
	req.Effort = message.EffortMinimal
	out := mustTranscode(t, req)
	if out.MaxTokens != 1024+thinkingCompletionMargin {
		t.Errorf("max_tokens = %d, want %d", out.MaxTokens, 1024+thinkingCompletionMargin)
	}
}

// TestEffortLargeMaxKept: a max_tokens already above budget+margin is kept, not
// shrunk.
func TestEffortLargeMaxKept(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.MaxTokens = 64000
	req.Effort = message.EffortHigh
	out := mustTranscode(t, req)
	if out.MaxTokens != 64000 {
		t.Errorf("max_tokens = %d, want 64000 (kept)", out.MaxTokens)
	}
}

// TestEffortOffAndUnsetNoThinking: EffortOff and EffortUnset emit no thinking
// block and leave temperature/top_p intact.
func TestEffortOffAndUnsetNoThinking(t *testing.T) {
	temp := 0.7
	for _, e := range []message.Effort{message.EffortUnset, message.EffortOff} {
		req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
		req.Temperature = &temp
		req.Effort = e
		out := mustTranscode(t, req)
		if out.Thinking != nil {
			t.Errorf("effort %q: thinking block must be absent", e)
		}
		if out.Temperature == nil {
			t.Errorf("effort %q: temperature must be preserved", e)
		}
	}
}
