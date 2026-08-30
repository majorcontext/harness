package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// toolRequestWithPattern is a request carrying one tool whose parameter
// schema has a `pattern` keyword — the exact keyword the ChatGPT Codex
// backend 400s on when it uses a regex lookaround
// ($.properties.email.pattern).
func toolRequestWithPattern() *provider.Request {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.Tools = []provider.ToolDef{{
		Name:        "send_email",
		Description: "sends an email",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"email": {"type": "string", "pattern": "^(?=.*@).+$"}
			},
			"required": ["email"]
		}`),
	}}
	return req
}

// TestTranscodeSanitizeToolSchemasOff: with the flag off (the default),
// transcodeRequestFamily must leave a tool's parameter schema — including
// an unsupported keyword like `pattern` — completely unchanged. This is the
// "normal openai/anthropic/bifrost providers are unaffected" half of the
// feature.
func TestTranscodeSanitizeToolSchemasOff(t *testing.T) {
	req := toolRequestWithPattern()
	out, err := transcodeRequestFamily(req, Family, nil, false)
	if err != nil {
		t.Fatalf("transcodeRequestFamily: %v", err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("Tools = %#v, want 1 entry", out.Tools)
	}
	got := string(out.Tools[0].Parameters)
	want := string(req.Tools[0].InputSchema)
	if got != want {
		t.Errorf("Parameters = %s, want unchanged %s", got, want)
	}
	if !strings.Contains(got, `"pattern"`) {
		t.Errorf("Parameters = %s, want pattern to survive when sanitization is off", got)
	}
}

// TestTranscodeSanitizeToolSchemasOn is the request-build proof: with the
// flag on, the emitted tool's parameter schema has `pattern` stripped.
func TestTranscodeSanitizeToolSchemasOn(t *testing.T) {
	req := toolRequestWithPattern()
	out, err := transcodeRequestFamily(req, Family, nil, true)
	if err != nil {
		t.Fatalf("transcodeRequestFamily: %v", err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("Tools = %#v, want 1 entry", out.Tools)
	}
	got := string(out.Tools[0].Parameters)
	if strings.Contains(got, `"pattern"`) {
		t.Errorf("Parameters = %s, want pattern stripped when sanitization is on", got)
	}

	var schema map[string]interface{}
	if err := json.Unmarshal(out.Tools[0].Parameters, &schema); err != nil {
		t.Fatalf("unmarshal sanitized parameters: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("type = %v, want object preserved", schema["type"])
	}
	required, ok := schema["required"].([]interface{})
	if !ok || len(required) != 1 || required[0] != "email" {
		t.Errorf("required = %#v, want [email] preserved", schema["required"])
	}
}
