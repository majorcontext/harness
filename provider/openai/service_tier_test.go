package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestServiceTierSetsField: a non-empty Request.ServiceTier sets the
// Responses API top-level "service_tier" field to the same string. Mirrors
// TestSessionKeySetsPromptCacheKey (session_affinity_test.go) — the same
// pass-through shape as PromptCacheKey, forwarded verbatim with no
// validation of which tiers exist (boxes owns that gating table).
func TestServiceTierSetsField(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.ServiceTier = "fast"
	out := mustTranscode(t, req)
	if out.ServiceTier != "fast" {
		t.Errorf("ServiceTier = %q, want %q", out.ServiceTier, "fast")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"service_tier":"fast"`) {
		t.Errorf("wire missing service_tier field: %s", raw)
	}
}

// TestServiceTierEmptyOmitsField: an empty Request.ServiceTier (the zero
// value) omits the "service_tier" field entirely rather than sending an
// empty string. Mirrors TestSessionKeyEmptyOmitsPromptCacheKey.
func TestServiceTierEmptyOmitsField(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.ServiceTier = ""
	out := mustTranscode(t, req)
	if out.ServiceTier != "" {
		t.Errorf("ServiceTier = %q, want empty", out.ServiceTier)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"service_tier":`) {
		t.Errorf("wire must omit service_tier field: %s", raw)
	}
}
