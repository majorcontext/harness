package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func TestCodexRequestIncludesReasoningSummaryAutoWhenUnset(t *testing.T) {
	req := &provider.Request{
		Model:    message.ModelRef{Provider: CodexFamily, Model: "gpt-5.6-sol"},
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
	}
	wire, err := transcodeRequestFamily(req, CodexFamily, nil, false)
	if err != nil {
		t.Fatalf("transcodeRequestFamily: %v", err)
	}
	if wire.Reasoning == nil {
		t.Fatal("expected wire.Reasoning to be non-nil for Codex when Effort is unset")
	}
	if wire.Reasoning.Summary != "auto" {
		t.Fatalf("Reasoning.Summary = %q, want %q", wire.Reasoning.Summary, "auto")
	}
	if wire.Reasoning.Effort != "" {
		t.Fatalf("Reasoning.Effort = %q, want empty (unset)", wire.Reasoning.Effort)
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"summary":"auto"`) {
		t.Fatalf("marshaled wire missing summary:auto: %s", string(raw))
	}
}

func TestCodexRequestIncludesReasoningSummaryWithEnabledEffort(t *testing.T) {
	req := &provider.Request{
		Model:    message.ModelRef{Provider: CodexFamily, Model: "gpt-5.6-sol"},
		Effort:   message.EffortMedium,
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
	}
	wire, err := transcodeRequestFamily(req, CodexFamily, nil, false)
	if err != nil {
		t.Fatalf("transcodeRequestFamily: %v", err)
	}
	if wire.Reasoning == nil {
		t.Fatal("expected wire.Reasoning to be non-nil")
	}
	if wire.Reasoning.Effort != "medium" {
		t.Fatalf("Reasoning.Effort = %q, want medium", wire.Reasoning.Effort)
	}
	if wire.Reasoning.Summary != "auto" {
		t.Fatalf("Reasoning.Summary = %q, want auto", wire.Reasoning.Summary)
	}
}

func TestCodexRequestOmitsReasoningWhenEffortOff(t *testing.T) {
	req := &provider.Request{
		Model:    message.ModelRef{Provider: CodexFamily, Model: "gpt-5.6-sol"},
		Effort:   message.EffortOff,
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
	}
	wire, err := transcodeRequestFamily(req, CodexFamily, nil, false)
	if err != nil {
		t.Fatalf("transcodeRequestFamily: %v", err)
	}
	if wire.Reasoning != nil {
		t.Fatalf("Reasoning = %+v, want nil for EffortOff", wire.Reasoning)
	}
}

func TestGenericOpenAIRequestOmitsReasoningSummary(t *testing.T) {
	req := &provider.Request{
		Model:    message.ModelRef{Provider: Family, Model: "gpt-5"},
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
	}
	wire, err := transcodeRequestFamily(req, Family, nil, false)
	if err != nil {
		t.Fatalf("transcodeRequestFamily: %v", err)
	}
	if wire.Reasoning != nil {
		t.Fatalf("generic OpenAI Reasoning = %+v, want nil when Effort is unset", wire.Reasoning)
	}
}

func TestResponsesRequestPropertiesMatchIncludesReasoningSummary(t *testing.T) {
	r1 := &apiRequest{Reasoning: &apiReasoning{Effort: "low", Summary: "auto"}}
	r2 := &apiRequest{Reasoning: &apiReasoning{Effort: "low", Summary: "none"}}
	if responsesRequestPropertiesMatch(r1, r2) {
		t.Fatal("responsesRequestPropertiesMatch should return false when Summary differs")
	}
	r3 := &apiRequest{Reasoning: &apiReasoning{Effort: "low", Summary: "auto"}}
	if !responsesRequestPropertiesMatch(r1, r3) {
		t.Fatal("responsesRequestPropertiesMatch should return true when Summary matches")
	}
}
