package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestSessionKeySetsPromptCacheKey: a non-empty Request.SessionKey sets the
// Responses API top-level "prompt_cache_key" field to the same string, for
// per-replica prompt-cache routing affinity (see docs/models-and-providers.md,
// "Session affinity" section). This adapter uses prompt_cache_key, the Responses
// API's own routing hint — NOT the "user" field the openaicompat adapter
// uses for a generic chat-completions gateway.
func TestSessionKeySetsPromptCacheKey(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.SessionKey = "sess_abc"
	out := mustTranscode(t, req)
	if out.PromptCacheKey != "sess_abc" {
		t.Errorf("PromptCacheKey = %q, want %q", out.PromptCacheKey, "sess_abc")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"prompt_cache_key":"sess_abc"`) {
		t.Errorf("wire missing prompt_cache_key field: %s", raw)
	}
}

// TestSessionKeyEmptyOmitsPromptCacheKey: an empty Request.SessionKey (the
// zero value) omits the "prompt_cache_key" field entirely rather than
// sending an empty string.
func TestSessionKeyEmptyOmitsPromptCacheKey(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.SessionKey = ""
	out := mustTranscode(t, req)
	if out.PromptCacheKey != "" {
		t.Errorf("PromptCacheKey = %q, want empty", out.PromptCacheKey)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"prompt_cache_key":`) {
		t.Errorf("wire must omit prompt_cache_key field: %s", raw)
	}
}
