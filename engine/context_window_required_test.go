package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// A registry MISS and a registry HIT, both through the REAL modelmeta
// table: these tests are about what the shipped registry does or does not
// know, so stubbing the lookup would test nothing.
var (
	unknownRef = message.ModelRef{Provider: "openrouter", Model: "anthropic/claude-opus-4.1"}
	knownRef   = message.ModelRef{Provider: "openai", Model: "gpt-5.6-sol"}
)

// TestResolveContextWindowReportsRegistryMiss is the core of the fix: an
// unrecognized model must be REPORTED as a miss, not folded silently into
// the same "disabled" answer a deliberate opt-out produces. Silently
// running an unknown model with no context management is how a session
// dies with "context exhausted" instead of compacting.
func TestResolveContextWindowReportsRegistryMiss(t *testing.T) {
	if _, _, err := resolveContextWindow(0, knownRef); err != nil {
		t.Errorf("known model %s reported a miss: %v", knownRef, err)
	}
	tokens, source, err := resolveContextWindow(0, unknownRef)
	if err == nil {
		t.Fatalf("unknown model %s resolved silently to %d/%q, want a reported miss", unknownRef, tokens, source)
	}
	if !errors.Is(err, ErrUnknownContextWindow) {
		t.Errorf("error = %v, want it to wrap ErrUnknownContextWindow", err)
	}
	if !strings.Contains(err.Error(), unknownRef.String()) {
		t.Errorf("error = %q, want it to name the offending model ref %q", err, unknownRef.String())
	}
}

// TestResolveContextWindowLegitimateDisabledCases pins what must stay
// allowed. Only a registry miss on a real model ref is a failure.
func TestResolveContextWindowLegitimateDisabledCases(t *testing.T) {
	t.Run("explicit operator window wins over an unknown model", func(t *testing.T) {
		tokens, source, err := resolveContextWindow(400_000, unknownRef)
		if err != nil {
			t.Errorf("explicit window still reported a miss: %v", err)
		}
		if tokens != 400_000 || source != contextWindowSourceConfig {
			t.Errorf("= %d, %q; want 400000, %q", tokens, source, contextWindowSourceConfig)
		}
	})
	t.Run("explicit negative is an opt-out", func(t *testing.T) {
		tokens, source, err := resolveContextWindow(-1, unknownRef)
		if err != nil {
			t.Errorf("explicit opt-out reported a miss: %v", err)
		}
		if tokens != 0 || source != contextWindowSourceOptOut {
			t.Errorf("= %d, %q; want 0, %q", tokens, source, contextWindowSourceOptOut)
		}
	})
	t.Run("no model at all is nothing to look up", func(t *testing.T) {
		if _, _, err := resolveContextWindow(0, message.ModelRef{}); err != nil {
			t.Errorf("zero model ref reported a miss: %v", err)
		}
	})
	t.Run("a known model below the auto floor stays allowed", func(t *testing.T) {
		stubContextWindowLookup(t, testContextWindowTable())
		tokens, source, err := resolveContextWindow(0, modelBogusTiny)
		if err != nil {
			t.Errorf("a known-but-small model reported a miss: %v", err)
		}
		if tokens != 0 || source != contextWindowSourceDisabled {
			t.Errorf("= %d, %q; want 0, %q", tokens, source, contextWindowSourceDisabled)
		}
	})
}

func requireCfg(prov *scriptedProvider, ref message.ModelRef) Config {
	return Config{
		Providers:            provider.Registry{prov.name: prov},
		Model:                ref,
		RequireContextWindow: true,
	}
}

// TestPromptRefusesUnknownModelWhenRequired is the loud failure itself: a
// session whose model has no known context window must not run. Before
// this, it started happily with source=disabled and no context management
// at all.
func TestPromptRefusesUnknownModelWhenRequired(t *testing.T) {
	prov := &scriptedProvider{name: "openrouter", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "should never run"}),
	}}
	s := NewSession(requireCfg(prov, unknownRef))
	if err := s.ContextWindowErr(); err == nil {
		t.Fatal("NewSession reported no context-window error for an unknown model")
	}

	_, err := s.Prompt(context.Background(), "go")
	if err == nil {
		t.Fatal("Prompt succeeded for a model with no known context window, want a loud failure")
	}
	if !errors.Is(err, ErrUnknownContextWindow) {
		t.Errorf("error = %v, want it to wrap ErrUnknownContextWindow", err)
	}
	if !strings.Contains(err.Error(), unknownRef.String()) {
		t.Errorf("error = %q, want it to name %q", err, unknownRef.String())
	}
	// It must fail BEFORE touching history or the provider.
	if n := len(s.History()); n != 0 {
		t.Errorf("history = %d messages, want 0: the refusal must precede any append", n)
	}
	if prov.call != 0 {
		t.Errorf("provider called %d times, want 0", prov.call)
	}
}

