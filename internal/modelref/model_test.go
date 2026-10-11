package modelref

import (
	"encoding/json"
	"testing"
)

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
