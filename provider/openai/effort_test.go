package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestEffortSetsReasoning: a non-off effort level sets reasoning.effort to the
// same string, drops temperature/top_p (reasoning models reject them), and
// raises max_output_tokens above the floor (reasoning tokens count against it).
func TestEffortSetsReasoning(t *testing.T) {
	temp := 0.7
	topP := 0.9
	for _, e := range []message.Effort{message.EffortMinimal, message.EffortLow, message.EffortMedium, message.EffortHigh} {
		req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
		req.Temperature = &temp
		req.TopP = &topP
		req.MaxTokens = 8192
		req.Effort = e
		out := mustTranscode(t, req)
		if out.Reasoning == nil {
			t.Fatalf("effort %q: reasoning absent", e)
		}
		if out.Reasoning.Effort != string(e) {
			t.Errorf("effort %q: reasoning.effort = %q", e, out.Reasoning.Effort)
		}
		if out.Temperature != nil {
			t.Errorf("effort %q: temperature must be dropped with reasoning on", e)
		}
		if out.TopP != nil {
			t.Errorf("effort %q: top_p must be dropped with reasoning on", e)
		}
		if out.MaxOutputTokens != reasoningMinOutputTokens {
			t.Errorf("effort %q: max_output_tokens = %d, want %d", e, out.MaxOutputTokens, reasoningMinOutputTokens)
		}
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"reasoning":{"effort":"`+string(e)+`"}`) {
			t.Errorf("effort %q: wire missing reasoning: %s", e, raw)
		}
	}
}

// TestEffortKeepsLargeMaxOutput: a caller's max_output_tokens already above the
// floor is preserved, not shrunk.
func TestEffortKeepsLargeMaxOutput(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.MaxTokens = 64000
	req.Effort = message.EffortHigh
	out := mustTranscode(t, req)
	if out.MaxOutputTokens != 64000 {
		t.Errorf("max_output_tokens = %d, want 64000 (kept)", out.MaxOutputTokens)
	}
}

// TestNoEffortKeepsSampling: with no reasoning, temperature/top_p pass through
// unchanged (the fix must not touch the non-reasoning path).
func TestNoEffortKeepsSampling(t *testing.T) {
	temp := 0.7
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.Temperature = &temp
	req.Effort = message.EffortOff
	out := mustTranscode(t, req)
	if out.Temperature == nil {
		t.Error("temperature must be preserved when reasoning is off")
	}
}

// TestEffortOffAndUnsetNoReasoning: EffortOff and EffortUnset send no reasoning
// control at all (omitempty drops the field).
func TestEffortOffAndUnsetNoReasoning(t *testing.T) {
	for _, e := range []message.Effort{message.EffortUnset, message.EffortOff} {
		req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
		req.Effort = e
		out := mustTranscode(t, req)
		if out.Reasoning != nil {
			t.Errorf("effort %q: reasoning must be absent", e)
		}
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "reasoning\":{") {
			t.Errorf("effort %q: wire must omit reasoning object: %s", e, raw)
		}
	}
}
