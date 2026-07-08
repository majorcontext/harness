package message

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMessageJSONRoundTrip(t *testing.T) {
	in := Message{
		ID:   "msg_1",
		Role: RoleAssistant,
		Parts: Parts{
			&Text{Text: "hello"},
			&Reasoning{
				Text: "thinking summary",
				ProviderData: ProviderData{
					"anthropic": json.RawMessage(`{"signature":"abc"}`),
				},
			},
			&ToolCall{
				CallID:    "tc_1",
				Name:      "bash",
				Arguments: json.RawMessage(`{"command":"ls"}`),
			},
			&Blob{MediaType: "image/png", Data: []byte{0x89, 0x50}},
		},
		Model:     ModelRef{Provider: "anthropic", Model: "claude-fable-5"},
		CreatedAt: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
	}

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Message
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

func TestToolResultRoundTrip(t *testing.T) {
	in := Message{
		ID:   "msg_2",
		Role: RoleTool,
		Parts: Parts{
			&ToolResult{
				CallID: "tc_1",
				Content: Parts{
					&Text{Text: "output line"},
					&Blob{MediaType: "image/png", URL: "https://example.com/x.png"},
				},
				IsError: true,
			},
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Message
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

func TestPartTypeDiscriminator(t *testing.T) {
	raw, err := json.Marshal(Parts{&Text{Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"type":"text"`) {
		t.Errorf("marshaled part missing type discriminator: %s", raw)
	}
}

func TestUnknownPartTypeErrors(t *testing.T) {
	var ps Parts
	err := json.Unmarshal([]byte(`[{"type":"holo_deck"}]`), &ps)
	if err == nil {
		t.Fatal("expected error for unknown part type")
	}
}

func TestPartsText(t *testing.T) {
	ps := Parts{
		&Text{Text: "a"},
		&Blob{MediaType: "image/png"},
		&Text{Text: "b"},
	}
	if got := ps.Text(); got != "a\nb" {
		t.Errorf("Text() = %q, want %q", got, "a\nb")
	}
}

func TestProviderCallID(t *testing.T) {
	a := ProviderCallID("toolu_", "tc_1", 24)
	b := ProviderCallID("toolu_", "tc_1", 24)
	if a != b {
		t.Errorf("not deterministic: %q vs %q", a, b)
	}
	if len(a) != 24 {
		t.Errorf("len = %d, want 24", len(a))
	}
	if !strings.HasPrefix(a, "toolu_") {
		t.Errorf("missing prefix: %q", a)
	}
	if ProviderCallID("call_", "tc_2", 0) == ProviderCallID("call_", "tc_3", 0) {
		t.Error("distinct call IDs collided")
	}
}

func TestModelRef(t *testing.T) {
	ref, err := ParseModelRef("amazon-bedrock/us.anthropic.claude-opus-4-8")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Provider != "amazon-bedrock" || ref.Model != "us.anthropic.claude-opus-4-8" {
		t.Errorf("unexpected parse: %+v", ref)
	}

	// Model portion may contain slashes.
	ref, err = ParseModelRef("vertex/publishers/google/gemini-3.1-pro")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Model != "publishers/google/gemini-3.1-pro" {
		t.Errorf("slash split wrong: %+v", ref)
	}

	for _, bad := range []string{"", "anthropic", "/model", "anthropic/"} {
		if _, err := ParseModelRef(bad); err == nil {
			t.Errorf("ParseModelRef(%q) should fail", bad)
		}
	}

	raw, err := json.Marshal(ModelRef{Provider: "anthropic", Model: "claude-fable-5"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `"anthropic/claude-fable-5"` {
		t.Errorf("marshal = %s", raw)
	}
	var out ModelRef
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "anthropic/claude-fable-5" {
		t.Errorf("unmarshal = %+v", out)
	}

	// Zero ref round-trips through "".
	raw, _ = json.Marshal(ModelRef{})
	if string(raw) != `""` {
		t.Errorf("zero marshal = %s", raw)
	}
	var zero ModelRef
	if err := json.Unmarshal(raw, &zero); err != nil {
		t.Fatal(err)
	}
	if !zero.IsZero() {
		t.Errorf("zero unmarshal = %+v", zero)
	}
}
