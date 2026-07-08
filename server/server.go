// Package server is the HTTP+SSE surface that `harness serve` exposes inside a
// sandbox. It is a protocol, not a product: a single external orchestrator
// drives many harness instances through it (see server/openapi.yaml).
//
// The server owns an orchestrator-facing event journal (<SessionDir>/events.jsonl):
// an append-only log of durable records — session lifecycle, canonical
// messages, model swaps, status changes — each carrying a global monotonic
// sequence number. Live deltas (text, reasoning, tool progress) stream between
// durable records over SSE but are never journaled. A client that reconnects
// with from=<last seq> replays exactly what it missed.
//
// The engine is injected (Options.NewSession / Options.LoadSession) so the
// server has no opinion on provider wiring; tests use scripted providers.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

// Options configures a Server. RunToken, NewSession, and LoadSession are
// required.
type Options struct {
	// SessionDir is the session log directory. events.jsonl (the durable
	// journal) lives here alongside the per-session <id>.jsonl logs. Empty
	// keeps the journal in memory only (no durability).
	SessionDir string
	// RunToken authenticates every request except /health, compared in
	// constant time.
	RunToken string
	// Version is reported by /health.
	Version string
	// NewSession creates a fresh engine session for the given model (zero
	// model = caller's default). The wrapper is expected to wire
	// engine.Config.OnEvent to Server.Publish.
	NewSession func(model message.ModelRef) (*engine.Session, error)
	// LoadSession resumes an on-disk session by ID, wiring OnEvent the same
	// way. It returns an error when no log with that ID exists.
	LoadSession func(id string) (*engine.Session, error)
	// HeartbeatInterval is the SSE keep-alive comment period; 0 defaults to
	// 30s.
	HeartbeatInterval time.Duration
	// MaxResident caps the number of in-memory (resident) sessions. After a
	// prompt completes, the longest-idle non-busy sessions beyond this cap are
	// unloaded from memory; they remain listable and promptable via a
	// transparent reload from disk. 0 defaults to 32.
	MaxResident int
}

// Server implements http.Handler for the harness serve API.
type Server struct {
	opts Options
	mux  *http.ServeMux

	// wg tracks in-flight runPrompt goroutines. They are decoupled from their
	// HTTP handlers (the 202 returns immediately), so http.Server.Shutdown does
	// not wait for them; Drain does, via this group.
	wg sync.WaitGroup

	mu      sync.Mutex
	seq     int64                    // global monotonic durable sequence
	journal []Event                  // in-memory durable records, for replay
	jf      *os.File                 // events.jsonl handle (nil when disabled)
	lastErr error                    // most recent journal write failure
	subs    map[*subscriber]struct{} // connected SSE clients
	// seen maps session ID -> journaled message IDs; it is authoritative for
	// journal idempotency (syncMessages skips already-journaled IDs), so it is
	// never evicted when resident sessions are unloaded for MaxResident. It is
	// bounded by the number of message IDs, which are small, so retaining it for
	// unloaded sessions is cheap and keeps replay/reconcile correct.
	seen     map[string]map[string]bool
	sessions map[string]*sessionState // in-memory (resident) sessions
}

// sessionState tracks an in-memory session and any in-flight prompt. lastUsed
// is the time the session was created, loaded, or last finished a prompt; it
// orders MaxResident eviction (longest-idle first).
type sessionState struct {
	sess     *engine.Session
	running  bool
	cancel   context.CancelFunc
	lastUsed time.Time
}

// New builds a Server and reconciles its journal against the on-disk session
// logs (appending any message records lost to a crash between the session-log
// and journal appends). Reconciliation reads the session directory only — it
// touches no network and spawns nothing, so it is safe on the startup path.
func New(opts Options) (*Server, error) {
	if opts.RunToken == "" {
		return nil, errors.New("server: RunToken is required")
	}
	if opts.NewSession == nil || opts.LoadSession == nil {
		return nil, errors.New("server: NewSession and LoadSession are required")
	}
	if opts.Version == "" {
		opts.Version = "0.1.0"
	}
	if opts.MaxResident <= 0 {
		opts.MaxResident = 32
	}
	s := &Server{
		opts:     opts,
		subs:     make(map[*subscriber]struct{}),
		seen:     make(map[string]map[string]bool),
		sessions: make(map[string]*sessionState),
	}
	if err := s.reconcile(); err != nil {
		return nil, err
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /session", s.auth(s.handleList))
	mux.HandleFunc("POST /session", s.auth(s.handleCreate))
	// A precise pattern outranks the {id} wildcard, so /session/status is not
	// mistaken for a session named "status".
	mux.HandleFunc("GET /session/status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /session/{id}", s.auth(s.handleGet))
	mux.HandleFunc("GET /session/{id}/message", s.auth(s.handleMessages))
	mux.HandleFunc("POST /session/{id}/prompt_async", s.auth(s.handlePrompt))
	mux.HandleFunc("POST /session/{id}/abort", s.auth(s.handleAbort))
	mux.HandleFunc("GET /event", s.auth(s.handleEvent))
	s.mux = mux
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Drain waits for in-flight prompts to finish, then returns. It first waits up
// to ctx's deadline for prompts to complete on their own; if ctx expires while
// prompts are still running, it cancels their contexts (which journals a
// durable session.aborted for each) and waits for them to unwind. It must be
// called before Close so the journal file stays open while the trailing
// records — the final assistant message and the session.aborted/idle
// transitions — are written; otherwise those records are lost on shutdown.
func (s *Server) Drain(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	// Grace period expired with prompts still in flight: cancel them so their
	// runPrompt goroutines observe context.Canceled, journal session.aborted,
	// and exit. Wait for that to finish so the records land before Close.
	s.mu.Lock()
	for _, st := range s.sessions {
		if st.cancel != nil {
			st.cancel()
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Close releases the journal file, if any.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jf != nil {
		return s.jf.Close()
	}
	return nil
}

// auth wraps a handler with constant-time bearer-token authentication.
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	tok := h[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.opts.RunToken)) == 1
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
