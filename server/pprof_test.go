package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPProf_OffByDefault proves the profiling routes do not exist unless a
// caller asks for them. They are a diagnostic surface, so an operator opts
// in per process.
func TestPProf_OffByDefault(t *testing.T) {
	srv := newServer(t, t.TempDir(), &scriptedProvider{name: "test"}, 0)

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/cmdline"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret-run-token")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s answered %d with profiling off, want 404", path, w.Code)
		}
	}
}

// TestPProf_EnabledRequiresAuth proves an enabled profiling route is behind
// the same bearer check as every other route. A profile carries function
// names and inlined data from the running process.
func TestPProf_EnabledRequiresAuth(t *testing.T) {
	srv := newServer(t, t.TempDir(), &scriptedProvider{name: "test"}, 0, func(o *Options) {
		o.PProf = true
	})

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated profile read answered %d, want 401", w.Code)
	}
}

// TestPProf_EnabledServesProfiles proves an authorized read returns a real
// profile, and that the index lists them.
func TestPProf_EnabledServesProfiles(t *testing.T) {
	srv := newServer(t, t.TempDir(), &scriptedProvider{name: "test"}, 0, func(o *Options) {
		o.PProf = true
	})

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=1", nil)
	req.Header.Set("Authorization", "Bearer secret-run-token")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("profile read answered %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "goroutine profile") {
		t.Errorf("body is not a goroutine profile: %q", w.Body.String()[:min(200, w.Body.Len())])
	}

	req = httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	req.Header.Set("Authorization", "Bearer secret-run-token")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("profile index answered %d, want 200", w.Code)
	}
}

// TestPProf_GoroutineDumpNeedsNoFlag proves the pre-existing all-goroutine
// dump still works with profiling off. It is the zero-configuration first
// step for a wedged process.
func TestPProf_GoroutineDumpNeedsNoFlag(t *testing.T) {
	srv := newServer(t, t.TempDir(), &scriptedProvider{name: "test"}, 0)

	req := httptest.NewRequest(http.MethodGet, "/debug/goroutines", nil)
	req.Header.Set("Authorization", "Bearer secret-run-token")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("goroutine dump answered %d, want 200", w.Code)
	}
}
