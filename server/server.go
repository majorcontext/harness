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
	// CORSOrigin, when non-empty, enables browser CORS support. Its literal
	// value is echoed in the Access-Control-Allow-Origin header on every
	// response (including 401s, so a browser can read the error), and "*" is
	// honored as-is. OPTIONS preflight requests to any route are answered 204
	// without authentication (preflights carry no credentials by spec). Empty
	// (the default) disables CORS entirely — no CORS headers are emitted.
	CORSOrigin string
}

// Server implements http.Handler for the harness serve API.
type Server struct {
	opts Options
	mux  *http.ServeMux

	// wg tracks in-flight runPrompt goroutines. They are decoupled from their
	// HTTP handlers (the 202 returns immediately), so http.Server.Shutdown does
	// not wait for them; Drain does, via this group.
	wg sync.WaitGroup

	// closing is closed exactly once, at the top of Drain, to signal that
	// shutdown has begun. Connected SSE streams select on it and return
	// promptly so http.Server.Shutdown sees idle connections; disconnected
	// orchestrators recover the records they miss via replay-from-seq.
	closing   chan struct{}
	closeOnce sync.Once

	mu       sync.Mutex
	draining bool                     // set once by Drain; gates prompt admission
	seq      int64                    // global monotonic durable sequence
	journal  []Event                  // in-memory durable records, for replay
	jf       *os.File                 // events.jsonl handle (nil when disabled)
	lastErr  error                    // most recent journal write failure
	subs     map[*subscriber]struct{} // connected SSE clients
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
		closing:  make(chan struct{}),
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

// ServeHTTP implements http.Handler. When CORS is enabled it layers the CORS
// contract over the mux: the allow-origin/Vary headers on every response and a
// short-circuited 204 for OPTIONS preflights (which carry no credentials, so
// they bypass auth entirely).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.opts.CORSOrigin != "" {
		h := w.Header()
		// Echo the configured origin (literal, including "*"). Setting it here,
		// before the mux runs, means it rides along on every downstream response
		// — success, 401, 404, or SSE — so a browser can always read the result.
		h.Set("Access-Control-Allow-Origin", s.opts.CORSOrigin)
		h.Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// Drain waits for in-flight prompts to finish, then returns. Under s.mu, and
// before it starts waiting, it sets the draining flag and closes the closing
// signal (once). Setting draining before wg.Wait is what makes the prompt
// admission gate correct: a new prompt's wg.Add(1) happens in the same s.mu
// critical section that checks draining (see claimForPrompt), so by mutex
// ordering every Add that ever runs either preceded draining=true — and is
// therefore counted by the Wait below — or observes draining and is rejected
// with 503. A WaitGroup Add can never race after this Wait begins.
//
// Closing the signal ends every connected SSE stream promptly, which lets a
// concurrently-running http.Server.Shutdown see idle connections and return
// instead of blocking on a live /event tail for the whole grace budget;
// disconnected orchestrators recover the trailing records via replay-from-seq.
//
// Drain then waits up to ctx's deadline for prompts to complete on their own;
// if ctx expires while prompts are still running, it cancels their contexts
// (which journals a durable session.aborted for each) and waits for them to
// unwind. It must be called before Close so the journal file stays open while
// the trailing records — the final assistant message and the
// session.aborted/idle transitions — are written; otherwise those records are
// lost on shutdown.
func (s *Server) Drain(ctx context.Context) {
	s.mu.Lock()
	s.draining = true
	s.closeOnce.Do(func() { close(s.closing) })
	s.mu.Unlock()
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

// Shutdown gracefully stops a harness serve instance. It runs the HTTP server's
// Shutdown and the Server's Drain CONCURRENTLY under one deadline, waits for
// both, and returns Shutdown's error.
//
// The two must overlap, not run in sequence:
//
//   - httpSrv.Shutdown closes the listener as its first synchronous action, so
//     no new request is accepted the instant shutdown begins. It then waits for
//     open connections to go idle.
//   - Drain closes the closing signal at entry, which ends connected SSE tails
//     promptly; that is what lets the concurrent Shutdown see idle connections
//     and return quickly instead of blocking on a live /event tail for the whole
//     grace budget. In parallel, Drain gives the detached prompt goroutines
//     (their 202 already returned; Shutdown does not track them) the full grace
//     budget to finish before it cancels them and journals their trailing
//     records.
//
// Running them sequentially either way loses: Shutdown-then-Drain would block
// Shutdown on the SSE tail, and Drain-then-Shutdown would keep the listener open
// for the whole drain window, admitting new prompts mid-drain (a data-loss bug
// the draining gate exists to prevent).
func Shutdown(ctx context.Context, httpSrv *http.Server, srv *Server) error {
	drained := make(chan struct{})
	go func() {
		srv.Drain(ctx)
		close(drained)
	}()
	err := httpSrv.Shutdown(ctx)
	<-drained
	return err
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
