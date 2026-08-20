package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// stubContextWindowLookup replaces modelContextWindowLookup with a fixed
// table for the duration of t, restoring the real modelmeta-backed lookup
// on cleanup. Using a fake table (rather than depending on modelmeta's real,
// evolving data) keeps these tests about the PRECEDENCE/floor/follow-switch
// logic in this file, not about any particular model's actual window.
func stubContextWindowLookup(t *testing.T, table map[message.ModelRef]int) {
	t.Helper()
	orig := modelContextWindowLookup
	modelContextWindowLookup = func(ref message.ModelRef) (int, bool) {
		tokens, ok := table[ref]
		return tokens, ok
	}
	t.Cleanup(func() { modelContextWindowLookup = orig })
}

var (
	modelKnownBig   = message.ModelRef{Provider: "test", Model: "big"}   // 500_000 tokens
	modelKnownSmall = message.ModelRef{Provider: "test", Model: "small"} // 32_000 tokens
	modelBogusTiny  = message.ModelRef{Provider: "test", Model: "bogus"} // 100 tokens, below the floor
	modelUnknown    = message.ModelRef{Provider: "test", Model: "unknown-to-table"}
)

func testContextWindowTable() map[message.ModelRef]int {
	return map[message.ModelRef]int{
		modelKnownBig:   500_000,
		modelKnownSmall: 32_000,
		modelBogusTiny:  100,
	}
}

// --- resolveContextWindow: precedence + floor -----------------------------

func TestResolveContextWindowExplicitConfigWinsOverModel(t *testing.T) {
	stubContextWindowLookup(t, testContextWindowTable())

	tokens, source := resolveContextWindow(50_000, modelKnownBig)
	if tokens != 50_000 || source != contextWindowSourceConfig {
		t.Fatalf("resolveContextWindow(50000, big) = %d, %q; want 50000, %q", tokens, source, contextWindowSourceConfig)
	}
}

func TestResolveContextWindowModelDerivedWhenUnset(t *testing.T) {
	stubContextWindowLookup(t, testContextWindowTable())

	tokens, source := resolveContextWindow(0, modelKnownBig)
	if tokens != 500_000 || source != contextWindowSourceModelDerived {
		t.Fatalf("resolveContextWindow(0, big) = %d, %q; want 500000, %q", tokens, source, contextWindowSourceModelDerived)
	}
}

func TestResolveContextWindowUnknownModelDisabled(t *testing.T) {
	stubContextWindowLookup(t, testContextWindowTable())

	tokens, source := resolveContextWindow(0, modelUnknown)
	if tokens != 0 || source != contextWindowSourceDisabled {
		t.Fatalf("resolveContextWindow(0, unknown) = %d, %q; want 0, %q", tokens, source, contextWindowSourceDisabled)
	}
}

// TestResolveContextWindowFloorRejectsBogusValue is the safety-floor case
// the jumpy-pizza follow-up asked for explicitly: a implausibly small
// model-derived value (well below minAutoContextWindowTokens) must not arm
// a nonsense compaction threshold. Pinned below the floor by construction
// (modelBogusTiny = 100 tokens), so this red-verifies against ANY future
// change to the floor constant, not just today's value.
func TestResolveContextWindowFloorRejectsBogusValue(t *testing.T) {
	stubContextWindowLookup(t, testContextWindowTable())

	tokens, source := resolveContextWindow(0, modelBogusTiny)
	if tokens != 0 || source != contextWindowSourceDisabled {
		t.Fatalf("resolveContextWindow(0, bogus-tiny) = %d, %q; want 0, %q (floor must reject it)", tokens, source, contextWindowSourceDisabled)
	}
}

func TestResolveContextWindowFloorBoundary(t *testing.T) {
	stubContextWindowLookup(t, map[message.ModelRef]int{
		modelKnownSmall: minAutoContextWindowTokens, // exactly at the floor
	})
	tokens, source := resolveContextWindow(0, modelKnownSmall)
	if tokens != minAutoContextWindowTokens || source != contextWindowSourceModelDerived {
		t.Fatalf("resolveContextWindow(0, exactly-floor) = %d, %q; want %d, %q (floor is inclusive)",
			tokens, source, minAutoContextWindowTokens, contextWindowSourceModelDerived)
	}

	stubContextWindowLookup(t, map[message.ModelRef]int{
		modelKnownSmall: minAutoContextWindowTokens - 1,
	})
	tokens, source = resolveContextWindow(0, modelKnownSmall)
	if tokens != 0 || source != contextWindowSourceDisabled {
		t.Fatalf("resolveContextWindow(0, one-under-floor) = %d, %q; want 0, %q", tokens, source, contextWindowSourceDisabled)
	}
}

