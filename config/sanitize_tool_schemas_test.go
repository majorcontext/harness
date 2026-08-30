package config

import (
	"strings"
	"testing"
)

// TestSanitizeToolSchemasOnNativeOpenAIKeyOK: the bare "openai" key (empty
// type) builds the Responses adapter, so it may carry
// sanitize_tool_schemas too — the field is gated on the ADAPTER an entry
// builds, not on its type string, exactly like ResponsesPath and
// OmitResponseParams.
func TestSanitizeToolSchemasOnNativeOpenAIKeyOK(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"openai": {SanitizeToolSchemas: true},
	}}
	merged, err := mergeAndValidate(c, &Config{})
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	if !merged.Providers["openai"].SanitizeToolSchemas {
		t.Error("SanitizeToolSchemas = false, want true")
	}
}

// TestSanitizeToolSchemasOnTypeOpenAIKeyOK: a TypeOpenAI entry under an
// arbitrary key builds the same adapter, so it may carry the field too.
func TestSanitizeToolSchemasOnTypeOpenAIKeyOK(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"secondary": {
			Type:                TypeOpenAI,
			BaseURL:             "https://gateway.example",
			SanitizeToolSchemas: true,
		},
	}}
	merged, err := mergeAndValidate(c, &Config{})
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	if !merged.Providers["secondary"].SanitizeToolSchemas {
		t.Error("SanitizeToolSchemas = false, want true")
	}
}

// TestSanitizeToolSchemasOnWrongAdapterFails: only the Responses adapter
// reads sanitize_tool_schemas. Set anywhere else it would vanish silently
// into a client that never looks at it, so it is rejected loudly instead —
// the same rule responses_path, omit_response_params, no_prompt_cache_key,
// and cache_ttl follow.
func TestSanitizeToolSchemasOnWrongAdapterFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		p    Provider
	}{
		{"openai-compat", "mycompat", Provider{Type: TypeOpenAICompat, BaseURL: "http://x", SanitizeToolSchemas: true}},
		{"native anthropic", "anthropic", Provider{SanitizeToolSchemas: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Providers: map[string]Provider{tc.key: tc.p}}
			_, err := mergeAndValidate(c, &Config{})
			if err == nil {
				t.Fatalf("mergeAndValidate accepted sanitize_tool_schemas on a %s entry", tc.name)
			}
			if !strings.Contains(err.Error(), "sanitize_tool_schemas") {
				t.Errorf("error %q does not name the offending field", err)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name the offending key", err)
			}
		})
	}
}

// TestSanitizeToolSchemasFalseIsAlwaysValid: the default (false/absent) is
// valid on every provider type — the check only fires when the flag is set.
func TestSanitizeToolSchemasFalseIsAlwaysValid(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"mycompat": {Type: TypeOpenAICompat, BaseURL: "http://x"},
	}}
	if _, err := mergeAndValidate(c, &Config{}); err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
}

// TestMergeProviderSanitizeToolSchemasNonClearable: sanitize_tool_schemas is
// non-clearable like NoPromptCacheKey — a project layer can set it to true,
// but an override layer that leaves it false/absent must not clear an
// inherited true.
func TestMergeProviderSanitizeToolSchemasNonClearable(t *testing.T) {
	base := &Config{Providers: map[string]Provider{
		"openai": {SanitizeToolSchemas: true},
	}}
	over := &Config{Providers: map[string]Provider{
		"openai": {},
	}}
	merged, err := mergeAndValidate(base, over)
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	if !merged.Providers["openai"].SanitizeToolSchemas {
		t.Error("SanitizeToolSchemas = false, want true inherited from base layer")
	}
}

// TestMergeProviderSanitizeToolSchemasProjectCanSetTrue: an override layer
// can flip an unset base to true.
func TestMergeProviderSanitizeToolSchemasProjectCanSetTrue(t *testing.T) {
	base := &Config{Providers: map[string]Provider{
		"openai": {},
	}}
	over := &Config{Providers: map[string]Provider{
		"openai": {SanitizeToolSchemas: true},
	}}
	merged, err := mergeAndValidate(base, over)
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	if !merged.Providers["openai"].SanitizeToolSchemas {
		t.Error("SanitizeToolSchemas = false, want true set by override layer")
	}
}
