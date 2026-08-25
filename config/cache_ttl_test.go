package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestProviderCacheTTLLoads: a providers entry may carry cache_ttl, and Load
// keeps the value verbatim for cmd/harness to hand to the anthropic adapter.
func TestProviderCacheTTLLoads(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, `{"providers": {"anthropic": {"api_key_env": "K", "cache_ttl": "5m"}}}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Providers["anthropic"].CacheTTL; got != "5m" {
		t.Errorf("CacheTTL = %q, want %q", got, "5m")
	}
}

// TestProviderCacheTTLDefaultsEmpty: an entry with no cache_ttl leaves the
// field empty, so the adapter's own DefaultCacheTTL (1h) applies.
func TestProviderCacheTTLDefaultsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, `{"providers": {"anthropic": {"api_key_env": "K"}}}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Providers["anthropic"].CacheTTL; got != "" {
		t.Errorf("CacheTTL = %q, want empty", got)
	}
}

// TestProviderCacheTTLRejectsUnknownValue: a typo'd TTL fails the merged-config
// validation loudly, naming the provider and the valid values. A silent
// fallback would ship different cache economics than the operator asked for.
func TestProviderCacheTTLRejectsUnknownValue(t *testing.T) {
	err := validateProviders(map[string]Provider{
		"anthropic": {CacheTTL: "30m"},
	})
	if err == nil {
		t.Fatal("validateProviders with cache_ttl 30m = nil error, want error")
	}
	for _, want := range []string{"anthropic", "cache_ttl", "5m", "1h"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestProviderCacheTTLRejectedOnNonAnthropicEntry: cache_ttl only reaches the
// anthropic adapter, so setting it anywhere else fails Load rather than being
// silently ignored.
func TestProviderCacheTTLRejectedOnNonAnthropicEntry(t *testing.T) {
	err := validateProviders(map[string]Provider{
		"bifrost": {Type: TypeOpenAICompat, BaseURL: "http://x", CacheTTL: "1h"},
	})
	if err == nil {
		t.Fatal("validateProviders with cache_ttl on a compat entry = nil error, want error")
	}
	if !strings.Contains(err.Error(), "cache_ttl") {
		t.Errorf("error %q does not name cache_ttl", err)
	}
}

// TestProviderCacheTTLMergesFromProject: a project override may set cache_ttl
// without restating the rest of the entry, like base_url and api_key_env.
func TestProviderCacheTTLMergesFromProject(t *testing.T) {
	user := &Config{Providers: map[string]Provider{
		"anthropic": {APIKeyEnv: "K", BaseURL: "http://x"},
	}}
	proj := &Config{Providers: map[string]Provider{
		"anthropic": {CacheTTL: "5m"},
	}}
	got, err := mergeAndValidate(user, proj)
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	p := got.Providers["anthropic"]
	if p.CacheTTL != "5m" {
		t.Errorf("CacheTTL = %q, want %q", p.CacheTTL, "5m")
	}
	if p.APIKeyEnv != "K" || p.BaseURL != "http://x" {
		t.Errorf("merge lost sibling fields: %+v", p)
	}
}

// TestCacheTTLRejectedOnCompatTypedAnthropicKey: validateCacheTTL must gate on
// provider IDENTITY, not on the map key alone. An entry keyed "anthropic" but
// typed openai-compat builds an openaicompat client (cmd/harness's
// registerOpenAICompatProviders overwrites the native one under that key), so
// nothing would ever read its cache_ttl. Rejecting a name match that is not
// the native adapter keeps the value from reaching a client that ignores it.
func TestCacheTTLRejectedOnCompatTypedAnthropicKey(t *testing.T) {
	err := validateProviders(map[string]Provider{
		"anthropic": {Type: TypeOpenAICompat, BaseURL: "http://x", CacheTTL: "1h"},
	})
	if err == nil {
		t.Fatal("validateProviders with cache_ttl on a compat-typed anthropic entry = nil error, want error")
	}
	if !strings.Contains(err.Error(), "cache_ttl") {
		t.Errorf("error %q does not name cache_ttl", err)
	}
}

// TestNoPromptCacheKeyLoadsOnCompatEntry: an openai-compat entry may suppress
// the prompt_cache_key wire field for a strict upstream.
func TestNoPromptCacheKeyLoadsOnCompatEntry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, `{"providers": {"strict": {"type": "openai-compat", "base_url": "http://x", "no_prompt_cache_key": true}}}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Providers["strict"].NoPromptCacheKey {
		t.Error("NoPromptCacheKey = false, want true")
	}
}

// TestNoPromptCacheKeyRejectedOnNativeEntry: only the openaicompat adapter
// reads the flag. The native openai (Responses) adapter always sends
// prompt_cache_key — its own documented field — so the flag is rejected there
// rather than silently ignored, the same rule cache_ttl follows.
func TestNoPromptCacheKeyRejectedOnNativeEntry(t *testing.T) {
	for _, name := range []string{"anthropic", "openai"} {
		err := validateProviders(map[string]Provider{name: {NoPromptCacheKey: true}})
		if err == nil {
			t.Fatalf("providers.%s with no_prompt_cache_key = nil error, want error", name)
		}
		if !strings.Contains(err.Error(), "no_prompt_cache_key") {
			t.Errorf("error %q does not name no_prompt_cache_key", err)
		}
	}
}

// TestNoPromptCacheKeyMergesFromProject: a project override may flip the flag
// without restating the entry, like cache_ttl above.
func TestNoPromptCacheKeyMergesFromProject(t *testing.T) {
	user := &Config{Providers: map[string]Provider{
		"strict": {Type: TypeOpenAICompat, BaseURL: "http://x"},
	}}
	proj := &Config{Providers: map[string]Provider{
		"strict": {NoPromptCacheKey: true},
	}}
	got, err := mergeAndValidate(user, proj)
	if err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
	p := got.Providers["strict"]
	if !p.NoPromptCacheKey {
		t.Error("NoPromptCacheKey = false, want true after merge")
	}
	if p.BaseURL != "http://x" {
		t.Errorf("merge lost sibling fields: %+v", p)
	}
}
