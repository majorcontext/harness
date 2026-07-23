package server

import (
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
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
		if c.elapsed < 0 {
			t.Fatalf("phase %q elapsed = %v, want >= 0", c.phase, c.elapsed)
		}
	}
	for phase, seen := range wantPhases {
		if !seen {
			t.Errorf("phase %q not reported", phase)
		}
	}
}
