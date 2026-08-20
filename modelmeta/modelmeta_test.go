package modelmeta

import (
	"testing"

	"github.com/majorcontext/harness/message"
)

func TestContextWindowAnthropic(t *testing.T) {
	// config.DefaultModel — also the model jumpy-pizza's incident named
	// ("prompt 1136916 tokens > limit 1000000"), so this case is pinned to
	// the exact incident value on purpose.
	tokens, ok := ContextWindow(message.ModelRef{Provider: "anthropic", Model: "claude-fable-5"})
	if !ok || tokens != 1_000_000 {
		t.Fatalf("ContextWindow(anthropic/claude-fable-5) = %d, %v; want 1000000, true", tokens, ok)
	}
}

func TestContextWindowOpenAI(t *testing.T) {
	tokens, ok := ContextWindow(message.ModelRef{Provider: "openai", Model: "gpt-5"})
	if !ok || tokens != 400_000 {
		t.Fatalf("ContextWindow(openai/gpt-5) = %d, %v; want 400000, true", tokens, ok)
	}
}

func TestContextWindowBedrockRegionPrefixes(t *testing.T) {
	cases := []string{
		"anthropic.claude-opus-4-8",
		"us.anthropic.claude-opus-4-8",
		"eu.anthropic.claude-opus-4-8",
		"au.anthropic.claude-opus-4-8",
		"jp.anthropic.claude-opus-4-8",
		"global.anthropic.claude-opus-4-8",
	}
	for _, model := range cases {
		tokens, ok := ContextWindow(message.ModelRef{Provider: "amazon-bedrock", Model: model})
		if !ok || tokens != 1_000_000 {
			t.Errorf("ContextWindow(amazon-bedrock/%s) = %d, %v; want 1000000, true", model, tokens, ok)
		}
	}
}

func TestContextWindowBedrockVersionedSuffix(t *testing.T) {
	// Bedrock's model IDs carry a "-v1:0"/"-v1" version suffix that the
	// plain "anthropic" provider's IDs don't — this table keys on the
	// bedrock ID verbatim (see bedrockAnthropicContextWindows), so a mismatch
	// here would silently disable compaction on every bedrock session.
	tokens, ok := ContextWindow(message.ModelRef{Provider: "amazon-bedrock", Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0"})
	if !ok || tokens != 200_000 {
		t.Fatalf("ContextWindow(amazon-bedrock/us.anthropic.claude-sonnet-4-5-20250929-v1:0) = %d, %v; want 200000, true", tokens, ok)
	}
}

func TestContextWindowUnknown(t *testing.T) {
	cases := []message.ModelRef{
		{Provider: "anthropic", Model: "claude-nonexistent"},
		{Provider: "some-unconfigured-provider", Model: "claude-fable-5"},
		{Provider: "amazon-bedrock", Model: "amazon.titan-text-express-v1"},
		{Provider: "amazon-bedrock", Model: "us.amazon.titan-text-express-v1"},
		{},
	}
	for _, ref := range cases {
		if tokens, ok := ContextWindow(ref); ok {
			t.Errorf("ContextWindow(%v) = %d, true; want ok=false", ref, tokens)
		}
	}
}

func TestStripBedrockAnthropicPrefix(t *testing.T) {
	tests := []struct {
		model      string
		wantSuffix string
		wantOK     bool
	}{
		{"anthropic.claude-opus-4-8", "claude-opus-4-8", true},
		{"us.anthropic.claude-opus-4-8", "claude-opus-4-8", true},
		{"global.anthropic.claude-fable-5", "claude-fable-5", true},
		{"amazon.titan-text-express-v1", "", false},
		{"us.amazon.titan-text-express-v1", "", false},
		{"a.b.anthropic.claude-opus-4-8", "", false}, // only ONE segment is ever treated as region
		{"anthropic", "", false},
	}
	for _, tt := range tests {
		suffix, ok := stripBedrockAnthropicPrefix(tt.model)
		if suffix != tt.wantSuffix || ok != tt.wantOK {
			t.Errorf("stripBedrockAnthropicPrefix(%q) = %q, %v; want %q, %v", tt.model, suffix, ok, tt.wantSuffix, tt.wantOK)
		}
	}
}
