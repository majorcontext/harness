package modelmeta

import (
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestSupportsToolSearch pins the gate native delegation runs on. Every
// "true" model here is one the Anthropic tool-search doc's compatibility
// table lists; every "false" case is one where emitting the tool search
// tool would risk a rejected request on every turn, so the gate fails safe
// and the session keeps harness's own client-side deferral.
func TestSupportsToolSearch(t *testing.T) {
	tests := []struct {
		name string
		ref  message.ModelRef
		want bool
	}{
		// The documented table, in both the dated and undated forms a
		// config may name.
		{name: "opus 5", ref: message.ModelRef{Provider: "anthropic", Model: "claude-opus-5"}, want: true},
		{name: "fable 5", ref: message.ModelRef{Provider: "anthropic", Model: "claude-fable-5"}, want: true},
		{name: "mythos 5", ref: message.ModelRef{Provider: "anthropic", Model: "claude-mythos-5"}, want: true},
		{name: "sonnet 4.5 dated", ref: message.ModelRef{Provider: "anthropic", Model: "claude-sonnet-4-5-20250929"}, want: true},
		{name: "sonnet 4.5 undated", ref: message.ModelRef{Provider: "anthropic", Model: "claude-sonnet-4-5"}, want: true},
		{name: "haiku 4.5 dated", ref: message.ModelRef{Provider: "anthropic", Model: "claude-haiku-4-5-20251001"}, want: true},
		{name: "opus 4.5 dated", ref: message.ModelRef{Provider: "anthropic", Model: "claude-opus-4-5-20251101"}, want: true},
		{name: "opus 4.6", ref: message.ModelRef{Provider: "anthropic", Model: "claude-opus-4-6"}, want: true},
		{name: "opus 4.8", ref: message.ModelRef{Provider: "anthropic", Model: "claude-opus-4-8"}, want: true},

		// A Bifrost boxes ref carries a routing-namespace segment ahead of
		// the model ID; ContextWindow strips it and so must this.
		{name: "bifrost namespaced", ref: message.ModelRef{Provider: "anthropic", Model: "anthropic/claude-opus-5"}, want: true},

		// Opus 4.1 and earlier are named in the doc as unsupported.
		{name: "opus 4.1", ref: message.ModelRef{Provider: "anthropic", Model: "claude-opus-4-1"}, want: false},
		{name: "unknown model", ref: message.ModelRef{Provider: "anthropic", Model: "claude-something-9"}, want: false},
		{name: "empty model", ref: message.ModelRef{Provider: "anthropic"}, want: false},

		// Bedrock-style refs: server-side tool search is InvokeModel-only
		// there, and a ref cannot say which Bedrock API is in front of it.
		{name: "bedrock-style anthropic ref", ref: message.ModelRef{Provider: "anthropic", Model: "anthropic.claude-opus-5-v1:0"}, want: false},
		{name: "bedrock-style regional", ref: message.ModelRef{Provider: "anthropic", Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0"}, want: false},
		{name: "bedrock provider", ref: message.ModelRef{Provider: "bedrock", Model: "anthropic.claude-opus-5"}, want: false},

		// Other providers have no tool_search on the surface we speak.
		{name: "openai", ref: message.ModelRef{Provider: "openai", Model: "gpt-5.4"}, want: false},
		{name: "openai-compat gateway", ref: message.ModelRef{Provider: "bifrost", Model: "anthropic/claude-opus-5"}, want: false},
		{name: "empty ref", ref: message.ModelRef{}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SupportsToolSearch(tc.ref); got != tc.want {
				t.Fatalf("SupportsToolSearch(%+v) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}

// toolSearchModelsWithoutContextWindow are the tool-search models this
// package deliberately cannot size. models.dev publishes no entry for them
// (checked live), and no route this repo talks to serves them, so inventing
// a context window would be worse than admitting the gap: a wrong window
// arms compaction at the wrong threshold.
//
// The exemption is SELF-RETIRING — see the test below, which fails once a
// model here does gain a context-window entry, so the list cannot quietly
// outlive its reason.
var toolSearchModelsWithoutContextWindow = map[string]bool{
	"claude-mythos-5": true,
}

// TestToolSearchModelsAreKnownToContextWindow keeps the two tables honest
// about the same model set: every ref this package says can do tool search
// is one it also knows a context window for. A name in one table and not
// the other is a typo in whichever was edited last.
func TestToolSearchModelsAreKnownToContextWindow(t *testing.T) {
	for model := range anthropicToolSearchModels {
		ref := message.ModelRef{Provider: "anthropic", Model: model}
		_, ok := ContextWindow(ref)
		exempt := toolSearchModelsWithoutContextWindow[model]
		switch {
		case !ok && !exempt:
			t.Errorf("%q supports tool search but has no context-window entry", model)
		case ok && exempt:
			t.Errorf("%q now has a context-window entry: drop it from toolSearchModelsWithoutContextWindow", model)
		}
	}
}

// TestToolSearchExemptionsAreToolSearchModels stops the exemption list
// drifting into naming models that are not in the tool-search table at all.
func TestToolSearchExemptionsAreToolSearchModels(t *testing.T) {
	for model := range toolSearchModelsWithoutContextWindow {
		if !anthropicToolSearchModels[model] {
			t.Errorf("%q is exempted but is not a tool-search model", model)
		}
	}
}
