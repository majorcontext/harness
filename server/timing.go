package server

import (
	"net/http"
	"time"
)

// Per-request timing for the serve API. A caller that waits seconds for a
// reply cannot tell, from the outside, whether this process was slow or
// the network in front of it was. A line here says this process was.

// slowRequestThreshold is the cutoff for the warn below. Every timed route
// is local work — a session read, an enqueue, a goal write — so half a
// second is already far outside normal and keeps the line rare. A var, not
// a const, so a test can drive the quiet path; production never
// reassigns it.
var slowRequestThreshold = 500 * time.Millisecond

// slowRequestMsg is the warn line's message. One fixed string, so a log
// search finds every slow request.
const slowRequestMsg = "slow request"

// unmatchedRoute labels a request that matched no route. The path is
// caller-controlled, so it never reaches a log line: a fixed label bounds
// both what a caller can write into the log and how many distinct route
// values exist.
const unmatchedRoute = "unmatched"

// longLivedRoutes are the routes that run for as long as their caller
// wants: the event stream and the wait long-poll. A duration means nothing
// for them, so they are never timed — otherwise every healthy client would
// produce a warn.
var longLivedRoutes = map[string]bool{
	"GET /event":             true,
	"GET /session/{id}/wait": true,
}

// maxRequestIDLen bounds the caller-supplied X-Request-Id a log line
// echoes.
const maxRequestIDLen = 64

// serveTimed dispatches to the mux and warns when this process took longer
// than slowRequestThreshold to answer.
func (s *Server) serveTimed(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	tw := &timedWriter{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(tw, r)
	elapsed := s.now().Sub(start)
	if elapsed <= slowRequestThreshold {
		return
	}
	// r.Pattern is set by the mux during the dispatch above, so it is read
	// here rather than before it.
	route := r.Pattern
	if route == "" {
		route = unmatchedRoute
	}
	if longLivedRoutes[route] {
		return
	}
	attrs := []any{
		"method", r.Method,
		"route", route,
		"status", tw.status,
		"duration_ms", elapsed.Milliseconds(),
	}
	if id := requestID(r); id != "" {
		attrs = append(attrs, "request_id", id)
	}
	s.logWarn(slowRequestMsg, attrs...)
}

// requestID returns the caller's X-Request-Id when it is a single
// printable ASCII token within maxRequestIDLen bytes, else "". The header
// is untrusted input that lands in a log line, so anything that could
// forge a field or a line break is dropped whole.
func requestID(r *http.Request) string {
	id := r.Header.Get("X-Request-Id")
	if id == "" || len(id) > maxRequestIDLen {
		return ""
	}
	for i := 0; i < len(id); i++ {
		if id[i] <= ' ' || id[i] > '~' {
			return ""
		}
	}
	return id
}

// timedWriter records the status code for the line above. It forwards
// Flush, which the event stream requires, and Unwrap, so an
// http.ResponseController still reaches the underlying writer.
type timedWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *timedWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.status = status
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *timedWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

func (w *timedWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *timedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