// --- NewSession wiring ------------------------------------------------------

func TestNewSessionDerivesContextWindowFromModel(t *testing.T) {
	stubContextWindowLookup(t, testContextWindowTable())

	s := NewSession(Config{Model: modelKnownBig})
	if s.cfg.ContextWindowTokens != 500_000 {
		t.Fatalf("cfg.ContextWindowTokens = %d, want 500000", s.cfg.ContextWindowTokens)
	}
	if s.contextWindowSource != contextWindowSourceModelDerived {
		t.Fatalf("contextWindowSource = %q, want %q", s.contextWindowSource, contextWindowSourceModelDerived)
	}
	if s.contextWindowExplicit {
		t.Fatal("contextWindowExplicit = true, want false (ContextWindowTokens was unset)")
	}
}

func TestNewSessionExplicitConfigOverridesModel(t *testing.T) {
	stubContextWindowLookup(t, testContextWindowTable())

	s := NewSession(Config{Model: modelKnownBig, ContextWindowTokens: 999_000})
	if s.cfg.ContextWindowTokens != 999_000 {
		t.Fatalf("cfg.ContextWindowTokens = %d, want 999000 (explicit must win over model metadata)", s.cfg.ContextWindowTokens)
	}
	if s.contextWindowSource != contextWindowSourceConfig {
		t.Fatalf("contextWindowSource = %q, want %q", s.contextWindowSource, contextWindowSourceConfig)
	}
	if !s.contextWindowExplicit {
		t.Fatal("contextWindowExplicit = false, want true (ContextWindowTokens was set)")
	}
}

func TestNewSessionUnknownModelStaysDisabled(t *testing.T) {
	stubContextWindowLookup(t, testContextWindowTable())

	s := NewSession(Config{Model: modelUnknown})
	if s.cfg.ContextWindowTokens != 0 {
		t.Fatalf("cfg.ContextWindowTokens = %d, want 0 (unknown model, no config override)", s.cfg.ContextWindowTokens)
	}
	if s.contextWindowSource != contextWindowSourceDisabled {
		t.Fatalf("contextWindowSource = %q, want %q", s.contextWindowSource, contextWindowSourceDisabled)
	}
}

// --- SetModel: the window follows the active model --------------------------

// TestSetModelFollowsActiveModel is the mid-session-switch case the incident
// follow-up called out explicitly: compaction must not stay armed (or
// disarmed) for a model this session no longer runs.
func TestSetModelFollowsActiveModel(t *testing.T) {
	stubContextWindowLookup(t, testContextWindowTable())

	s := NewSession(Config{
		Model: modelUnknown, // starts disabled: no metadata
		Providers: provider.Registry{
			"test": &scriptedProvider{name: "test"},
		},
	})
	if s.cfg.ContextWindowTokens != 0 {
		t.Fatalf("initial cfg.ContextWindowTokens = %d, want 0", s.cfg.ContextWindowTokens)
	}

	s.SetModel(modelKnownBig)
	if s.cfg.ContextWindowTokens != 500_000 {
		t.Fatalf("after switch to known model, cfg.ContextWindowTokens = %d, want 500000", s.cfg.ContextWindowTokens)
	}
	if s.contextWindowSource != contextWindowSourceModelDerived {
		t.Fatalf("after switch to known model, contextWindowSource = %q, want %q", s.contextWindowSource, contextWindowSourceModelDerived)
	}

	// Switching AWAY from a known model must disarm it again, not leave the
	// stale window from the previous model in place.
	s.SetModel(modelUnknown)
	if s.cfg.ContextWindowTokens != 0 {
		t.Fatalf("after switch back to unknown model, cfg.ContextWindowTokens = %d, want 0 (stale window must not survive the switch)", s.cfg.ContextWindowTokens)
	}
	if s.contextWindowSource != contextWindowSourceDisabled {
		t.Fatalf("after switch back to unknown model, contextWindowSource = %q, want %q", s.contextWindowSource, contextWindowSourceDisabled)
	}
}

