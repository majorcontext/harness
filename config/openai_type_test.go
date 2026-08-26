package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadProviderTypeOpenAI: an entry typed "openai" under an arbitrary
// providers-map key is accepted and round-trips its fields. This is what
// lets a deployment run a SECOND native Responses-API provider beside the
// bare "openai" key — pointing at a different endpoint, with its own
// request path — instead of being limited to the one built-in key.
func TestLoadProviderTypeOpenAI(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, `{
		"providers": {
			"secondary": {
				"type": "openai",
				"base_url": "https://gateway.example",
				"api_key_env": "SECONDARY_API_KEY",
				"responses_path": "/alt/responses"
			}
		}
	}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pr, ok := c.Providers["secondary"]
	if !ok {
		t.Fatal("providers.secondary missing")
	}
	if pr.Type != TypeOpenAI {
		t.Errorf("Type = %q, want %q", pr.Type, TypeOpenAI)
	}
	if pr.BaseURL != "https://gateway.example" {
		t.Errorf("BaseURL = %q", pr.BaseURL)
	}
	if pr.ResponsesPath != "/alt/responses" {
		t.Errorf("ResponsesPath = %q", pr.ResponsesPath)
	}
	if _, err := mergeAndValidate(c, &Config{}); err != nil {
		t.Errorf("mergeAndValidate: %v", err)
	}
}

// TestLoadProviderTypeOpenAIMissingBaseURLFails: an arbitrary key has no
// sensible built-in endpoint to fall back on, so base_url is required —
// the same rule openai-compat already enforces, for the same reason. The
// bare "openai" key keeps its built-in default and is covered separately
// (TestLoadProviderEmptyTypeOnNativeKeysOK).
func TestLoadProviderTypeOpenAIMissingBaseURLFails(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"secondary": {Type: TypeOpenAI},
	}}
	_, err := mergeAndValidate(c, &Config{})
	if err == nil {
		t.Fatal("mergeAndValidate did not fail on missing base_url for type openai")
	}
	if !strings.Contains(err.Error(), "base_url") {
		t.Errorf("error %q does not mention base_url", err)
	}
	if !strings.Contains(err.Error(), "secondary") {
		t.Errorf("error %q does not name the offending key", err)
	}
}

// TestResponsesPathOnNativeOpenAIKeyOK: the bare "openai" key (empty type)
// builds the very same Responses adapter, so it may carry responses_path
// too — the field is gated on the ADAPTER an entry builds, not on its type
// string.
func TestResponsesPathOnNativeOpenAIKeyOK(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"openai": {ResponsesPath: "/alt/responses"},
	}}
	merged, err := mergeAndValidate(c, &Config{})
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	if merged.Providers["openai"].ResponsesPath != "/alt/responses" {
		t.Errorf("ResponsesPath = %q", merged.Providers["openai"].ResponsesPath)
	}
}

// TestResponsesPathOnWrongAdapterFails: only the Responses adapter reads
// responses_path. Set anywhere else it would vanish silently into a client
// that never looks at it, so it is rejected loudly instead — exactly the
// rule no_prompt_cache_key and cache_ttl already follow.
func TestResponsesPathOnWrongAdapterFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		p    Provider
	}{
		{"openai-compat", "mycompat", Provider{Type: TypeOpenAICompat, BaseURL: "http://x", ResponsesPath: "/alt"}},
		{"native anthropic", "anthropic", Provider{ResponsesPath: "/alt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Providers: map[string]Provider{tc.key: tc.p}}
			_, err := mergeAndValidate(c, &Config{})
			if err == nil {
				t.Fatalf("mergeAndValidate accepted responses_path on a %s entry", tc.name)
			}
			if !strings.Contains(err.Error(), "responses_path") {
				t.Errorf("error %q does not name the offending field", err)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name the offending key", err)
			}
		})
	}
}

// TestUnknownTypeErrorListsOpenAI: the unknown-type and empty-type errors
// are a user's only map of what is valid. A new accepted type that never
// reaches those messages is undiscoverable.
//
// Both assertions match the rendered valid-types LIST, not the bare word
// "openai": every one of these messages already says "anthropic"/"openai"
// while describing the native keys, so a substring check for the type name
// alone passes even against code that never learned the type — a vacuous
// guard this test was caught being on its first red-verify run.
func TestUnknownTypeErrorListsOpenAI(t *testing.T) {
	wantList := `"openai-compat", "openai"`

	c := &Config{Providers: map[string]Provider{
		"mystery": {Type: "carrier-pigeon", BaseURL: "http://x"},
	}}
	_, err := mergeAndValidate(c, &Config{})
	if err == nil {
		t.Fatal("mergeAndValidate did not fail on an unknown type")
	}
	if !strings.Contains(err.Error(), wantList) {
		t.Errorf("unknown-type error %q does not list the valid types %s", err, wantList)
	}

	c = &Config{Providers: map[string]Provider{"mycompat": {BaseURL: "http://x"}}}
	_, err = mergeAndValidate(c, &Config{})
	if err == nil {
		t.Fatal("mergeAndValidate did not fail on an empty type for an unknown key")
	}
	if !strings.Contains(err.Error(), wantList) {
		t.Errorf("empty-type error %q does not list the valid types %s", err, wantList)
	}
}

// TestMergeProviderResponsesPath: responses_path layers like every other
// Provider field — a project layer may set it over a user layer.
func TestMergeProviderResponsesPath(t *testing.T) {
	base := &Config{Providers: map[string]Provider{
		"secondary": {Type: TypeOpenAI, BaseURL: "https://gateway.example"},
	}}
	over := &Config{Providers: map[string]Provider{
		"secondary": {ResponsesPath: "/alt/responses"},
	}}
	merged, err := mergeAndValidate(base, over)
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	if got := merged.Providers["secondary"].ResponsesPath; got != "/alt/responses" {
		t.Errorf("ResponsesPath = %q, want the project layer's value", got)
	}
}
