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

// TestEffortUnsetNoReasoningEffort: EffortUnset sends no reasoning_effort,
// leaving the upstream/gateway default in force (omitempty drops the empty
// string).
func TestEffortUnsetNoReasoningEffort(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.Effort = message.EffortUnset
	out := mustTranscode(t, req)
	if out.ReasoningEffort != "" {
		t.Errorf("effort unset: reasoning_effort must be empty, got %q", out.ReasoningEffort)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "reasoning_effort") {
		t.Errorf("effort unset: wire must omit reasoning_effort: %s", raw)
	}
}

// TestEffortOffSendsLiteralOff: EffortOff sends the literal string "off" so
// a gateway upstream that reasons by default (measured: Fireworks kimi-k3 via
// Bifrost emits a full reasoning block when the field is absent, and zero
// reasoning tokens when "reasoning_effort":"off" is sent) can actually be
// told to disable reasoning. Unset must never gain this field.
func TestEffortOffSendsLiteralOff(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.Effort = message.EffortOff
	out := mustTranscode(t, req)
	if out.ReasoningEffort != "off" {
		t.Errorf("effort off: reasoning_effort = %q, want %q", out.ReasoningEffort, "off")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"reasoning_effort":"off"`) {
		t.Errorf("effort off: wire missing literal reasoning_effort off: %s", raw)
	}
}
