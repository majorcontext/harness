package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestOnCreatePhaseReportsPhases verifies Options.OnCreatePhase fires for
// every phase of a successful POST /session — new_session, persist,
// register, emit_created, and finally total — all carrying the created
// session's ID, matching cmd/harness/main.go's createPhaseLogger
// accumulator shape (keyed by session ID, since concurrent creates are
// possible even though this test only drives one).
func TestOnCreatePhaseReportsPhases(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"}

	type call struct {
		sessionID, phase string
		elapsed          time.Duration
	}
	var mu sync.Mutex
	var calls []call
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		o.OnCreatePhase = func(sessionID, phase string, elapsed time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, call{sessionID, phase, elapsed})
		}
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	h := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}

	resp, data := h.do("POST", "/session", map[string]string{"model": "test/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create status %d: %s", resp.StatusCode, data)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("created session has empty ID")
	}

	mu.Lock()
	defer mu.Unlock()
	wantPhases := map[string]bool{"new_session": false, "persist": false, "register": false, "emit_created": false, "total": false}
	for _, c := range calls {
		if c.sessionID != created.ID {
			t.Fatalf("call %q sessionID = %q, want %q", c.phase, c.sessionID, created.ID)
		}
		if _, ok := wantPhases[c.phase]; !ok {
			t.Fatalf("unexpected phase %q", c.phase)
		}
		wantPhases[c.phase] = true
	}
	for phase, seen := range wantPhases {
		if !seen {
			t.Errorf("phase %q not reported", phase)
		}
	}
}

// unwritableDir returns a path guaranteed to make os.OpenFile(..., O_CREATE,
// ...) fail inside it deterministically, regardless of which user runs the
// test — the same technique as engine/store_failure_test.go's
// unwritableSessionDir, replicated locally since that helper is unexported
// to the engine package. Used below to make ONLY a session's own log
// directory unwritable, distinct from the server harness's own SessionDir
// (which must stay writable — server.New's reconcile eagerly opens
// events.jsonl there, and a shared unwritable dir would fail server
// construction itself, not just the one Persist call this test targets).
func unwritableDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	if os.Geteuid() == 0 {
		// Root bypasses DAC permission bits, so chmod alone wouldn't be
		// deterministic under a root test runner. Blocking a path component
		// with a plain file is: no privilege level can mkdir/create through
		// a path segment that already exists as a non-directory.
		blocked := filepath.Join(base, "blocked")
		if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("seed blocking file: %v", err)
		}
		return filepath.Join(blocked, "sessions")
	}
	dir := filepath.Join(base, "sessions")
	if err := os.MkdirAll(dir, 0o555); err != nil {
		t.Fatalf("seed unwritable dir: %v", err)
	}
	return dir
}

// TestOnCreatePhaseReportsTotalOnFailedCreate is a regression test for the
// phase-accumulator leak found in PR #87 review: handleCreate originally
// reported "total" only on its success tail, so a failure after
// "new_session" (e.g. Persist erroring on a saturated storage volume) never
// reported "total" at all, permanently orphaning
// that session ID's entry in the cmd-layer accumulator (see
// cmd/harness/main.go's createPhaseLogger, keyed by session ID). handleCreate
// now reports "total" via a defer installed right after "new_session"
// succeeds, so it fires on every return path — this pins that: Persist is
// made to fail deterministically (the session's own log directory is
// unwritable, independent of the server's own SessionDir), and the create
// must still report "new_session" and "total". It must ALSO report "persist"
// itself (a PR #89 review fix: timedCreatePhase, see handlers.go, guarantees
// a started phase's own OnCreatePhase fires on error too, not just success —
// see TestOnCreatePhaseReportsPersistEndOnFailure in create_phase_start_
// test.go for the dedicated regression test of that fix). "register" and
// "emit_created" still never fire, since the handler returns before ever
// reaching them.
func TestOnCreatePhaseReportsTotalOnFailedCreate(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"}
	model := message.ModelRef{Provider: prov.Name(), Model: "m1"}
	badLogDir := unwritableDir(t)

	type call struct {
		sessionID, phase string
	}
	var mu sync.Mutex
	var calls []call
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		o.OnCreatePhase = func(sessionID, phase string, elapsed time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, call{sessionID, phase})
		}
		// Sessions built by this override log into badLogDir, not the
		// server's own (writable) SessionDir — so NewSession itself (lazy,
		// no disk touch) still succeeds, and only the later Persist call
		// fails.
		o.NewSession = func(m message.ModelRef, workDir, parentSession string) (*engine.Session, error) {
			if m.IsZero() {
				m = model
			}
			return engine.NewSession(engine.Config{
				Providers:     provider.Registry{prov.Name(): prov},
				Model:         m,
				SessionDir:    badLogDir,
				WorkDir:       workDir,
				ParentSession: parentSession,
			}), nil
		}
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	h := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}

	resp, data := h.do("POST", "/session", map[string]string{"model": "test/m1"})
	if resp.StatusCode != 500 {
		t.Fatalf("create status %d, want 500 (Persist should fail): %s", resp.StatusCode, data)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("got %d OnCreatePhase calls, want 3 (new_session, persist, total): %+v", len(calls), calls)
	}
	if calls[0].phase != "new_session" || calls[1].phase != "persist" || calls[2].phase != "total" {
		t.Fatalf("phases = %q, %q, %q; want new_session, persist, total", calls[0].phase, calls[1].phase, calls[2].phase)
	}
	if calls[0].sessionID == "" {
		t.Fatal("new_session call carries empty sessionID")
	}
	for _, c := range calls[1:] {
		if c.sessionID != calls[0].sessionID {
			t.Fatalf("%s sessionID = %q, want %q (same as new_session)", c.phase, c.sessionID, calls[0].sessionID)
		}
	}
}
