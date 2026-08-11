package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestEffortSetsReasoningEffort: a non-off effort level sets the top-level
// reasoning_effort field to the same string.
func TestEffortSetsReasoningEffort(t *testing.T) {
	for _, e := range []message.Effort{message.EffortMinimal, message.EffortLow, message.EffortMedium, message.EffortHigh} {
		req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
		req.Effort = e
		out := mustTranscode(t, req)
		if out.ReasoningEffort != string(e) {
			t.Errorf("effort %q: reasoning_effort = %q", e, out.ReasoningEffort)
		}
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"reasoning_effort":"`+string(e)+`"`) {
			t.Errorf("effort %q: wire missing reasoning_effort: %s", e, raw)
		}
	}
}

// TestEffortOffAndUnsetNoReasoningEffort: EffortOff and EffortUnset send no
// reasoning_effort (omitempty drops the empty string).
func TestEffortOffAndUnsetNoReasoningEffort(t *testing.T) {
	for _, e := range []message.Effort{message.EffortUnset, message.EffortOff} {
		req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
		req.Effort = e
		out := mustTranscode(t, req)
		if out.ReasoningEffort != "" {
			t.Errorf("effort %q: reasoning_effort must be empty", e)
		}
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "reasoning_effort") {
			t.Errorf("effort %q: wire must omit reasoning_effort: %s", e, raw)
		}
	}
}
