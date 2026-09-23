package modelmeta

import (
	"testing"

	"github.com/majorcontext/harness/message"
)

func TestContextWindowAnthropic(t *testing.T) {
	// config.DefaultModel must have context-window metadata.
	tokens, ok := ContextWindow(message.ModelRef{Provider: "anthropic", Model: "claude-fable-5"})
	if !ok || tokens != 1_000_000 {
		t.Fatalf("ContextWindow(anthropic/claude-fable-5) = %d, %v; want 1000000, true", tokens, ok)
	}
}

func TestContextWindowOpenAI(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"gpt-5", 400_000},
		{"gpt-6-astra", 1_050_000},
	}
	for _, c := range cases {
		tokens, ok := ContextWindow(message.ModelRef{Provider: "openai", Model: c.model})
		if !ok || tokens != c.want {
			t.Errorf("ContextWindow(openai/%s) = %d, %v; want %d, true", c.model, tokens, ok, c.want)
		}
	}
}

// TestContextWindowCodex proves a "codex"-provider ref (the boxes platform's
// form for a ChatGPT Codex backend model — see
// meetneptune/boxes internal/api/codex_models.go, which mints refs like
// "codex/gpt-5.6-sol") resolves from the SAME openaiContextWindows table the
// "openai" provider case already uses: each model is served over two
// different transports (openai/<model> and codex/<model>) but names one
// model, so it must report one context window.
func TestContextWindowCodex(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"gpt-5.6-sol", 1_050_000},
		{"gpt-6-astra", 1_050_000},
		{"gpt-6-sol", 1_050_000},
		{"gpt-6-luna", 1_050_000},
	}
	for _, c := range cases {
		tokens, ok := ContextWindow(message.ModelRef{Provider: "codex", Model: c.model})
		if !ok || tokens != c.want {
			t.Errorf("ContextWindow(codex/%s) = %d, %v; want %d, true", c.model, tokens, ok, c.want)
		}
	}
}

// TestContextWindowCodexUnknownModelStillMisses proves the codex case never falls back to a stand-in figure.
func TestContextWindowCodexUnknownModelStillMisses(t *testing.T) {
	if tokens, ok := ContextWindow(message.ModelRef{Provider: "codex", Model: "gpt-nonexistent"}); ok {
		t.Errorf("ContextWindow(codex/gpt-nonexistent) = %d, true; want ok=false", tokens)
	}
}

func TestContextWindowClaudeCodeNoStandIn(t *testing.T) {
	tokens, ok := ContextWindow(message.ModelRef{Provider: "claude-code", Model: "opus"})
	if !ok || tokens != 0 {
		t.Fatalf("ContextWindow(claude-code/opus) = %d, %v; want 0, true", tokens, ok)
	}
}

func TestSuppressUsageGauge(t *testing.T) {
	cases := []struct {
		ref  message.ModelRef
		want bool
	}{
		{message.ModelRef{Provider: "bifrost", Model: "fireworks/accounts/fireworks/routers/firerouter"}, true},
		{message.ModelRef{Provider: "bifrost", Model: "fireworks/accounts/fireworks/models/glm-5p2"}, false},
		{message.ModelRef{Provider: "claude-code", Model: "opus"}, false},
	}
	for _, c := range cases {
		if got := SuppressUsageGauge(c.ref); got != c.want {
			t.Errorf("SuppressUsageGauge(%v) = %v, want %v", c.ref, got, c.want)
		}
	}
}

