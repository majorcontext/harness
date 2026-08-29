package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// float64Ptr is a tiny helper so tests can take the address of a literal.
func float64Ptr(f float64) *float64 { return &f }

// TestOmitResponseParamsNoneListedUnchanged: a request transcoded with no
// omit list behaves exactly as before the field existed — the "changed
// nothing for anyone" half of this feature, mirrored on ResponsesPath's own
// default test.
func TestOmitResponseParamsNoneListedUnchanged(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.Temperature = float64Ptr(0.5)
	req.TopP = float64Ptr(0.9)

	out, err := transcodeRequestFamily(req, Family, nil)
	if err != nil {
		t.Fatalf("transcodeRequestFamily: %v", err)
	}
	if out.MaxOutputTokens != 4096 {
		t.Errorf("MaxOutputTokens = %d, want 4096 (unchanged)", out.MaxOutputTokens)
	}
	if out.Temperature == nil || *out.Temperature != 0.5 {
		t.Errorf("Temperature = %v, want 0.5", out.Temperature)
	}
	if out.TopP == nil || *out.TopP != 0.9 {
		t.Errorf("TopP = %v, want 0.9", out.TopP)
	}
}

// TestOmitResponseParamsAllFourOmitsFromWire is the reason the field
// exists: the ChatGPT Codex backend 400s on max_output_tokens, temperature,
// top_p, and metadata. Listing all four (the config allowlist) must clear
// every field this adapter actually sends for them — MaxOutputTokens (via
// its omitempty int tag), Temperature, and TopP; "metadata" has no
// corresponding apiRequest field yet, so it is accepted but a no-op here
// (see applyOmitResponseParams's doc comment).
func TestOmitResponseParamsAllFourOmitsFromWire(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.Temperature = float64Ptr(0.5)
	req.TopP = float64Ptr(0.9)

	omit := []string{"max_output_tokens", "temperature", "top_p", "metadata"}
	out, err := transcodeRequestFamily(req, Family, omit)
	if err != nil {
		t.Fatalf("transcodeRequestFamily: %v", err)
	}
	if out.MaxOutputTokens != 0 {
		t.Errorf("MaxOutputTokens = %d, want 0 (omitted)", out.MaxOutputTokens)
	}
	if out.Temperature != nil {
		t.Errorf("Temperature = %v, want nil (omitted)", out.Temperature)
	}
	if out.TopP != nil {
		t.Errorf("TopP = %v, want nil (omitted)", out.TopP)
	}

	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{`"max_output_tokens"`, `"temperature"`, `"top_p"`} {
		if strings.Contains(string(body), field) {
			t.Errorf("wire body contains %s, want it omitted entirely: %s", field, body)
		}
	}
}

// TestOmitResponseParamsPartialList: an entry that lists only SOME of the
// four params must omit exactly those and leave the rest unchanged — the
// per-field independence a config.Provider.OmitResponseParams entry needs
// (a deployment omitting only max_output_tokens still wants an explicit
// temperature honored).
func TestOmitResponseParamsPartialList(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.Temperature = float64Ptr(0.5)

	out, err := transcodeRequestFamily(req, Family, []string{"max_output_tokens"})
	if err != nil {
		t.Fatalf("transcodeRequestFamily: %v", err)
	}
	if out.MaxOutputTokens != 0 {
		t.Errorf("MaxOutputTokens = %d, want 0 (omitted)", out.MaxOutputTokens)
	}
	if out.Temperature == nil || *out.Temperature != 0.5 {
		t.Errorf("Temperature = %v, want 0.5 (not listed, must be unchanged)", out.Temperature)
	}
}

// TestOmitResponseParamsWinsOverReasoningFloor: reasoningOutputFloor raises
// MaxOutputTokens for a reasoning turn BEFORE applyOmitResponseParams runs.
// An entry that omits max_output_tokens must still send none — the omit
// list is the caller's assertion that the upstream rejects the field
// outright, which no amount of internal floor-raising changes.
func TestOmitResponseParamsWinsOverReasoningFloor(t *testing.T) {
	req := reasoningRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})

	out, err := transcodeRequestFamily(req, Family, []string{"max_output_tokens"})
	if err != nil {
		t.Fatalf("transcodeRequestFamily: %v", err)
	}
	if out.MaxOutputTokens != 0 {
		t.Errorf("MaxOutputTokens = %d, want 0 (omitted even though reasoning would otherwise raise it)", out.MaxOutputTokens)
	}
}