// TestPromptAcceptsKnownModelWhenRequired is the other half: the check must
// not disturb a model the registry knows.
func TestPromptAcceptsKnownModelWhenRequired(t *testing.T) {
	prov := &scriptedProvider{name: "openai", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	s := NewSession(requireCfg(prov, knownRef))
	if err := s.ContextWindowErr(); err != nil {
		t.Fatalf("known model reported a context-window error: %v", err)
	}
	if s.contextWindowSource != contextWindowSourceModelDerived {
		t.Errorf("source = %q, want %q", s.contextWindowSource, contextWindowSourceModelDerived)
	}
	if got := s.cfg.ContextWindowTokens; got != 1_050_000 {
		t.Errorf("context window = %d, want 1050000", got)
	}
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
}

// TestUnknownModelRunsWhenNotRequired pins the engine's zero value: an
// embedder building a bare engine.Config (and every test in this package)
// keeps the pre-fix behavior. The config/CLI layer is what turns the
// requirement on.
func TestUnknownModelRunsWhenNotRequired(t *testing.T) {
	prov := &scriptedProvider{name: "openrouter", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := requireCfg(prov, unknownRef)
	cfg.RequireContextWindow = false
	s := NewSession(cfg)
	if err := s.ContextWindowErr(); err != nil {
		t.Fatalf("ContextWindowErr = %v, want nil when the requirement is off", err)
	}
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
}

// TestExplicitWindowSatisfiesTheRequirement pins the operator escape hatch:
// naming the window explicitly is exactly the missing information, so it
// must satisfy the requirement for any model.
func TestExplicitWindowSatisfiesTheRequirement(t *testing.T) {
	prov := &scriptedProvider{name: "openrouter", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := requireCfg(prov, unknownRef)
	cfg.ContextWindowTokens = 200_000
	s := NewSession(cfg)
	if err := s.ContextWindowErr(); err != nil {
		t.Fatalf("ContextWindowErr = %v, want nil with an explicit window", err)
	}
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
}

// TestSetModelToUnknownModelIsRefused covers the model-set route: a switch
// is the other point where a session starts calling a model, and
// CheckModel is the pre-set gate every SetModel route consults (the same
// shape as ModelSupported).
func TestSetModelToUnknownModelIsRefused(t *testing.T) {
	prov := &scriptedProvider{name: "openai", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := requireCfg(prov, knownRef)
	cfg.Providers["openrouter"] = &scriptedProvider{name: "openrouter"}
	s := NewSession(cfg)

	if err := s.CheckModel(knownRef); err != nil {
		t.Errorf("CheckModel(known) = %v, want nil", err)
	}
	err := s.CheckModel(unknownRef)
	if err == nil {
		t.Fatalf("CheckModel(%s) = nil, want a loud refusal", unknownRef)
	}
	if !errors.Is(err, ErrUnknownContextWindow) || !strings.Contains(err.Error(), unknownRef.String()) {
		t.Errorf("error = %v, want ErrUnknownContextWindow naming %q", err, unknownRef.String())
	}

	// A route that sets it anyway must not leave the session silently
	// running with no context management: the next Prompt fails loudly.
	s.SetModel(unknownRef)
	if _, err := s.Prompt(context.Background(), "go"); !errors.Is(err, ErrUnknownContextWindow) {
		t.Errorf("Prompt after switching to an unknown model = %v, want ErrUnknownContextWindow", err)
	}
}

var (
	firerouterRef = message.ModelRef{Provider: "bifrost", Model: "fireworks/accounts/fireworks/routers/firerouter"}
	claudeCodeRef = message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "opus"}
)

func TestSessionContextWindowTokensHidesGauge(t *testing.T) {
	cases := []struct {
		ref          message.ModelRef
		wantCfgArmed bool
	}{
		{firerouterRef, true},
		{claudeCodeRef, false},
	}
	for _, c := range cases {
		s := NewSession(Config{Model: c.ref})
		if armed := s.cfg.ContextWindowTokens != 0; armed != c.wantCfgArmed {
			t.Errorf("%v: cfg.ContextWindowTokens armed = %v, want %v", c.ref, armed, c.wantCfgArmed)
		}
		if err := s.ContextWindowErr(); err != nil {
			t.Errorf("%v: ContextWindowErr() = %v, want nil", c.ref, err)
		}
		if got := s.ContextWindowTokens(); got != 0 {
			t.Errorf("%v: ContextWindowTokens() = %d, want 0", c.ref, got)
		}
	}
}

// TestBeginClaudeCodeContextUsageTurnClearsPriorReading: an unanswered turn reports unknown, not a stale reading.
func TestBeginClaudeCodeContextUsageTurnClearsPriorReading(t *testing.T) {
	s := NewSession(Config{
		Model:     claudeCodeRef,
		Providers: provider.Registry{"test": &scriptedProvider{name: "test"}},
	})
	s.setClaudeCodeContextUsage(s.beginClaudeCodeContextUsageTurn(), 15_554, 1_000_000)
	if _, ok := s.ContextUsedTokens(); !ok {
		t.Fatal("setup: first reading did not apply")
	}

	s.beginClaudeCodeContextUsageTurn() // a new turn starts; its own response never arrives
	if _, ok := s.ContextUsedTokens(); ok {
		t.Error("ContextUsedTokens() still ok after a new turn began with no response yet, want cleared")
	}
}

// TestContextGauge pins its two lanes: a live claude-code reading, and the native LastUsage fallback.
func TestContextGauge(t *testing.T) {
	prov := provider.Registry{"test": &scriptedProvider{name: "test"}}

	claudeCode := NewSession(Config{Model: claudeCodeRef, Providers: prov})
	claudeCode.setClaudeCodeContextUsage(claudeCode.beginClaudeCodeContextUsageTurn(), 15_554, 1_000_000)
	if window, used := claudeCode.ContextGauge(); window != 1_000_000 || used != 15_554 {
		t.Errorf("claude-code: ContextGauge() = %d, %d; want 1000000, 15554", window, used)
	}

	native := NewSession(Config{
		Model:               message.ModelRef{Provider: "test", Model: "big"},
		Providers:           prov,
		ContextWindowTokens: 500_000,
	})
	native.mu.Lock()
	native.lastUsage = provider.Usage{InputTokens: 100, CacheReadTokens: 20, CacheWriteTokens: 5}
	native.haveLastUsage = true
	native.mu.Unlock()
	if window, used := native.ContextGauge(); window != 500_000 || used != 125 {
		t.Errorf("native: ContextGauge() = %d, %d; want 500000, 125", window, used)
	}
}

func TestSetClaudeCodeContextUsage(t *testing.T) {
	s := NewSession(Config{
		Model:     claudeCodeRef,
		Providers: provider.Registry{"test": &scriptedProvider{name: "test"}},
	})
	gen := s.beginClaudeCodeContextUsageTurn()
	s.setClaudeCodeContextUsage(gen, 15_554, 1_000_000)

	if got := s.ContextWindowTokens(); got != 1_000_000 {
		t.Errorf("ContextWindowTokens() = %d, want 1000000", got)
	}
	if used, ok := s.ContextUsedTokens(); !ok || used != 15_554 {
		t.Errorf("ContextUsedTokens() = %d, %v; want 15554, true", used, ok)
	}

	s.SetModel(message.ModelRef{Provider: "test", Model: "x"})
	if _, ok := s.ContextUsedTokens(); ok {
		t.Error("ContextUsedTokens() still ok after switching away from claude-code")
	}
}

// TestSetClaudeCodeContextUsageIgnoresStaleAndExplicit: a stale generation, an explicit window, and an opt-out each reject.
func TestSetClaudeCodeContextUsageIgnoresStaleAndExplicit(t *testing.T) {
	prov := provider.Registry{"test": &scriptedProvider{name: "test"}}

	stale := NewSession(Config{Model: claudeCodeRef, Providers: prov})
	staleGen := stale.beginClaudeCodeContextUsageTurn()
	stale.SetModel(message.ModelRef{Provider: "test", Model: "x"})
	stale.setClaudeCodeContextUsage(staleGen, 15_554, 1_000_000)

	explicit := NewSession(Config{Model: claudeCodeRef, ContextWindowTokens: 42_000, Providers: prov})
	explicit.setClaudeCodeContextUsage(explicit.beginClaudeCodeContextUsageTurn(), 15_554, 1_000_000)

	optOut := NewSession(Config{Model: claudeCodeRef, ContextWindowTokens: -1, Providers: prov})
	optOut.setClaudeCodeContextUsage(optOut.beginClaudeCodeContextUsageTurn(), 15_554, 1_000_000)

	for name, s := range map[string]*Session{"stale": stale, "explicit": explicit, "opt-out": optOut} {
		if _, ok := s.ContextUsedTokens(); ok {
			t.Errorf("%s: ContextUsedTokens() ok, want not ok", name)
		}
	}
}

// TestSetClaudeCodeContextUsageIgnoresStaleAfterSwitchingBack: a provider match alone is not enough after SetModel returns to the SAME ref.
func TestSetClaudeCodeContextUsageIgnoresStaleAfterSwitchingBack(t *testing.T) {
	s := NewSession(Config{
		Model:     claudeCodeRef,
		Providers: provider.Registry{"test": &scriptedProvider{name: "test"}},
	})
	staleGen := s.beginClaudeCodeContextUsageTurn()

	s.SetModel(message.ModelRef{Provider: "test", Model: "x"})
	s.SetModel(claudeCodeRef)

	s.setClaudeCodeContextUsage(staleGen, 15_554, 1_000_000)
	if _, ok := s.ContextUsedTokens(); ok {
		t.Error("ContextUsedTokens() ok for a stale generation after switching back, want rejected")
	}
}
