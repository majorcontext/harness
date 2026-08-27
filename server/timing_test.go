package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stepClock is a manual clock a test advances explicitly, so a measured
// duration is an exact number rather than a race against real time.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
	d  time.Duration
}

// now returns the current time and advances the clock by d, so each
// timestamp a handler path reads differs from the last by exactly d.
func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.t
	c.t = c.t.Add(c.d)
	return now
}

// newSlowServer builds a server whose clock makes every request appear to
// take d, with logger as the log sink.
func newSlowServer(t *testing.T, logger *slog.Logger, d time.Duration) *Server {
	t.Helper()
	srv := newServer(t, t.TempDir(), &scriptedProvider{name: "test"}, 0, func(o *Options) {
		o.Logger = logger
	})
	srv.now = (&stepClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), d: d}).now
	return srv
}

// TestSlowRequest_WarnsWithRouteAndDuration proves a request the harness
// itself was slow to handle logs one WARN naming the route, the status,
// and the duration. This is what separates "the harness handler was slow"
// from "the network in front of it was slow".
func TestSlowRequest_WarnsWithRouteAndDuration(t *testing.T) {
	var logBuf bytes.Buffer
	srv := newSlowServer(t, slog.New(slog.NewTextHandler(&logBuf, nil)), 900*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/session", nil)
	req.Header.Set("Authorization", "Bearer secret-run-token")
	srv.ServeHTTP(httptest.NewRecorder(), req)

	line := findLogLine(t, logBuf.String(), slowRequestMsg)
	for _, want := range []string{"level=WARN", "method=GET", "status=200", "duration_ms=900"} {
		if !hasLogField(line, want) {
			t.Errorf("slow request line is missing %q: %s", want, line)
		}
	}
	// The route carries a space, so slog quotes it as one field.
	if !strings.Contains(line, `route="GET /session"`) {
		t.Errorf("slow request line is missing the route: %s", line)
	}
}

// TestSlowRequest_CarriesRequestID proves the caller's X-Request-Id rides
// the line, so one harness-side line joins the calling service's own log
// for the same request.
func TestSlowRequest_CarriesRequestID(t *testing.T) {
	var logBuf bytes.Buffer
	srv := newSlowServer(t, slog.New(slog.NewTextHandler(&logBuf, nil)), 900*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/session", nil)
	req.Header.Set("Authorization", "Bearer secret-run-token")
	req.Header.Set("X-Request-Id", "req_01abc")
	srv.ServeHTTP(httptest.NewRecorder(), req)

	if line := findLogLine(t, logBuf.String(), slowRequestMsg); !hasLogField(line, "request_id=req_01abc") {
		t.Errorf("slow request line dropped the caller's request id: %s", line)
	}
}

// TestSlowRequest_RejectsUnusableRequestID proves a caller-supplied id that
// is too long, or carries anything outside a printable single token, never
// reaches a log line. The id is untrusted input.
func TestSlowRequest_RejectsUnusableRequestID(t *testing.T) {
	for name, id := range map[string]string{
		"too long":  strings.Repeat("x", maxRequestIDLen+1),
		"space":     "req 01abc",
		"newline":   "req_01abc\nlevel=ERROR msg=forged",
		"non ascii": "req_\x00abc",
	} {
		t.Run(name, func(t *testing.T) {
			var logBuf bytes.Buffer
			srv := newSlowServer(t, slog.New(slog.NewTextHandler(&logBuf, nil)), 900*time.Millisecond)

			req := httptest.NewRequest(http.MethodGet, "/session", nil)
			req.Header.Set("Authorization", "Bearer secret-run-token")
			req.Header.Set("X-Request-Id", id)
			srv.ServeHTTP(httptest.NewRecorder(), req)

			if line := findLogLine(t, logBuf.String(), slowRequestMsg); strings.Contains(line, "request_id=") {
				t.Errorf("an unusable request id reached the log line: %s", line)
			}
		})
	}
}

// TestSlowRequest_QuietUnderThreshold proves a fast request logs nothing.
// The warn only means something if it stays rare.
func TestSlowRequest_QuietUnderThreshold(t *testing.T) {
	var logBuf bytes.Buffer
	srv := newSlowServer(t, slog.New(slog.NewTextHandler(&logBuf, nil)), slowRequestThreshold)

	req := httptest.NewRequest(http.MethodGet, "/session", nil)
	req.Header.Set("Authorization", "Bearer secret-run-token")
	srv.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(logBuf.String(), slowRequestMsg) {
		t.Errorf("a request AT the threshold warned; only one PAST it may: %s", logBuf.String())
	}
}

// TestSlowRequest_SkipsLongLivedRoutes proves the streaming and blocking
// routes never warn. Both run for as long as their caller wants, so timing
// them would log a warn for every healthy client.
func TestSlowRequest_SkipsLongLivedRoutes(t *testing.T) {
	for _, route := range []string{"GET /event", "GET /session/{id}/wait"} {
		if !longLivedRoutes[route] {
			t.Errorf("route %q must be exempt from slow-request logging", route)
		}
	}

	var logBuf bytes.Buffer
	srv := newSlowServer(t, slog.New(slog.NewTextHandler(&logBuf, nil)), 30*time.Second)

	req := httptest.NewRequest(http.MethodGet, "/session/ses_missing/wait?until=idle", nil)
	req.Header.Set("Authorization", "Bearer secret-run-token")
	srv.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(logBuf.String(), slowRequestMsg) {
		t.Errorf("a long-lived route warned: %s", logBuf.String())
	}
}

// TestSlowRequest_UnmatchedPathLogsFixedRoute proves an unrouted path logs
// a fixed label, never the caller's own path. A caller controls the path,
// so logging it verbatim would let a caller choose what a log line says.
func TestSlowRequest_UnmatchedPathLogsFixedRoute(t *testing.T) {
	var logBuf bytes.Buffer
	srv := newSlowServer(t, slog.New(slog.NewTextHandler(&logBuf, nil)), 900*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/no/such/route", nil)
	req.Header.Set("Authorization", "Bearer secret-run-token")
	srv.ServeHTTP(httptest.NewRecorder(), req)

	line := findLogLine(t, logBuf.String(), slowRequestMsg)
	if !hasLogField(line, "route="+unmatchedRoute) {
		t.Errorf("unmatched path did not log the fixed route label: %s", line)
	}
	if strings.Contains(line, "/no/such/route") {
		t.Errorf("unmatched path leaked into the log line: %s", line)
	}
}

// TestSlowRequest_NilLoggerStaysSilent proves a server with no Logger
// keeps its current behavior: no output, no panic.
func TestSlowRequest_NilLoggerStaysSilent(t *testing.T) {
	srv := newSlowServer(t, nil, 30*time.Second)
	srv.opts.Logger = nil

	req := httptest.NewRequest(http.MethodGet, "/session", nil)
	req.Header.Set("Authorization", "Bearer secret-run-token")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestSlowRequest_PreservesFlusher proves the timing wrapper keeps the
// response writer's Flusher, which the event stream requires. A wrapper
// that hides it turns every stream into a 500.
func TestSlowRequest_PreservesFlusher(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := &timedWriter{ResponseWriter: rec, status: http.StatusOK}
	if _, ok := any(tw).(http.Flusher); !ok {
		t.Fatal("timedWriter does not implement http.Flusher")
	}
	tw.WriteHeader(http.StatusAccepted)
	if tw.status != http.StatusAccepted {
		t.Errorf("timedWriter recorded status %d, want 202", tw.status)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if rec.Code != http.StatusAccepted || rec.Body.String() != "x" {
		t.Errorf("timedWriter did not pass the response through: %d %q", rec.Code, rec.Body.String())
	}
}

// findLogLine returns the one log line carrying msg.
func findLogLine(t *testing.T, logged, msg string) string {
	t.Helper()
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "msg="+msg+" ") || strings.Contains(line, `msg="`+msg+`"`) {
			return line
		}
	}
	t.Fatalf("no log line with msg %q in: %s", msg, logged)
	return ""
}

// hasLogField reports whether line carries pair as a whole space-delimited
// field, so "duration_ms=900" cannot satisfy an assertion on "ms=900".
func hasLogField(line, pair string) bool {
	for _, field := range strings.Fields(line) {
		if field == pair {
			return true
		}
	}
	return false
}
