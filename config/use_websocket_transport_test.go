package config

import (
	"strings"
	"testing"
)

// UseWebSocketTransport follows the same buildsResponsesAdapter gating and
// non-clearable merge semantics as OmitResponseParams/NoPromptCacheKey —
// see omit_response_params_test.go and cache_ttl_test.go for the sibling
// fields this mirrors.

func TestUseWebSocketTransportOnNativeOpenAIKeyOK(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"openai": {UseWebSocketTransport: true},
	}}
	merged, err := mergeAndValidate(c, &Config{})
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	if !merged.Providers["openai"].UseWebSocketTransport {
		t.Error("UseWebSocketTransport = false, want true")
	}
}

func TestUseWebSocketTransportOnTypeOpenAIKeyOK(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"secondary": {
			Type:                  TypeOpenAI,
			BaseURL:               "https://gateway.example",
			UseWebSocketTransport: true,
		},
	}}
	merged, err := mergeAndValidate(c, &Config{})
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	if !merged.Providers["secondary"].UseWebSocketTransport {
		t.Error("UseWebSocketTransport = false, want true")
	}
}

// TestUseWebSocketTransportOnWrongAdapterFails: only the Responses adapter
// reads use_websocket_transport — set anywhere else it would vanish
// silently into a client that never looks at it, the same rule
// omit_response_params and responses_path follow.
func TestUseWebSocketTransportOnWrongAdapterFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		p    Provider
	}{
		{"openai-compat", "mycompat", Provider{Type: TypeOpenAICompat, BaseURL: "http://x", UseWebSocketTransport: true}},
		{"native anthropic", "anthropic", Provider{UseWebSocketTransport: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Providers: map[string]Provider{tc.key: tc.p}}
			_, err := mergeAndValidate(c, &Config{})
			if err == nil {
				t.Fatalf("mergeAndValidate accepted use_websocket_transport on a %s entry", tc.name)
			}
			if !strings.Contains(err.Error(), "use_websocket_transport") {
				t.Errorf("error %q does not name the offending field", err)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name the offending key", err)
			}
		})
	}
}

// TestUseWebSocketTransportFalseIsAlwaysValid: the default is never
// rejected on any adapter — only an explicit true is gated.
func TestUseWebSocketTransportFalseIsAlwaysValid(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"anthropic": {},
		"mycompat":  {Type: TypeOpenAICompat, BaseURL: "http://x"},
	}}
	if _, err := mergeAndValidate(c, &Config{}); err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
}

// TestUseWebSocketTransportMergesFromProject: a project override may turn
// the flag on without restating the entry, like no_prompt_cache_key.
func TestUseWebSocketTransportMergesFromProject(t *testing.T) {
	user := &Config{Providers: map[string]Provider{
		"openai": {BaseURL: "https://api.example"},
	}}
	proj := &Config{Providers: map[string]Provider{
		"openai": {UseWebSocketTransport: true},
	}}
	got, err := mergeAndValidate(user, proj)
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	p := got.Providers["openai"]
	if !p.UseWebSocketTransport {
		t.Error("UseWebSocketTransport = false, want true after merge")
	}
	if p.BaseURL != "https://api.example" {
		t.Errorf("merge lost sibling fields: %+v", p)
	}
}

// TestUseWebSocketTransportNonClearable: like NoPromptCacheKey, a project
// layer can turn this on but a later layer cannot turn an inherited true
// back off — there is no *bool escape hatch for it, deliberately (see the
// field's doc comment).
func TestUseWebSocketTransportNonClearable(t *testing.T) {
	base := &Config{Providers: map[string]Provider{
		"openai": {UseWebSocketTransport: true},
	}}
	over := &Config{Providers: map[string]Provider{
		"openai": {BaseURL: "https://api.example"}, // does not restate the flag
	}}
	merged, err := mergeAndValidate(base, over)
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	if !merged.Providers["openai"].UseWebSocketTransport {
		t.Error("UseWebSocketTransport = false, want true (inherited, non-clearable)")
	}
}
