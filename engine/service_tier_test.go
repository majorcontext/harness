package engine

import (
	"context"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestSetServiceTierRidesRequest: SetServiceTier sets the value carried on
// the next provider.Request. It drives the real Prompt path and inspects the
// request via OnRequest, the same hook production observers use. Mirrors
// TestSetEffortRidesRequest (effort_test.go).
func TestSetServiceTierRidesRequest(t *testing.T) {
	var got string
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	}
	cfg.OnRequest = func(_ string, _ int, req *provider.Request) { got = req.ServiceTier }
	s := NewSession(cfg)

	s.SetServiceTier("fast")
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if got != "fast" {
		t.Fatalf("request service tier = %q, want fast", got)
	}
}

// TestSetServiceTierNoopEmitsNothing: a set to the current value changes
// nothing and emits no event (the surplus-direction guard against a
// redundant emit). Mirrors TestSetEffortNoopEmitsNothing.
func TestSetServiceTierNoopEmitsNothing(t *testing.T) {
	var changes int
	prov := &scriptedProvider{name: "test"}
	cfg := Config{
		Providers:   provider.Registry{"test": prov},
		Model:       message.ModelRef{Provider: "test", Model: "m1"},
		ServiceTier: "standard",
	}
	cfg.OnEvent = func(ev Event) {
		if ev.Type == EventServiceTierChanged {
			changes++
		}
	}
	s := NewSession(cfg)
	s.SetServiceTier("standard") // no-op: already standard
	if changes != 0 {
		t.Fatalf("EventServiceTierChanged fired %d times on no-op set, want 0", changes)
	}
	s.SetServiceTier("fast") // real change
	if changes != 1 {
		t.Fatalf("EventServiceTierChanged fired %d times, want 1", changes)
	}
}

// TestPersistServiceTierChangeReplay: a SetServiceTier change survives
// LoadSession — the production resume path, not a hand-built replay. Mirrors
// TestPersistEffortChangeReplay.
func TestPersistServiceTierChangeReplay(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "one"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "two"}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)

	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	s.SetServiceTier("fast")
	if _, err := s.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ServiceTier() != "fast" {
		t.Errorf("loaded service tier = %q, want fast", loaded.ServiceTier())
	}
}

// TestServiceTierInitialValuePersists: a Config.ServiceTier set at create
// time survives a LoadSession round trip through the session header record.
// Mirrors TestEffortInitialValuePersists.
func TestServiceTierInitialValuePersists(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "one"}),
	}}
	cfg := persistCfg(dir, prov)
	cfg.ServiceTier = "ultrafast"
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}

	// Reload with a cfg that does NOT set ServiceTier — the header must
	// restore it.
	reloadCfg := persistCfg(dir, prov)
	loaded, err := LoadSession(reloadCfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ServiceTier() != "ultrafast" {
		t.Errorf("loaded service tier = %q, want ultrafast (from header)", loaded.ServiceTier())
	}
}
