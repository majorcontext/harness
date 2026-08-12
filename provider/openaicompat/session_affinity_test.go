package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestSessionKeySetsUserField: a non-empty Request.SessionKey sets the
// top-level "user" field to the same string, for Fireworks-style
// per-replica prompt-cache affinity through a gateway (see AGENTS.md,
// "Session affinity" section).
func TestSessionKeySetsUserField(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.SessionKey = "sess_abc"
	out := mustTranscode(t, req)
	if out.User != "sess_abc" {
		t.Errorf("User = %q, want %q", out.User, "sess_abc")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"user":"sess_abc"`) {
		t.Errorf("wire missing user field: %s", raw)
	}
}

// TestSessionKeyEmptyOmitsUserField: an empty Request.SessionKey (the
// zero value) omits the "user" field entirely rather than sending an empty
// string.
func TestSessionKeyEmptyOmitsUserField(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.SessionKey = ""
	out := mustTranscode(t, req)
	if out.User != "" {
		t.Errorf("User = %q, want empty", out.User)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"user":`) {
		t.Errorf("wire must omit user field: %s", raw)
	}
}