func TestFirerouterFloorPinnedToCandidates(t *testing.T) {
	if len(firerouterCandidateModels) == 0 {
		t.Fatal("firerouterCandidateModels is empty")
	}
	min := -1
	for _, m := range firerouterCandidateModels {
		tokens, ok := bifrostFireworksContextWindows[m]
		if !ok {
			t.Fatalf("firerouterCandidateModels names %q, which bifrostFireworksContextWindows does not key", m)
		}
		if min == -1 || tokens < min {
			min = tokens
		}
	}
	floor := bifrostFireworksContextWindows["firerouter"]
	if floor > min {
		t.Errorf("firerouter floor = %d, want <= %d (the smallest candidate's window, %s)", floor, min, firerouterCandidateModels)
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

// TestContextWindowBifrost verifies the boxes fleet default
// ("bifrost/fireworks/accounts/fireworks/routers/firerouter") and every
// other bifrost/<vendor>/<path> ref the boxes catalog ships resolves a
// known window.
func TestContextWindowBifrost(t *testing.T) {
	cases := []struct {
		refString string
		want      int
	}{
		{"bifrost/fireworks/accounts/fireworks/routers/firerouter", 1_000_000},
		{"bifrost/fireworks/accounts/fireworks/models/kimi-k3", 1_048_576},
		{"bifrost/fireworks/accounts/fireworks/models/kimi-k2p7-code", 262_000},
		{"bifrost/fireworks/accounts/fireworks/models/glm-5p2", 1_048_575},
		{"bifrost/fireworks/accounts/fireworks/models/glm-5p3", 1_048_573},
		{"bifrost/fireworks/accounts/fireworks/models/glm-5p3-flash", 1_048_573},
		{"bifrost/fireworks/accounts/fireworks/models/deepseek-v4-pro-0813", 1_000_000},
		{"bifrost/fireworks/accounts/fireworks/models/deepseek-v4-flash-0731", 1_000_000},
		{"bifrost/vertex/gemini-3.1-pro-preview", 1_048_576},
		{"bifrost/vertex/gemini-3.5-flash-lite", 1_048_576},
		{"bifrost/vertex/gemini-3.7-flash", 1_048_576},
		{"bifrost/vertex/gemini-3.8-flash", 1_048_576},
	}
	for _, tt := range cases {
		ref, err := message.ParseModelRef(tt.refString)
		if err != nil {
			t.Fatalf("message.ParseModelRef(%q) error: %v", tt.refString, err)
		}
		tokens, ok := ContextWindow(ref)
		if !ok || tokens != tt.want {
			t.Errorf("ContextWindow(%q) = %d, %v; want %d, true", tt.refString, tokens, ok, tt.want)
		}
	}
}

// TestContextWindowBoxesThreeSegmentRefs verifies that the boxes platform
// (meetneptune/boxes internal/api/bifrost_models.go) passes THREE-segment
// model refs exclusively, e.g. "anthropic/anthropic/claude-fable-5" and
// "anthropic/bedrock_mantle/anthropic.claude-opus-5". message.ParseModelRef
// splits on the FIRST slash only, so these refs parse to
// Provider "anthropic" and a Model that still carries the routing
// namespace segment ("anthropic/claude-fable-5",
// "bedrock_mantle/anthropic.claude-opus-5") ahead of the actual model ID —
// a map lookup keyed on the bare ID (e.g. "claude-fable-5") misses every
// one of them without namespace removal.
func TestContextWindowBoxesThreeSegmentRefs(t *testing.T) {
	cases := []struct {
		refString string
		want      int
	}{
		// Direct vendor route: provider/anthropic/<model-id>.
		{"anthropic/anthropic/claude-fable-5", 1_000_000},
		{"anthropic/anthropic/claude-opus-5", 1_000_000},
		{"anthropic/anthropic/claude-haiku-4-5-20251001", 200_000},
		// bedrock_mantle route: provider/bedrock_mantle/anthropic.<model-id>,
		// still under the "anthropic" top-level provider key (see
		// bifrost_models.go: "both the direct vendor route and
		// bedrock_mantle" share the native anthropic adapter).
		{"anthropic/bedrock_mantle/anthropic.claude-fable-5", 1_000_000},
		{"anthropic/bedrock_mantle/anthropic.claude-opus-5", 1_000_000},
		// A version-suffixed mantle ID must use the same key.
		{"anthropic/bedrock_mantle/anthropic.claude-opus-5-v1:0", 1_000_000},
		// The one family where the two tables DIVERGE (see
		// bedrockAnthropicContextWindows's doc comment): a mantle-routed
		// ref is served through Bedrock, so it must honor the Bedrock
		// window (200k), not the first-party 1M — over-reporting arms
		// compaction at 800k against a real 200k limit, re-creating the
		// exact overflow class this package exists to prevent.
		{"anthropic/bedrock_mantle/anthropic.claude-sonnet-4-5-20250929-v1:0", 200_000},
		{"anthropic/bedrock_mantle/anthropic.claude-sonnet-4-5-20250929", 200_000},
		// A version suffix on a NON-dotted direct-vendor ref must also
		// normalize away (suffix stripping must not hide inside the
		// "anthropic." prefix branch).
		{"anthropic/claude-opus-5-v1:0", 1_000_000},
		// A region-prefixed mantle ref must still be recognized as
		// Bedrock-style: the anthropic branch shares
		// stripBedrockAnthropicPrefix with the amazon-bedrock branch, so
		// "us."/"eu."/"global." tolerance is symmetric — a bare
		// CutPrefix("anthropic.") here would miss this ref entirely and
		// silently disarm compaction.
		{"anthropic/bedrock_mantle/us.anthropic.claude-opus-5", 1_000_000},
		// Bare two-segment forms (non-boxes callers) must keep working.
		{"anthropic/claude-fable-5", 1_000_000},
		{"anthropic/claude-opus-5", 1_000_000},
	}
	for _, tt := range cases {
		ref, err := message.ParseModelRef(tt.refString)
		if err != nil {
			t.Fatalf("message.ParseModelRef(%q) error: %v", tt.refString, err)
		}
		tokens, ok := ContextWindow(ref)
		if !ok || tokens != tt.want {
			t.Errorf("ContextWindow(%q) = %d, %v; want %d, true", tt.refString, tokens, ok, tt.want)
		}
	}
}

// TestContextWindowBedrockVersionSuffixNormalized verifies that Bedrock keys
// inconsistent about carrying a trailing "-vN"/"-vN:M" suffix (some
// entries have it, some don't — see bedrockAnthropicContextWindows), and
// stripBedrockAnthropicPrefix normalizes region/family but not version.
// "amazon-bedrock/us.anthropic.claude-opus-4-8-v1:0" must hit the same
// entry as the bare "claude-opus-4-8" form, and a query for a model whose
// table entry legitimately carries a version suffix must hit regardless
// of whether the query itself is suffixed.
func TestContextWindowBedrockVersionSuffixNormalized(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		// claude-opus-4-8's table entry is bare; a suffixed query must still hit.
		{"us.anthropic.claude-opus-4-8-v1:0", 1_000_000},
		{"anthropic.claude-opus-4-8-v1:0", 1_000_000},
		// claude-sonnet-4-5-20250929's real models.dev bedrock ID carries
		// "-v1:0" (see the table's doc comment); both the exact suffixed
		// form and a hypothetical bare query must hit the same entry.
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", 200_000},
		{"us.anthropic.claude-sonnet-4-5-20250929", 200_000},
	}
	for _, tt := range cases {
		tokens, ok := ContextWindow(message.ModelRef{Provider: "amazon-bedrock", Model: tt.model})
		if !ok || tokens != tt.want {
			t.Errorf("ContextWindow(amazon-bedrock/%s) = %d, %v; want %d, true", tt.model, tokens, ok, tt.want)
		}
	}
}

func TestContextWindowUnknown(t *testing.T) {
	cases := []message.ModelRef{
		{Provider: "anthropic", Model: "claude-nonexistent"},
		{Provider: "some-unconfigured-provider", Model: "claude-fable-5"},
		{Provider: "amazon-bedrock", Model: "amazon.titan-text-express-v1"},
		{Provider: "amazon-bedrock", Model: "us.amazon.titan-text-express-v1"},
		// A dotted (Bedrock-served) ref whose family the bedrock table
		// doesn't key MUST resolve unknown, never borrow the first-party
		// window: claude-sonnet-4-5 is 1M first-party but 200k on Bedrock
		// (keyed only under its dated form), so a first-party fallback
		// here would over-report 5x and re-create the overflow class this
		// package prevents.
		{Provider: "anthropic", Model: "bedrock_mantle/anthropic.claude-sonnet-4-5"},
		// A bifrost near miss must fail closed rather than match a
		// neighboring table entry by accident.
		{Provider: "bifrost", Model: "vertex/gemini-3.1-pro"},
		{Provider: "bifrost", Model: "fireworks/accounts/fireworks/models/kimi-k3x"},
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
