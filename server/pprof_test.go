package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

// TestPProf_NotRegisteredOnDefaultServeMux is the load-bearing assertion of
// the profiling opt-in. Importing net/http/pprof registers /debug/pprof/*
// on http.DefaultServeMux from that package's own init, so a program that
// merely LINKS this one and serves the default mux —
// http.ListenAndServe(addr, nil), an ordinary shape — would expose
// profiling with no opt-in at all, and Options.PProf could not prevent it.
// A library must not register global handlers as an import side effect.
//
// This asserts the whole linked test binary registers nothing there, which
// is only true if no package on this one's import graph pulls in
// net/http/pprof.
func TestPProf_NotRegisteredOnDefaultServeMux(t *testing.T) {
	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/heap",
		"/debug/pprof/profile",
		"/debug/pprof/cmdline",
		"/debug/pprof/trace",
		"/debug/pprof/symbol",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if _, pattern := http.DefaultServeMux.Handler(req); pattern != "" {
			t.Errorf("%s is registered on http.DefaultServeMux as %q; profiling must exist only on this server's own mux, behind Options.PProf", path, pattern)
		}
	}
}

// TestPProf_OffByDefault proves the routes do not exist on this server
// unless a caller asks for them.
func TestPProf_OffByDefault(t *testing.T) {
	srv := newServer(t, t.TempDir(), &scriptedProvider{name: "test"}, 0)

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/cmdline", "/debug/pprof/profile"} {
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
// names and allocation sites from the running process.
func TestPProf_EnabledRequiresAuth(t *testing.T) {
	srv := pprofServer(t)

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/cmdline", "/debug/pprof/profile", "/debug/pprof/trace"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s answered %d, want 401", path, w.Code)
		}
	}
}

// TestPProf_EnabledServesProfiles proves an authorized read returns real
// profile data, in both the text and the binary form.
func TestPProf_EnabledServesProfiles(t *testing.T) {
	srv := pprofServer(t)

	t.Run("text goroutine profile", func(t *testing.T) {
		w := pprofGet(t, srv, "/debug/pprof/goroutine?debug=1")
		if w.Code != http.StatusOK {
			t.Fatalf("answered %d, want 200", w.Code)
		}
		if !strings.Contains(w.Body.String(), "goroutine profile") {
			t.Errorf("body is not a goroutine profile: %q", firstLine(w.Body.String()))
		}
	})

	t.Run("binary heap profile", func(t *testing.T) {
		w := pprofGet(t, srv, "/debug/pprof/heap")
		if w.Code != http.StatusOK {
			t.Fatalf("answered %d, want 200", w.Code)
		}
		// A binary pprof profile is gzip-framed: it starts with the gzip
		// magic bytes, which is what `go tool pprof` expects to read.
		if b := w.Body.Bytes(); len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
			t.Errorf("body is not a gzip-framed pprof profile: % x", b[:min(4, len(b))])
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("Content-Type = %q, want application/octet-stream", ct)
		}
	})

	t.Run("index lists the profiles", func(t *testing.T) {
		w := pprofGet(t, srv, "/debug/pprof/")
		if w.Code != http.StatusOK {
			t.Fatalf("answered %d, want 200", w.Code)
		}
		for _, want := range []string{"goroutine", "heap", "profile", "trace"} {
			if !strings.Contains(w.Body.String(), want) {
				t.Errorf("index does not mention %q: %s", want, w.Body.String())
			}
		}
	})

	t.Run("cmdline", func(t *testing.T) {
		if w := pprofGet(t, srv, "/debug/pprof/cmdline"); w.Code != http.StatusOK {
			t.Fatalf("answered %d, want 200", w.Code)
		}
	})

	t.Run("unknown profile is a 404", func(t *testing.T) {
		if w := pprofGet(t, srv, "/debug/pprof/no-such-profile"); w.Code != http.StatusNotFound {
			t.Errorf("answered %d, want 404", w.Code)
		}
	})
}

// TestPProf_CPUProfileRunsForTheRequestedTime proves the CPU profile path
// works end to end at its shortest allowed duration.
func TestPProf_CPUProfileRunsForTheRequestedTime(t *testing.T) {
	srv := pprofServer(t)

	w := pprofGet(t, srv, "/debug/pprof/profile?seconds=1")
	if w.Code != http.StatusOK {
		t.Fatalf("answered %d, want 200: %s", w.Code, w.Body.String())
	}
	if b := w.Body.Bytes(); len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		t.Errorf("body is not a gzip-framed CPU profile: % x", b[:min(4, len(b))])
	}
}

// TestPProf_CPUProfileConflictIsA409 proves a second concurrent CPU profile
// is refused with a real status instead of a 500 or a hang. Only one CPU
// profile can run in a process at a time.
func TestPProf_CPUProfileConflictIsA409(t *testing.T) {
	srv := pprofServer(t)

	if err := pprof.StartCPUProfile(io.Discard); err != nil {
		t.Fatalf("starting the conflicting profile: %v", err)
	}
	t.Cleanup(pprof.StopCPUProfile)

	if w := pprofGet(t, srv, "/debug/pprof/profile?seconds=1"); w.Code != http.StatusConflict {
		t.Errorf("answered %d, want 409", w.Code)
	}
}

// TestProfileSeconds pins the duration parsing: no value a caller can send
// turns into an unbounded profile, and a malformed one is a 400 like every
// other integer parameter in this API rather than a silent default.
func TestProfileSeconds(t *testing.T) {
	t.Run("clamped and defaulted", func(t *testing.T) {
		cases := map[string]time.Duration{
			"?":             defaultProfileSeconds, // absent
			"?seconds=5":    5 * time.Second,
			"?seconds=0":    minProfileSeconds,
			"?seconds=9999": maxProfileSeconds,
			"?other=1":      defaultProfileSeconds,
		}
		for query, want := range cases {
			req := httptest.NewRequest(http.MethodGet, "/debug/pprof/profile"+query, nil)
			got, ok := profileSeconds(httptest.NewRecorder(), req)
			if !ok {
				t.Errorf("profileSeconds(%q) rejected a valid request", query)
				continue
			}
			if got != want {
				t.Errorf("profileSeconds(%q) = %v, want %v", query, got, want)
			}
		}
	})

	t.Run("malformed is a 400", func(t *testing.T) {
		for _, query := range []string{"?seconds=not-a-number", "?seconds=", "?seconds=-3", "?seconds=1&seconds=2"} {
			req := httptest.NewRequest(http.MethodGet, "/debug/pprof/profile"+query, nil)
			w := httptest.NewRecorder()
			if _, ok := profileSeconds(w, req); ok {
				t.Errorf("profileSeconds(%q) accepted a malformed value", query)
				continue
			}
			if w.Code != http.StatusBadRequest {
				t.Errorf("profileSeconds(%q) answered %d, want 400", query, w.Code)
			}
		}
	})
}

func pprofServer(t *testing.T) *Server {
	t.Helper()
	return newServer(t, t.TempDir(), &scriptedProvider{name: "test"}, 0, func(o *Options) {
		o.PProf = true
	})
}

func pprofGet(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer secret-run-token")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
