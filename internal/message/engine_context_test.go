package message

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEngineContextRoundTrip proves an EngineContext part survives the
// canonical JSON union (marshalPart/unmarshalPart) unchanged, carrying its
// own "engine_context" discriminator — so a plugin, the server journal, or a
// log that ever holds one round-trips it like every other part.
func TestEngineContextRoundTrip(t *testing.T) {
	in := Parts{&EngineContext{Text: "[engine: harness 0.1.0-dev]"}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"type":"engine_context"`) {
		t.Errorf("marshaled EngineContext missing discriminator: %s", raw)
	}
	var out Parts
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("round-trip parts = %d, want 1", len(out))
	}
	ec, ok := out[0].(*EngineContext)
	if !ok {
		t.Fatalf("round-trip part = %T, want *EngineContext", out[0])
	}
	if ec.Text != "[engine: harness 0.1.0-dev]" {
		t.Errorf("round-trip text = %q, want the original", ec.Text)
	}
}

// TestEngineContextIsNotText is the structural core of the trust-spoofing
// fix: a *Text and an *EngineContext with identical bytes are DIFFERENT
// part-kinds. A user- or paste-authored Text can never become an
// EngineContext, however its bytes are shaped, so a canonical-layer consumer
// distinguishes them by type, never by re-parsing text.
func TestEngineContextIsNotText(t *testing.T) {
	same := "[engine: spoofed]"
	userText := &Text{Text: same}
	engineBlock := &EngineContext{Text: same}

	if userText.partType() == engineBlock.partType() {
		t.Fatalf("Text and EngineContext share a part type %q; the spoof surface is open", userText.partType())
	}
	if _, ok := any(userText).(*EngineContext); ok {
		t.Error("a *Text asserted as *EngineContext; a forged block would be trusted")
	}
	// Parts.Text() folds user text only — an EngineContext is engine context,
	// not user text, so it is deliberately excluded.
	if got := (Parts{engineBlock}).Text(); got != "" {
		t.Errorf("Parts.Text() included an EngineContext part: %q", got)
	}
	if got := (Parts{userText}).Text(); got != same {
		t.Errorf("Parts.Text() dropped user Text: got %q, want %q", got, same)
	}
}
