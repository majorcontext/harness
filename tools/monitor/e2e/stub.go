// Package e2e provides a REAL, non-mocked backend for verifying
// tools/monitor's board/detail/composer behavior end-to-end: a real
// server.Server (the same wiring as `harness serve`) fed by a handful of
// small scripted providers (no API key needed, and — for the tool-call
// scenarios — a REAL "bash" tool execution via a short `sleep`, not a
// simulated one), plus a tiny static file server for the ACTUAL committed
// tools/monitor/index.html (the monitor has no Go-side handler of its own —
// see index.html's header comment: "open it from file:// or serve it from
// any static host" — so this test hosts it exactly the way a developer
// would, unlike tools/hub which embeds and serves its page from Go).
//
// This exists specifically to answer "does the monitor page actually work
// against a real harness box, or only against hand-rolled mocks in a JS
// unit test?" — see e2e_test.go, which drives the real page (via Node +
// jsdom, since this repo's UI has no other DOM available) with Node's own,
// unmocked fetch against the servers Start returns. Mirrors
// tools/hub/e2e/stub.go's structure and naming throughout.
package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/server"
)

// RunToken is the fixed run token the stub box authenticates with.
const RunToken = "monitor-e2e-token"

// Provider names: one distinct registry key (and therefore one distinct
// scriptedProvider instance, with its own independent call counter) per
// e2e scenario/session, so two sessions can never accidentally share a
// turn index — see the doc comment on scriptedProvider.Stream.
const (
	ProvQuickIdle  = "e2e-quick-idle"  // composer-send-on-idle-session scenario
	ProvToolBoard  = "e2e-tool-board"  // board phase transitions (streaming + real tool)
	ProvToolDetail = "e2e-tool-detail" // detail view's live running-tool fold
	ProvStallStale = "e2e-stall-stale" // staleness tiers (quiet/stalled)
	ProvStallDedup = "e2e-stall-dedup" // busy composer send -> prompt.queued dedup
)

// scriptedProvider serves one pre-built turn (a []provider.Event) per call,
// numbered from 0; calls beyond the scripted turns repeat the last one
// (defensive — a session should never be prompted more times than its
// script anticipates, but repeating beats an opaque io.EOF panic if a test
// timing assumption is ever off by one call). Each instance is used by
// exactly ONE session for exactly ONE test's turns — see the Prov* consts
// above — so its call counter is never shared or raced across sessions.
type scriptedProvider struct {
	mu    sync.Mutex
	name  string
	call  int
	turns [][]provider.Event
}

func (p *scriptedProvider) Name() string { return p.name }

func (p *scriptedProvider) Stream(_ context.Context, _ *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	n := p.call
	if n >= len(p.turns) {
		n = len(p.turns) - 1
	}
	p.call++
	p.mu.Unlock()
	return &scriptedStream{events: p.turns[n]}, nil
}

type scriptedStream struct {
	events []provider.Event
	i      int
}

func (s *scriptedStream) Next() (provider.Event, error) {
	if s.i >= len(s.events) {
		return provider.Event{}, io.EOF
	}
	ev := s.events[s.i]
	s.i++
	return ev, nil
}

func (s *scriptedStream) Close() error { return nil }

var msgSeq int64

func nextMsgID() string {
	n := atomic.AddInt64(&msgSeq, 1)
	return fmt.Sprintf("msg_e2e_%d", n)
}

// textDelta/reasoningDelta build the two streaming-event kinds the monitor's
// reduceActivity/transcriptModel render live (see index.html's "streaming"
// phase and the draft's openText/openReasoning).
func textDelta(s string) provider.Event {
	return provider.Event{Type: provider.EventTextDelta, Text: s}
}
func reasoningDelta(s string) provider.Event {
	return provider.Event{Type: provider.EventReasoningDelta, Text: s}
}

// doneEvent builds the turn-closing EventDone, whose Message carries the
// durable, fully-assembled parts (matching what the preceding deltas above
// streamed, same convention tools/hub/e2e/stub.go uses).
func doneEvent(stop provider.StopReason, parts ...message.Part) provider.Event {
	return provider.Event{
		Type:       provider.EventDone,
		StopReason: stop,
		Message:    &message.Message{ID: nextMsgID(), Role: message.RoleAssistant, Parts: parts},
	}
}

// toolCallPart builds a real *message.ToolCall part naming the engine's
// built-in "bash" tool (see engine/bash.go — registered unconditionally in
// every engine.Config) with a `sleep <secs>` command: the engine actually
// shells out and blocks for that long, a REAL tool execution with a
// deterministic, controllable duration — not a mocked delay — which is what
// lets these scenarios observe a genuine "tool" phase / running fold / (with
// the shrunk test-only staleness thresholds, see index.html's
// window.__monitorTuning seam) a genuine quiet/stalled tier.
func toolCallPart(callID string, sleepSeconds float64) *message.ToolCall {
	return &message.ToolCall{
		CallID:    callID,
		Name:      "bash",
		Arguments: []byte(fmt.Sprintf(`{"command":"sleep %g"}`, sleepSeconds)),
	}
}