// TestSetModelNeverOverridesExplicitConfig: an operator-pinned window must
// survive every model switch for the session's entire lifetime — precedence
// is permanent, not just at session start.
func TestSetModelNeverOverridesExplicitConfig(t *testing.T) {
	stubContextWindowLookup(t, testContextWindowTable())

	s := NewSession(Config{
		Model:               modelUnknown,
		ContextWindowTokens: 42_000,
		Providers: provider.Registry{
			"test": &scriptedProvider{name: "test"},
		},
	})

	s.SetModel(modelKnownBig)
	if s.cfg.ContextWindowTokens != 42_000 {
		t.Fatalf("cfg.ContextWindowTokens = %d, want 42000 (explicit config must survive a model switch)", s.cfg.ContextWindowTokens)
	}
	if s.contextWindowSource != contextWindowSourceConfig {
		t.Fatalf("contextWindowSource = %q, want %q", s.contextWindowSource, contextWindowSourceConfig)
	}
}

// TestSetModelNoOpDoesNotRederive: SetModel to the CURRENT model is already a
// documented no-op (see TestSetModelNoOpEmitsNothing in model_tool_test.go);
// this just confirms the context-window bookkeeping shares that early return
// rather than doing pointless re-derivation work on every no-op call.
func TestSetModelNoOpDoesNotRederive(t *testing.T) {
	calls := 0
	orig := modelContextWindowLookup
	modelContextWindowLookup = func(ref message.ModelRef) (int, bool) {
		calls++
		return orig(ref)
	}
	t.Cleanup(func() { modelContextWindowLookup = orig })

	s := NewSession(Config{Model: modelUnknown})
	callsAfterCreate := calls

	s.SetModel(modelUnknown) // no-op: same model
	if calls != callsAfterCreate {
		t.Fatalf("SetModel no-op called the lookup %d more time(s), want 0", calls-callsAfterCreate)
	}
}

// --- SetModel: the churn-guard must not survive a window change -------------

// TestSetModelClearsStaleHysteresisOnWindowChange is the red-first
// regression test for Finding 4: compactHysteresis means "folding again
// won't relieve pressure at the CURRENT window" (see its doc comment in
// engine.go). SetModel re-derives s.cfg.ContextWindowTokens from the new
// model but left compactHysteresis untouched, so a switch to a
// smaller-window model after an automatic fold left the churn guard
// latched against a window that no longer applies — maybeAutoCompact would
// then see "over threshold" + "on cooldown" and never compact again while
// the prompt stayed above the NEW (smaller) threshold.
//
// Sequence mirrors TestMaybeAutoCompactTriggersAndHysteresisPreventsThrash's
// working shape: a "no lastUsage yet" call, then an "under" call to build a
// 2-turn history without accidentally firing a fold with nothing to fold,
// then an "over" call that actually triggers the first automatic
// compaction (latching compactHysteresis). SetModel then switches to a
// small-window model. A further over-threshold turn must trigger a SECOND
// automatic compaction — which only happens if the switch cleared the
// stale guard.
func TestSetModelClearsStaleHysteresisOnWindowChange(t *testing.T) {
	modelBig := message.ModelRef{Provider: "test", Model: "big-window"}
	modelSmall := message.ModelRef{Provider: "test", Model: "small-window"}
	stubContextWindowLookup(t, map[message.ModelRef]int{
		modelBig:   100_000, // threshold (default 0.8) = 80_000; both clear minAutoContextWindowTokens's 16_000 floor
		modelSmall: 20_000,  // threshold (default 0.8) = 16_000
	})

	under := provider.Usage{InputTokens: 10_000} // under both thresholds
	over := provider.Usage{InputTokens: 85_000}  // over both thresholds

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("t1", under), // call 1: no lastUsage yet, no trigger
		compactTurn("t2", over),  // call 2: lastUsage(t1)=under, no trigger; history now has 2 turns
		compactSummaryTurn("gist-1", provider.Usage{InputTokens: 5}), // triggered before call 3 (lastUsage(t2)=85000 >= 80000)
		compactTurn("t3", over), // call 3's own turn
		// SetModel(modelSmall) happens here, between calls 3 and 4.
		compactSummaryTurn("gist-2", provider.Usage{InputTokens: 5}), // must trigger before call 4 IFF hysteresis was cleared
		compactTurn("t4", over), // call 4's own turn
	}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               modelBig,
		CompactionKeepTurns: 1,
	})

	runTurns(t, s, 3)
	if got := s.CompactionCount(); got != 1 {
		t.Fatalf("CompactionCount after 3 turns = %d, want 1 (first automatic fold)", got)
	}

	s.SetModel(modelSmall)

	runTurns(t, s, 1)
	if got := s.CompactionCount(); got != 2 {
		t.Fatalf("CompactionCount after model switch + 1 more over-threshold turn = %d, want 2 "+
			"(a stale compactHysteresis from the OLD window suppressed the second fold)", got)
	}
	if got := len(prov.requests); got != 6 {
		t.Fatalf("provider calls = %d, want 6 (4 worker turns + 2 compaction summaries)", got)
	}
}

