package engine

import (
	"context"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestSetEffortRidesRequest: SetEffort sets the level carried on the next
// provider.Request. It drives the real Prompt path and inspects the request via
// OnRequest, the same hook production observers use.
func TestSetEffortRidesRequest(t *testing.T) {
	var gotEffort message.Effort
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	}
	cfg.OnRequest = func(_ int, req *provider.Request) { gotEffort = req.Effort }
	s := NewSession(cfg)

	s.SetEffort(message.EffortHigh)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if gotEffort != message.EffortHigh {
		t.Fatalf("request effort = %q, want high", gotEffort)
	}
}

// TestSetEffortNoopEmitsNothing: a set to the current level changes nothing and
// emits no event (the surplus-direction guard against a redundant emit).
func TestSetEffortNoopEmitsNothing(t *testing.T) {
	var changes int
	prov := &scriptedProvider{name: "test"}
	cfg := Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		Effort:    message.EffortMedium,
	}
	cfg.OnEvent = func(ev Event) {
		if ev.Type == EventEffortChanged {
			changes++
		}
	}
	s := NewSession(cfg)
	s.SetEffort(message.EffortMedium) // no-op: already medium
	if changes != 0 {
		t.Fatalf("EventEffortChanged fired %d times on no-op set, want 0", changes)
	}
	s.SetEffort(message.EffortHigh) // real change
	if changes != 1 {
		t.Fatalf("EventEffortChanged fired %d times, want 1", changes)
	}
}

// TestPersistEffortChangeReplay: a SetEffort change survives LoadSession — the
// production resume path, not a hand-built replay.
func TestPersistEffortChangeReplay(t *testing.T) {
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
	s.SetEffort(message.EffortLow)
	if _, err := s.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Effort() != message.EffortLow {
		t.Errorf("loaded effort = %q, want low", loaded.Effort())
	}
}

// TestEffortInitialValuePersists: a Config.Effort set at create time survives a
// LoadSession round trip through the session header record.
func TestEffortInitialValuePersists(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "one"}),
	}}
	cfg := persistCfg(dir, prov)
	cfg.Effort = message.EffortMedium
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}

	// Reload with a cfg that does NOT set Effort — the header must restore it.
	reloadCfg := persistCfg(dir, prov)
	loaded, err := LoadSession(reloadCfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Effort() != message.EffortMedium {
		t.Errorf("loaded effort = %q, want medium (from header)", loaded.Effort())
	}
}
