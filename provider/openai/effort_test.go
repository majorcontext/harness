package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestEffortSetsReasoning: a non-off effort level sets reasoning.effort to the
// same string; the marshaled request carries it.
func TestEffortSetsReasoning(t *testing.T) {
	for _, e := range []message.Effort{message.EffortMinimal, message.EffortLow, message.EffortMedium, message.EffortHigh} {
		req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
		req.Effort = e
		out := mustTranscode(t, req)
		if out.Reasoning == nil {
			t.Fatalf("effort %q: reasoning absent", e)
		}
		if out.Reasoning.Effort != string(e) {
			t.Errorf("effort %q: reasoning.effort = %q", e, out.Reasoning.Effort)
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
