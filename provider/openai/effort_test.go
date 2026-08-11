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
	// LITERAL expected floors (an oracle, NOT a call to reasoningOutputFloor):
	// caller MaxTokens 8192 is below every floor, so each level is raised to its
	// floor (minimal's floor is 10000, the lowest — 8192 is raised to it).
	wantFloor := map[message.Effort]int{
		message.EffortMinimal: 10000,
		message.EffortLow:     12000,
		message.EffortMedium:  18000,
		message.EffortHigh:    25000,
	}
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
		if out.MaxOutputTokens != wantFloor[e] {
			t.Errorf("effort %q: max_output_tokens = %d, want %d", e, out.MaxOutputTokens, wantFloor[e])
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

// TestReasoningStrippedWhenOff: a stored reasoning item (from an earlier
// reasoning-ON turn) is STRIPPED when the current request sends no reasoning
// control. Symmetric with the anthropic thinking-block strip: shipping a stored
// reasoning item while the request omits `reasoning` is rejected, and durable in
// history it would 400 every later turn (a permanent wedge). With reasoning ON
// the item is replayed (see TestTranscodeReasoningReplayVerbatim).
func TestReasoningStrippedWhenOff(t *testing.T) {
	history := func() []message.Message {
		return []message.Message{
			{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
			{Role: message.RoleAssistant, Parts: message.Parts{
				&message.Reasoning{Text: "hmm", ProviderData: message.ProviderData{
					Family: json.RawMessage(`{"id":"rs_1","type":"reasoning","encrypted_content":"ENC"}`),
				}},
				&message.Text{Text: "answer"},
			}},
			{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "again"}}},
		}
	}
	for _, e := range []message.Effort{message.EffortUnset, message.EffortOff} {
		req := baseRequest(history()...)
		req.Effort = e
		out := mustTranscode(t, req)
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"type":"reasoning"`) || strings.Contains(string(raw), `rs_1`) {
			t.Errorf("effort %q: wire carries a stored reasoning item, want it stripped with reasoning off:\n%s", e, raw)
		}
		// The assistant's visible answer must survive the strip.
		if !strings.Contains(string(raw), "answer") {
			t.Errorf("effort %q: assistant text lost with the stripped reasoning", e)
		}
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
