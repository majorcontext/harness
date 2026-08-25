package main

import (
	"slices"
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

// TestCacheTTLAcceptedSetsAreIdentical compares the two sides' OWN accepted
// sets — config.CacheTTLValues and anthropic.CacheTTLValues, each of which
// its validator derives from — rather than walking a third, manually
// maintained list here. A TTL added to only one side changes that side's
// exported set and fails the equality check; a hardcoded slice in this test
// would have stayed green through exactly that drift (it could only ever
// prove the values it happened to name).
func TestCacheTTLAcceptedSetsAreIdentical(t *testing.T) {
	cfg := config.CacheTTLValues()
	adp := anthropic.CacheTTLValues()
	slices.Sort(cfg)
	slices.Sort(adp)
	if !slices.Equal(cfg, adp) {
		t.Fatalf("accepted cache_ttl sets differ: config %q, adapter %q", cfg, adp)
	}
}

// TestEveryAcceptedCacheTTLPassesBothSides walks the union of both exported
// sets and asserts BOTH the config validator and the adapter resolver accept
// every element — the direct probe that no value is accepted at load and
// rejected at the first Stream call, or vice versa. Together with the
// set-equality test above this holds even if a future change breaks a
// validator's derivation from its own exported list.
func TestEveryAcceptedCacheTTLPassesBothSides(t *testing.T) {
	union := append(config.CacheTTLValues(), anthropic.CacheTTLValues()...)
	slices.Sort(union)
	for _, ttl := range slices.Compact(union) {
		if err := config.ValidateProviderCacheTTL(ttl); err != nil {
			t.Errorf("config rejects accepted TTL %q: %v", ttl, err)
		}
		if _, err := anthropic.ResolveCacheTTL(ttl); err != nil {
			t.Errorf("adapter rejects accepted TTL %q: %v", ttl, err)
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