// TestSetModelSameWindowDoesNotLog is the red-first regression test for
// Finding 5: context_window.go's logContextWindowArmed doc comment (and the
// AGENTS.md addendum) promise the "model_switch" INFO line fires only when
// the effective window actually changes, but SetModel logged on every
// non-no-op model change regardless — two models that happen to share the
// same modelmeta-derived window still produce a spurious "model_switch"
// line. Drives the real logger (slog.Default), not a mock, since this is
// specifically about what SetModel decides to log.
func TestSetModelSameWindowDoesNotLog(t *testing.T) {
	modelA := message.ModelRef{Provider: "test", Model: "same-window-a"}
	modelB := message.ModelRef{Provider: "test", Model: "same-window-b"}
	stubContextWindowLookup(t, map[message.ModelRef]int{
		modelA: 500_000,
		modelB: 500_000, // same tokens AND source (model-derived) as modelA
	})

	s := NewSession(Config{
		Model: modelA,
		Providers: provider.Registry{
			"test": &scriptedProvider{name: "test"},
		},
	})

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s.SetModel(modelB) // real model change (ref != s.model), but SAME effective window

	if out := buf.String(); strings.Contains(out, "model_switch") {
		t.Errorf("SetModel(modelB) logged a model_switch line for a same-window switch:\n%s", out)
	}
	if s.cfg.ContextWindowTokens != 500_000 {
		t.Fatalf("cfg.ContextWindowTokens = %d, want 500000 (window value itself is unaffected by log suppression)", s.cfg.ContextWindowTokens)
	}
}

// --- LoadSession: re-derives against the final replayed model ---------------

// TestLoadSessionRederivesAfterModelSwitch: a session that switched models
// before its process restarted must resume with the window for the model it
// actually ended on, not the loader's default model (loadSessionFn passes
// cmd/harness's defModel, which is very often NOT the session's last active
// model — see cmd/harness/main.go's loadSessionFn).
func TestLoadSessionRederivesAfterModelSwitch(t *testing.T) {
	stubContextWindowLookup(t, testContextWindowTable())

	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := Config{
		Providers:  provider.Registry{"test": prov},
		Model:      modelUnknown, // the loader's default — deliberately NOT modelKnownBig
		SessionDir: dir,
	}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	s.SetModel(modelKnownBig)
	if err := s.PersistErr(); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Model(); got != modelKnownBig {
		t.Fatalf("loaded.Model() = %v, want %v", got, modelKnownBig)
	}
	if loaded.cfg.ContextWindowTokens != 500_000 {
		t.Fatalf("loaded cfg.ContextWindowTokens = %d, want 500000 (must derive from the FINAL replayed model, not cfg.Model)", loaded.cfg.ContextWindowTokens)
	}
	if loaded.contextWindowSource != contextWindowSourceModelDerived {
		t.Fatalf("loaded contextWindowSource = %q, want %q", loaded.contextWindowSource, contextWindowSourceModelDerived)
	}
}

// TestLoadSessionExplicitConfigSurvivesModelSwitch: explicit config must
// still win on reload even when the durable log shows a model switch.
func TestLoadSessionExplicitConfigSurvivesModelSwitch(t *testing.T) {
	stubContextWindowLookup(t, testContextWindowTable())

	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := Config{
		Providers:           provider.Registry{"test": prov},
		Model:               modelUnknown,
		ContextWindowTokens: 77_000,
		SessionDir:          dir,
	}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	s.SetModel(modelKnownBig)
	if err := s.PersistErr(); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.cfg.ContextWindowTokens != 77_000 {
		t.Fatalf("loaded cfg.ContextWindowTokens = %d, want 77000 (explicit config must survive reload+switch)", loaded.cfg.ContextWindowTokens)
	}
	if loaded.contextWindowSource != contextWindowSourceConfig {
		t.Fatalf("loaded contextWindowSource = %q, want %q", loaded.contextWindowSource, contextWindowSourceConfig)
	}
}
