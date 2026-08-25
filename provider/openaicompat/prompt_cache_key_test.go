package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestSessionKeySetsPromptCacheKey: a non-empty Request.SessionKey also sets
// the top-level "prompt_cache_key" field, ALONGSIDE "user". A gateway
// (Bifrost) forwards prompt_cache_key to an upstream that reads it for
// prompt-cache affinity; "user" stays for the measured Fireworks path.
func TestSessionKeySetsPromptCacheKey(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.SessionKey = "sess_abc"
	out := mustTranscode(t, req)
	if out.PromptCacheKey != "sess_abc" {
		t.Errorf("PromptCacheKey = %q, want %q", out.PromptCacheKey, "sess_abc")
	}
	if out.User != "sess_abc" {
		t.Errorf("User = %q, want %q — user must stay as-is", out.User, "sess_abc")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"prompt_cache_key":"sess_abc"`) {
		t.Errorf("wire missing prompt_cache_key field: %s", raw)
	}
	if !strings.Contains(string(raw), `"user":"sess_abc"`) {
		t.Errorf("wire missing user field: %s", raw)
	}
}

// TestSessionKeyEmptyOmitsPromptCacheKey: an empty Request.SessionKey omits
// prompt_cache_key entirely rather than sending an empty string, the same
// omit-on-empty rule "user" follows.
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