// quickTurns: one plain text-only turn, no tool call — the baseline
// idle-session composer path (ProvQuickIdle).
func quickTurns(reply string) [][]provider.Event {
	return [][]provider.Event{
		{textDelta(reply), doneEvent(provider.StopEndTurn, &message.Text{Text: reply})},
	}
}

// toolTurns: turn 1 streams reasoning + text, then makes a real, briefly
// blocking bash tool call (StopToolUse); turn 2 (dispatched automatically by
// the engine once the tool result lands) streams a short final reply and
// ends the turn (StopEndTurn). Used by ProvToolBoard/ProvToolDetail — the
// board-phase-transition and running-tool-fold scenarios.
func toolTurns(reasoning, midText, callID string, sleepSeconds float64, finalText string) [][]provider.Event {
	return [][]provider.Event{
		{
			reasoningDelta(reasoning),
			textDelta(midText),
			doneEvent(provider.StopToolUse,
				&message.Reasoning{Text: reasoning},
				&message.Text{Text: midText},
				toolCallPart(callID, sleepSeconds)),
		},
		{textDelta(finalText), doneEvent(provider.StopEndTurn, &message.Text{Text: finalText})},
	}
}

// Stub is a running (box server, monitor static file server) pair plus its
// teardown.
type Stub struct {
	BoxBase     string // e.g. "http://127.0.0.1:54321" — a real harness serve-equivalent
	MonitorBase string // e.g. "http://127.0.0.1:54322" — serves the real, committed tools/monitor/index.html
	Token       string

	boxAddr    string // fixed host:port the box listens on — reused across Kill/Restart
	boxHTTP    *http.Server
	boxLn      net.Listener
	monitorLn  net.Listener
	srv        *server.Server
	sessionDir string
	mu         sync.Mutex
}

