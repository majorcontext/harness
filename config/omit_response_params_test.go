package config

import (
	"strings"
	"testing"
)

// TestOmitResponseParamsOnNativeOpenAIKeyOK: the bare "openai" key (empty
// type) builds the Responses adapter, so it may carry omit_response_params
// too — the field is gated on the ADAPTER an entry builds, not on its type
// string, exactly like ResponsesPath.
func TestOmitResponseParamsOnNativeOpenAIKeyOK(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"openai": {OmitResponseParams: []string{
			OmitResponseParamMaxOutputTokens,
			OmitResponseParamTemperature,
			OmitResponseParamTopP,
			OmitResponseParamMetadata,
		}},
	}}
	merged, err := mergeAndValidate(c, &Config{})
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	got := merged.Providers["openai"].OmitResponseParams
	want := []string{"max_output_tokens", "temperature", "top_p", "metadata"}
	if len(got) != len(want) {
		t.Fatalf("OmitResponseParams = %v, want %v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("OmitResponseParams[%d] = %q, want %q", i, got[i], v)
		}
	}
}

// TestOmitResponseParamsOnTypeOpenAIKeyOK: a TypeOpenAI entry under an
// arbitrary key builds the same adapter, so it may carry the field too.
func TestOmitResponseParamsOnTypeOpenAIKeyOK(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"secondary": {
			Type:               TypeOpenAI,
			BaseURL:            "https://gateway.example",
			OmitResponseParams: []string{OmitResponseParamMaxOutputTokens},
		},
	}}
	merged, err := mergeAndValidate(c, &Config{})
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	if got := merged.Providers["secondary"].OmitResponseParams; len(got) != 1 || got[0] != "max_output_tokens" {
		t.Errorf("OmitResponseParams = %v, want [max_output_tokens]", got)
	}
}

// TestOmitResponseParamsOnWrongAdapterFails: only the Responses adapter
// reads omit_response_params. Set anywhere else it would vanish silently
// into a client that never looks at it, so it is rejected loudly instead —
// the same rule responses_path, no_prompt_cache_key, and cache_ttl follow.
func TestOmitResponseParamsOnWrongAdapterFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		p    Provider
	}{
		{"openai-compat", "mycompat", Provider{Type: TypeOpenAICompat, BaseURL: "http://x", OmitResponseParams: []string{"max_output_tokens"}}},
		{"native anthropic", "anthropic", Provider{OmitResponseParams: []string{"max_output_tokens"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Providers: map[string]Provider{tc.key: tc.p}}
			_, err := mergeAndValidate(c, &Config{})
			if err == nil {
				t.Fatalf("mergeAndValidate accepted omit_response_params on a %s entry", tc.name)
			}
			if !strings.Contains(err.Error(), "omit_response_params") {
				t.Errorf("error %q does not name the offending field", err)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name the offending key", err)
			}
		})
	}
}

// TestOmitResponseParamsUnknownNameFails: a typo'd param name must not
// silently do nothing — the line between this bounded allowlist and an
// arbitrary-field-deletion mechanism.
func TestOmitResponseParamsUnknownNameFails(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"openai": {OmitResponseParams: []string{"max_output_tokens", "store"}},
	}}
	_, err := mergeAndValidate(c, &Config{})
	if err == nil {
		t.Fatal("mergeAndValidate did not fail on an unknown omit_response_params entry")
	}
	if !strings.Contains(err.Error(), `"store"`) {
		t.Errorf("error %q does not name the offending value", err)
	}
}

// TestMergeProviderOmitResponseParams: omit_response_params layers wholesale
// like SkillsDirs/Plugins, not additively like ExtraHeaders — a project
// layer's non-empty list replaces the user layer's entirely, so a project
// can also SHRINK the set (un-list a param the user layer added), which an
// additive merge could never represent.
func TestMergeProviderOmitResponseParams(t *testing.T) {
	base := &Config{Providers: map[string]Provider{
		"openai": {OmitResponseParams: []string{"temperature", "top_p"}},
	}}
	over := &Config{Providers: map[string]Provider{
		"openai": {OmitResponseParams: []string{"max_output_tokens"}},
	}}
	merged, err := mergeAndValidate(base, over)
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	got := merged.Providers["openai"].OmitResponseParams
	if len(got) != 1 || got[0] != "max_output_tokens" {
		t.Errorf("OmitResponseParams = %v, want [max_output_tokens] (project layer replaces wholesale)", got)
	}
}

// TestMergeProviderOmitResponseParamsInheritsWhenOverEmpty: an override
// layer that names no omit_response_params must inherit the base layer's
// list unchanged, exactly as ResponsesPath and every other Provider field
// does when the override leaves it zero.
func TestMergeProviderOmitResponseParamsInheritsWhenOverEmpty(t *testing.T) {
	base := &Config{Providers: map[string]Provider{
		"openai": {OmitResponseParams: []string{"temperature", "top_p"}},
	}}
	over := &Config{Providers: map[string]Provider{
		"openai": {},
	}}
	merged, err := mergeAndValidate(base, over)
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	got := merged.Providers["openai"].OmitResponseParams
	if len(got) != 2 || got[0] != "temperature" || got[1] != "top_p" {
		t.Errorf("OmitResponseParams = %v, want [temperature top_p]", got)
	}
}
