package engine

import (
	"context"
	"strings"
	"sync"
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
	if !strings.Contains(last, "engine started "+want) {
		t.Errorf("ambient block = %q, want it to contain %q (UTC)", last, "engine started "+want)
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
	if !strings.Contains(last, "engine started ") {
		t.Errorf("ambient block = %q, want the engine started clause still reported", last)
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
		t.Errorf("ambient block = %q, want no engine started clause when StartedAt is zero", last)
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

// TestIdentityStatusSegmentChildSharesParentEngineStartTime documents (and
// guards) the deliberate, process-wide value this segment reports —
// identityStatusSegment's own doc comment for why. SessionManager.Spawn's
// childCfg is copied wholesale from parent.session.configSnapshot()
// (session_manager.go), so a freshly Spawned child's own "engine started"
// clause reports the EXACT SAME process start time as its parent's, even
// though the child was created much later — never the child's own creation
// time, a genuinely different, not-yet-modeled fact this segment has never
// claimed to report. A live production case (a child Spawned ~14 hours
// after its serving process booted, both the operator and the model
// reading its own ambient context misreading the shared timestamp as "this
// session started") is what surfaced the ambiguity; the fix was the
// clearer "engine started" wording in identityStatusSegment, not changing
// which value is reported — this test is the regression guard for that
// distinction staying correct.
func TestIdentityStatusSegmentChildSharesParentEngineStartTime(t *testing.T) {
	started := time.Date(2026, 8, 24, 5, 12, 26, 0, time.UTC)
	rootProv := &scriptedProvider{name: "root", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "child ok"}),
	}}
	reg := provider.Registry{"root": rootProv, "child": childProv}

	// childDone closes the instant the child's own assistant reply is
	// appended — a real synchronization edge, not a guessed deadline.
	// OnEvent is set on the ROOT's Config here and inherited verbatim by
	// the child's own Config (SessionManager.Spawn's childCfg derivation
	// copies parent.session.configSnapshot() wholesale, OnEvent included —
	// the same inheritance this test is ABOUT), so one callback observes
	// both sessions' EventMessage emits. The root's own Prompt call below
	// runs to completion (and so emits its own, first, assistant
	// EventMessage) synchronously, BEFORE Spawn is ever called — so the
	// second assistant EventMessage this callback ever sees can only be
	// the child's, with no session-id filtering needed and no race window
	// where an event could fire before anything is listening (OnEvent is
	// part of Config from construction, armed before either session's
	// first turn starts).
	var mu sync.Mutex
	assistantMsgs := 0
	childDone := make(chan struct{})
	onEvent := func(ev Event) {
		if ev.Type != EventMessage || ev.Message == nil || ev.Message.Role != message.RoleAssistant {
			return
		}
		mu.Lock()
		assistantMsgs++
		n := assistantMsgs
		mu.Unlock()
		if n == 2 {
			close(childDone)
		}
	}

	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(Config{
		Providers:     reg,
		Model:         modelFor("root"),
		EngineVersion: "1.2.3",
		StartedAt:     started,
		OnEvent:       onEvent,
	})
	if _, err := root.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("root Prompt: %v", err)
	}
	wantClause := "engine started " + started.UTC().Format(time.RFC3339)
	rootLast := lastUserText(t, rootProv.requests[0])
	if !strings.Contains(rootLast, wantClause) {
		t.Fatalf("root ambient block = %q, want %q", rootLast, wantClause)
	}

	if _, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Block directly on childDone — no timeout wrapper. AGENTS.md: block
	// directly on channels for expected events and let the test binary
	// timeout catch a genuine hang, rather than a guessed deadline that
	// can flake under a loaded runner.
	<-childDone

	if len(childProv.requests) == 0 {
		t.Fatal("child never made a request")
	}
	childLast := lastUserText(t, childProv.requests[0])
	if !strings.Contains(childLast, wantClause) {
		t.Errorf("child ambient block = %q, want the SAME %q as its parent (process-wide, by design)", childLast, wantClause)
	}
}
