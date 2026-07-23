package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestOnCreatePhaseStartFiresBeforeCompletion verifies Options.
// OnCreatePhaseStart fires for each of persist/register/emit_created,
// strictly before that phase's matching OnCreatePhase completion — the
// pairing an in-flight watchdog (cmd/harness/main.go) relies on. new_session
// and total are deliberately NOT covered by OnCreatePhaseStart (see its doc
// comment in server.go), so this only asserts pairing for the three phases
// that are.
func TestOnCreatePhaseStartFiresBeforeCompletion(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"}

	type startCall struct{ sessionID, phase string }
	type doneCall struct {
		sessionID, phase string
		elapsed          time.Duration
	}
	var mu sync.Mutex
	var starts []startCall
	var dones []doneCall
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		o.OnCreatePhaseStart = func(sessionID, phase string) {
			mu.Lock()
			defer mu.Unlock()
			starts = append(starts, startCall{sessionID, phase})
		}
		o.OnCreatePhase = func(sessionID, phase string, elapsed time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			dones = append(dones, doneCall{sessionID, phase, elapsed})
		}
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	h := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}

	resp, data := h.do("POST", "/session", map[string]string{"model": "test/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create status %d: %s", resp.StatusCode, data)
	}

	mu.Lock()
	defer mu.Unlock()
	wantPhases := map[string]bool{"persist": false, "register": false, "emit_created": false}
	for _, c := range starts {
		if _, ok := wantPhases[c.phase]; !ok {
			t.Fatalf("unexpected OnCreatePhaseStart phase %q (new_session/total must never fire it)", c.phase)
		}
		if c.sessionID == "" {
			t.Fatalf("OnCreatePhaseStart phase %q carries empty sessionID", c.phase)
		}
		wantPhases[c.phase] = true
	}
	for phase, seen := range wantPhases {
		if !seen {
			t.Errorf("OnCreatePhaseStart phase %q never fired", phase)
		}
	}
	if len(starts) != 3 {
		t.Fatalf("got %d OnCreatePhaseStart calls, want 3: %+v", len(starts), starts)
	}

	// Every start must pair, at the same index, with the matching
	// completion (dones also carries new_session/total, which starts never
	// reports — so compare by matching phase, not raw index equality).
	doneByPhase := make(map[string]doneCall)
	for _, d := range dones {
		doneByPhase[d.phase] = d
	}
	for _, s := range starts {
		d, ok := doneByPhase[s.phase]
		if !ok {
			t.Fatalf("phase %q started but never completed", s.phase)
		}
		if d.sessionID != s.sessionID {
			t.Errorf("phase %q: start sessionID %q != completion sessionID %q", s.phase, s.sessionID, d.sessionID)
		}
	}
}

// TestOnCreatePhaseReportsPersistEndOnFailure is a regression test for the
// bug flagged in PR #89 review: handleCreate used to call
// reportCreatePhaseStart(sess.ID, "persist") and then, on a Persist error,
// return WITHOUT the matching reportCreatePhase — leaving a watchdog's
// in-flight table (see cmd/harness/main.go) with a "persist" entry nothing
// would ever clear, a permanent false "still stuck" warning for a phase
// that had, in fact, already failed and returned. Reuses
// TestOnCreatePhaseReportsTotalOnFailedCreate's arrangement (create_phase_
// test.go): a session whose own log directory is unwritable, so Persist
// fails deterministically while the server's own SessionDir (and so
// server.New's own construction) stays untouched.
func TestOnCreatePhaseReportsPersistEndOnFailure(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"}
	model := message.ModelRef{Provider: prov.Name(), Model: "m1"}
	badLogDir := unwritableDir(t)

	type startCall struct{ sessionID, phase string }
	type doneCall struct{ sessionID, phase string }
	var mu sync.Mutex
	var starts []startCall
	var dones []doneCall
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		o.OnCreatePhaseStart = func(sessionID, phase string) {
			mu.Lock()
			defer mu.Unlock()
			starts = append(starts, startCall{sessionID, phase})
		}
		o.OnCreatePhase = func(sessionID, phase string, elapsed time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			dones = append(dones, doneCall{sessionID, phase})
		}
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
	if len(starts) != 1 || starts[0].phase != "persist" {
		t.Fatalf("OnCreatePhaseStart calls = %+v, want exactly one for \"persist\"", starts)
	}
	// new_session, then persist (this is the bug fix under test: persist
	// must report its END despite Persist() erroring), then total (already
	// unconditional via its own defer, unaffected by this bug, but still
	// expected here since the handler still returns normally).
	if len(dones) != 3 || dones[0].phase != "new_session" || dones[1].phase != "persist" || dones[2].phase != "total" {
		t.Fatalf("OnCreatePhase calls = %+v, want new_session, persist, total", dones)
	}
	if dones[1].sessionID != starts[0].sessionID {
		t.Errorf("persist completion sessionID %q != start sessionID %q", dones[1].sessionID, starts[0].sessionID)
	}
}

// TestDebugGoroutinesRequiresAuth verifies GET /debug/goroutines is behind
// s.auth like every other route (see routes()) — a diagnostic surface that
// dumps the full goroutine stack must not be reachable without the run
// token.
func TestDebugGoroutinesRequiresAuth(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})

	req, err := http.NewRequest("GET", h.ts.URL+"/debug/goroutines", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}
}

// TestDebugGoroutinesReturnsStackDump verifies an authenticated GET
// /debug/goroutines returns 200 with a body that looks like a goroutine
// dump (the same shape SIGQUIT's default handler produces).
func TestDebugGoroutinesReturnsStackDump(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})

	resp, data := h.do("GET", "/debug/goroutines", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	if !strings.Contains(body, "goroutine ") {
		t.Fatalf("body does not look like a goroutine dump: %q", truncate(body, 200))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