// Start builds and starts a real box server (server.New, five scripted
// providers, CORS wide open — see the Prov* consts) and a plain static file
// server for the actual tools/monitor/index.html on disk (found relative to
// this source file, so it always serves the exact committed file, not a
// stale copy), each on an OS-assigned port on loopback. Close tears both
// down. SessionDir is a fresh temp directory (real on-disk journal, same as
// production), removed by Close.
func Start() (*Stub, error) {
	dir, err := os.MkdirTemp("", "monitor-e2e-sessions")
	if err != nil {
		return nil, err
	}

	reg := provider.Registry{
		ProvQuickIdle:  &scriptedProvider{name: ProvQuickIdle, turns: quickTurns("hello — this is the idle-session reply")},
		ProvToolBoard:  &scriptedProvider{name: ProvToolBoard, turns: toolTurns("thinking it over", "looking into it", "tc-board", 0.6, "all set")},
		ProvToolDetail: &scriptedProvider{name: ProvToolDetail, turns: toolTurns("checking the fold", "on it", "tc-detail", 1.0, "fold resolved")},
		ProvStallStale: &scriptedProvider{name: ProvStallStale, turns: toolTurns("this will take a while", "starting the slow one", "tc-stale", 2.8, "finally done")},
		ProvStallDedup: &scriptedProvider{name: ProvStallDedup, turns: append(
			toolTurns("working on the first ask", "on it", "tc-dedup", 1.4, "first one done"),
			[]provider.Event{textDelta("queued reply landed"), doneEvent(provider.StopEndTurn, &message.Text{Text: "queued reply landed"})},
		)},
	}

	var srv *server.Server
	mkCfg := func(m message.ModelRef) engine.Config {
		return engine.Config{
			Providers:  reg,
			Model:      m,
			WorkDir:    dir,
			SessionDir: dir,
			OnEvent:    func(ev engine.Event) { srv.Publish(ev) },
		}
	}
	srv, err = server.New(server.Options{
		SessionDir: dir,
		RunToken:   RunToken,
		Version:    "monitor-e2e",
		CORSOrigin: "*",
		NewSession: func(m message.ModelRef, workDir string, parentSession string) (*engine.Session, error) {
			cfg := mkCfg(m)
			cfg.WorkDir = workDir
			cfg.ParentSession = parentSession
			return engine.NewSession(cfg), nil
		},
		LoadSession: func(id string) (*engine.Session, error) {
			return engine.LoadSession(mkCfg(message.ModelRef{}), id)
		},
	})
	if err != nil {
		os.RemoveAll(dir) //nolint:errcheck
		return nil, err
	}

	boxLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		srv.Close() //nolint:errcheck
		os.RemoveAll(dir)
		return nil, err
	}
	monitorLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		boxLn.Close() //nolint:errcheck
		srv.Close()   //nolint:errcheck
		os.RemoveAll(dir)
		return nil, err
	}

	boxHTTP := &http.Server{Handler: srv}
	go boxHTTP.Serve(boxLn) //nolint:errcheck

	monitorDir, err := staticMonitorDir()
	if err != nil {
		boxLn.Close()     //nolint:errcheck
		monitorLn.Close() //nolint:errcheck
		srv.Close()       //nolint:errcheck
		os.RemoveAll(dir)
		return nil, err
	}

	stub := &Stub{
		BoxBase:     "http://" + boxLn.Addr().String(),
		MonitorBase: "http://" + monitorLn.Addr().String(),
		Token:       RunToken,
		boxAddr:     boxLn.Addr().String(),
		boxHTTP:     boxHTTP,
		boxLn:       boxLn,
		monitorLn:   monitorLn,
		srv:         srv,
		sessionDir:  dir,
	}

	// The monitor's own static-file listener also carries a tiny, TEST-ONLY
	// control plane (POST /__control/kill, /__control/restart) so
	// real_e2e.mjs — a separate OS process from this one, with no other way
	// to reach Go method calls — can drive the reconnect scenario's
	// server-side kill/restart (see KillBox/RestartBox). This is scoped
	// entirely to this e2e stub's own auxiliary listener; the REAL
	// server.Server (the box) gains no routes at all — "no server changes"
	// (the plan's own words) holds for the product code.
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(monitorDir)))
	mux.HandleFunc("POST /__control/kill", func(w http.ResponseWriter, _ *http.Request) {
		if err := stub.KillBox(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /__control/restart", func(w http.ResponseWriter, _ *http.Request) {
		if err := stub.RestartBox(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	go http.Serve(monitorLn, mux) //nolint:errcheck

	return stub, nil
}

// KillBox forcibly severs the box's HTTP layer — every accepted connection,
// including an in-flight SSE stream, per (*http.Server).Close's "no graceful
// close" contract — while leaving the underlying server.Server (sessions,
// journal, scripted providers) completely intact. This is the reconnect
// scenario's "server-side kill": the transport drops out from under the
// monitor page exactly as a network blip or a `harness serve` process
// restart would, without the test having to tear down and reconstruct all
// the session state a fresh restart would otherwise force it to redo.
func (s *Stub) KillBox() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boxHTTP.Close()
}

// RestartBox rebinds a fresh http.Server to the SAME host:port KillBox just
// vacated (a listening socket this process closed itself is immediately
// reusable — it never lingers in TIME_WAIT the way an established
// connection's active-close side does), serving the SAME underlying
// server.Server. The monitor's already-configured base URL and jittered
// reconnect loop (index.html's connectStream/scheduleReconnect) need no
// changes to find it again.
func (s *Stub) RestartBox() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ln, err := net.Listen("tcp", s.boxAddr)
	if err != nil {
		return err
	}
	s.boxLn = ln
	s.boxHTTP = &http.Server{Handler: s.srv}
	go s.boxHTTP.Serve(ln) //nolint:errcheck
	return nil
}

// Close tears down both listeners, closes the server, and removes the
// temporary session directory. Safe to call after KillBox (boxHTTP.Close is
// itself idempotent-safe to call twice per net/http's docs).
func (s *Stub) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.boxHTTP != nil {
		s.boxHTTP.Close() //nolint:errcheck
	}
	s.mu.Unlock()
	if s.monitorLn != nil {
		s.monitorLn.Close() //nolint:errcheck
	}
	if s.srv != nil {
		s.srv.Close() //nolint:errcheck
	}
	if s.sessionDir != "" {
		os.RemoveAll(s.sessionDir) //nolint:errcheck
	}
}

// staticMonitorDir locates tools/monitor (this source file's own directory,
// tools/monitor/e2e, one level up) via runtime.Caller rather than the
// process's working directory — robust regardless of how the test binary
// was invoked — so the static file server above serves the ACTUAL committed
// index.html, not a copy. Same "byte-for-byte, production wiring" guarantee
// tools/hub/e2e's real_e2e.mjs checks explicitly (there via go:embed
// instead, since the hub — unlike the monitor — has a Go-side handler).
func staticMonitorDir() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("could not determine tools/monitor/e2e directory")
	}
	up := filepath.Join(filepath.Dir(thisFile), "..")
	if _, err := os.Stat(filepath.Join(up, "index.html")); err != nil {
		return "", fmt.Errorf("locating tools/monitor/index.html from %s: %w", up, err)
	}
	return up, nil
}
