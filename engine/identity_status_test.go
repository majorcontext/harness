package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestAmbientEngineIdentityAbsentWhenUnset covers the overwhelming common
// case among EXISTING sessions/tests that predate this field: neither
// EngineVersion nor StartedAt is set on Config, so the ambient block must
// never appear — the same zero happy-path cost the process/MCP/goal-parked
// segments already commit to, and the reason no unrelated test asserting on
// request/message shape needed to change for this feature.
func TestAmbientEngineIdentityAbsentWhenUnset(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if last := lastUserText(t, prov.requests[0]); strings.Contains(last, "[engine:") {
		t.Fatalf("last user message = %q, want no ambient engine-identity block", last)
	}
}

// TestAmbientEngineIdentityPresent is the headline case: version, mode, and
// start time all configured (mirrors cmd/harness's mkCfg threading) renders
// one block naming all three, on the newest user message only.
func TestAmbientEngineIdentityPresent(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	started := time.Date(2026, 7, 21, 12, 30, 0, 0, time.FixedZone("PDT", -7*3600))
	s := NewSession(Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		EngineVersion: "1.2.3",
		StartedAt:     started,
		SessionSync:   SessionSyncVolume,
	})
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	req := prov.requests[0]
	last := lastUserText(t, req)
	if strings.Count(last, "[engine:") != 1 {
		t.Fatalf("last user message = %q, want exactly one ambient engine-identity block", last)
	}
	if !strings.Contains(last, "harness 1.2.3") {
		t.Errorf("ambient block = %q, want it to report the version", last)
	}
	if !strings.Contains(last, "session_sync=volume") {
		t.Errorf("ambient block = %q, want it to report session_sync=volume", last)
	}
	// Rendered as UTC RFC3339, not the FixedZone offset used above.
	want := started.UTC().Format(time.RFC3339)
	if !strings.Contains(last, "started "+want) {
		t.Errorf("ambient block = %q, want it to contain %q (UTC)", last, "started "+want)
	}
	if strings.Contains(last, "PDT") {
		t.Errorf("ambient block = %q, rendered in a non-UTC zone", last)
	}

	// Only the newest user message carries it — earlier messages must be
	// byte-identical to an uninjected request (mirrors
	// TestAmbientProcessStatusPresentAfterStart).
	for i, m := range req.Messages {
		if m.Role != message.RoleUser {
			continue
		}
		if i != len(req.Messages)-1 && strings.Contains(renderMsgText(m), "[engine:") {
			t.Fatalf("ambient engine-identity block leaked onto a non-newest message: %+v", m)
		}
	}
}

// TestAmbientEngineIdentityDefaultModeRendersFsync covers the self-
// describing-config requirement: the EFFECTIVE session_sync mode is always
// shown, even though Config.SessionSync's own zero value ("") means the
// same thing as an explicit "fsync" — an agent must not have to know the
// default to know its mode.
func TestAmbientEngineIdentityDefaultModeRendersFsync(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		EngineVersion: "1.2.3",
		SessionSync:   "", // zero value
	})
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	last := lastUserText(t, prov.requests[0])
	if !strings.Contains(last, "session_sync=fsync") {
		t.Errorf("ambient block = %q, want the zero-value SessionSync to render as the effective session_sync=fsync", last)
	}
}

// TestAmbientEngineIdentityEmptyVersionOmitsVersionClause covers
// EngineVersion's documented empty behavior: a Config built without a
// version (e.g. an embedder that bypasses cmd/harness, whose own version
// var always defaults to "0.1.0-dev" and so never actually reaches engine
// as "") gets a block missing just the "harness <version>" clause, not the
// whole block — StartedAt and session_sync are still worth reporting on
// their own.
func TestAmbientEngineIdentityEmptyVersionOmitsVersionClause(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		StartedAt: time.Now(),
	})
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	last := lastUserText(t, prov.requests[0])
	if !strings.Contains(last, "[engine:") {
		t.Fatalf("last user message = %q, want an ambient engine-identity block even with EngineVersion unset", last)
	}
	if strings.Contains(last, "harness ") {
		t.Errorf("ambient block = %q, want no \"harness \" version clause when EngineVersion is empty", last)
	}
	if !strings.Contains(last, "session_sync=fsync") {
		t.Errorf("ambient block = %q, want session_sync still reported", last)
	}
	if !strings.Contains(last, "started ") {
		t.Errorf("ambient block = %q, want the started clause still reported", last)
	}
}

// TestAmbientEngineIdentityZeroStartedAtOmitsStartedClause is the mirror
// case: StartedAt unset (Config built with only EngineVersion, e.g. a
// caller that doesn't track process start time) omits just the "started
// ..." clause.
func TestAmbientEngineIdentityZeroStartedAtOmitsStartedClause(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		EngineVersion: "1.2.3",
	})
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	last := lastUserText(t, prov.requests[0])
	if !strings.Contains(last, "harness 1.2.3") {
		t.Errorf("ambient block = %q, want the version clause still reported", last)
	}
	if strings.Contains(last, "started ") {
		t.Errorf("ambient block = %q, want no started clause when StartedAt is zero", last)
	}
}

// TestAmbientEngineIdentityNeverPersisted mirrors
// TestAmbientProcessStatusNeverPersisted/TestAmbientGoalParkedStatusNeverPersisted:
// the block must never leak into s.History() or a reloaded session's log.
func TestAmbientEngineIdentityNeverPersisted(t *testing.T) {
	sesDir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	cfg := Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir:    sesDir,
		EngineVersion: "1.2.3",
		StartedAt:     time.Now(),
		Instructions:  &InstructionsConfig{Disabled: true},
		SkillsDirs:    []string{},
	}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}

	// Sanity: the block really was present on the request.
	last := lastUserText(t, prov.requests[0])
	if !strings.Contains(last, "[engine:") {
		t.Fatalf("last user message = %q, want an ambient engine-identity block present before checking persistence", last)
	}

	for _, m := range s.History() {
		if strings.Contains(renderMsgText(m), "[engine:") {
			t.Fatalf("ambient engine-identity block leaked into in-memory history: %+v", m)
		}
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	for _, m := range loaded.History() {
		if strings.Contains(renderMsgText(m), "[engine:") {
			t.Fatalf("ambient engine-identity block leaked into persisted history: %+v", m)
		}
	}
}

// TestIdentityStatusSegmentAbsentWhenBothUnset is a direct unit test of the
// pure function backing the ambient block, covering the exact boundary
// condition: neither input set at all yields "".
func TestIdentityStatusSegmentAbsentWhenBothUnset(t *testing.T) {
	if got := identityStatusSegment("", time.Time{}, ""); got != "" {
		t.Fatalf("identityStatusSegment(\"\", zero, \"\") = %q, want \"\"", got)
	}
}
