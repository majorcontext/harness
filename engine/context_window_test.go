package engine

import (
	"context"
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
