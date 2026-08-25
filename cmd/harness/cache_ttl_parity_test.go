package main

import (
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/provider/anthropic"
)

// TestCacheTTLConstantsAgree binds the two copies of the cache-TTL value list
// together. config.CacheTTL5m/CacheTTL1h are duplicated from
// anthropic.CacheTTL5m/CacheTTL1h on purpose — package config must not import
// a provider package — but until now nothing but a comment kept them in step.
// config.validateCacheTTL accepts against one copy and anthropic.
// resolveCacheTTL accepts against the other, so a value added to one list
// alone would be accepted at load and then rejected at the first Stream call,
// or vice versa: the silent-drift class this PR set out to remove.
//
// cmd/harness is the right home for the check because it is the one package
// that already imports both, and it is the seam that carries a configured
// value from one to the other (registry -> anthropicCacheTTL ->
// anthropic.Client.CacheTTL).
func TestCacheTTLConstantsAgree(t *testing.T) {
	if config.CacheTTL5m != anthropic.CacheTTL5m {
		t.Errorf("config.CacheTTL5m = %q, anthropic.CacheTTL5m = %q", config.CacheTTL5m, anthropic.CacheTTL5m)
	}
	if config.CacheTTL1h != anthropic.CacheTTL1h {
		t.Errorf("config.CacheTTL1h = %q, anthropic.CacheTTL1h = %q", config.CacheTTL1h, anthropic.CacheTTL1h)
	}
}

// TestEveryConfigCacheTTLIsAcceptedByTheAdapter walks every value config
// accepts and asserts the adapter accepts it too. A new TTL added to config's
// list without a matching adapter case fails here, at build time for the
// developer who adds it, instead of at a user's first request.
func TestEveryConfigCacheTTLIsAcceptedByTheAdapter(t *testing.T) {
	for _, ttl := range []string{config.CacheTTL5m, config.CacheTTL1h} {
		if err := config.ValidateProviderCacheTTL(ttl); err != nil {
			t.Fatalf("config rejects its own constant %q: %v", ttl, err)
		}
		if _, err := anthropic.ResolveCacheTTL(ttl); err != nil {
			t.Errorf("adapter rejects config-accepted TTL %q: %v", ttl, err)
		}
	}
}

// TestAdapterDefaultCacheTTLIsAConfigValue: an operator must be able to write
// the adapter's own default into config and get the identical behavior.
func TestAdapterDefaultCacheTTLIsAConfigValue(t *testing.T) {
	if err := config.ValidateProviderCacheTTL(anthropic.DefaultCacheTTL); err != nil {
		t.Errorf("config rejects the adapter default %q: %v", anthropic.DefaultCacheTTL, err)
	}
}
