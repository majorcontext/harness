// Package e2e provides a REAL, non-mocked backend for verifying
// tools/monitor's board/detail/composer behavior end-to-end: a real
// server.Server (the same wiring as `harness serve`) fed by a handful of
// small scripted providers (no API key needed, and — for the tool-call
// scenarios — a REAL "bash" tool execution via a short `sleep`, not a
// simulated one), plus a tiny static file server for the ACTUAL committed
// tools/monitor/index.html (mirroring how a developer would statically host
// it per index.html's own header comment: "open it from file:// or serve it
// from any static host"). The box ALSO serves its own embedded copy at real
// GET /monitor (server.Options.MonitorPage, tools/monitor.Page — the same
// wiring cmd/harness's serveCmd uses), plus a SECOND, loopback-
// Unauthenticated box (Stub.UnauthBase) for the same-origin auto-connect
// scenarios (embeddedConnectPlan) that need a token-optional server.
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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/server"
	"github.com/majorcontext/harness/tools/monitor"
)

// RunToken is the fixed run token the stub box authenticates with.
const RunToken = "monitor-e2e-token"

// Provider names: one distinct registry key (and therefore one distinct
// scriptedProvider instance, with its own independent call counter) per
// e2e scenario/session, so two sessions can never accidentally share a
// turn index — see the doc comment on scriptedProvider.Stream.
const (
	ProvQuickIdle    = "e2e-quick-idle"    // composer-send-on-idle-session scenario
	ProvToolBoard    = "e2e-tool-board"    // board phase transitions (streaming + real tool)
	ProvToolDetail   = "e2e-tool-detail"   // detail view's live running-tool fold
	ProvStallStale   = "e2e-stall-stale"   // staleness tiers (quiet/stalled)
	ProvStallDedup   = "e2e-stall-dedup"   // busy composer send -> prompt.queued dedup
	ProvStreamError  = "e2e-stream-error"  // a genuine provider stream failure (detail transcript error entry)
	ProvReconnectGap = "e2e-reconnect-gap" // reconcileDetail's reconnect-gap-heal trigger (finding 1)
	ProvLiveCap      = "e2e-live-cap"      // reconcileDetail's liveEvents buffer-cap trigger (finding 3)
	ProvPendingThink = "e2e-pending-think" // "Thinking…" pending-assistant indicator (Change 2) — the idle-session optimistic-send scenario (Change 1) reuses ProvQuickIdle
)

// StreamErrorText is the exact (sanitize-passthrough — see errorTurns' doc
// comment) error text ProvStreamError's turn fails with; real_e2e.mjs
// duplicates this string (it has no Go tooling to import the const, same as
// the Prov* keys above) to assert the detail transcript's error entry
// renders the server's REAL error text, not a placeholder.
const StreamErrorText = "simulated upstream failure: connection reset by peer"

// ReconnectGapReply is the exact reply text ProvReconnectGap's turn ends
// with; real_e2e.mjs duplicates this string (same reasoning as
// StreamErrorText above) to assert the detail transcript eventually
// contains this turn's FULL content — proving reconcileDetail backfilled
// whatever the page's own SSE connection missed while it was down/
// reconnecting, not just that SOME turn happened to complete.
const ReconnectGapReply = "reconnect-gap turn landed"

// PendingThinkReply is the exact reply text ProvPendingThink's (delayed)
// turn ends with; real_e2e.mjs duplicates this string (same reasoning as
// StreamErrorText/ReconnectGapReply above) to assert the "Thinking…"
// pending-assistant indicator is dismissed exactly once THIS text starts
// streaming in, not merely "eventually".
const PendingThinkReply = "pending-think reply landed"

// scriptedTurn is one pre-built turn: a []provider.Event to stream, plus an
// optional terminal error. When err is set, scriptedStream.Next() returns it
// (instead of io.EOF) once events is exhausted — a REAL provider.Stream
// failure mid-turn, the same shape a real provider adapter's connection
// dying produces, not a simulated event. See engine/engine.go's runTurn: a
// non-nil, non-io.EOF Next() error with no tool call yet recorded (the case
// here — errorTurns below never includes a tool call) propagates straight
// out of Session.Prompt, driving server/handlers.go's runPrompt into its
// session.error + turn.end(outcome:"error") default branch — the exact wire
// path tools/monitor's transcript "error" entry (see index.html's
// pushIfNewError) exists to render.
//
// initialDelay, when set, is slept through before the turn's FIRST event is
// returned (see scriptedStream.Next) — used exclusively by
// pendingThinkTurns to give the "Thinking…" pending-assistant indicator
// scenario (tools/monitor/index.html's transcriptModel "pending" kind) a
// deterministic, CI-sane window in which the turn is genuinely
// busy-with-nothing-to-show-yet, rather than racing a real turn's own
// near-instant streaming start (every other scripted turn in this file
// streams its first event essentially immediately).
type scriptedTurn struct {
	events       []provider.Event
	err          error
	initialDelay time.Duration
}

// scriptedProvider serves one pre-built scriptedTurn per call, numbered from
// 0; calls beyond the scripted turns repeat the last one (defensive — a
// session should never be prompted more times than its script anticipates,
// but repeating beats an opaque io.EOF panic if a test timing assumption is
// ever off by one call). Each instance is used by exactly ONE session for
// exactly ONE test's turns — see the Prov* consts above — so its call
// counter is never shared or raced across sessions.
type scriptedProvider struct {
	mu    sync.Mutex
	name  string
	call  int
	turns []scriptedTurn
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
	return &scriptedStream{turn: p.turns[n]}, nil
}

type scriptedStream struct {
	turn scriptedTurn
	i    int
}

func (s *scriptedStream) Next() (provider.Event, error) {
	if s.i == 0 && s.turn.initialDelay > 0 {
		time.Sleep(s.turn.initialDelay)
	}
	if s.i >= len(s.turn.events) {
		if s.turn.err != nil {
			return provider.Event{}, s.turn.err
		}
		return provider.Event{}, io.EOF
	}
	ev := s.turn.events[s.i]
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
		Arguments: fmt.Appendf(nil, `{"command":"sleep %g"}`, sleepSeconds),
	}
}

// quickTurns: one plain text-only turn, no tool call — the baseline
// idle-session composer path (ProvQuickIdle).
func quickTurns(reply string) []scriptedTurn {
	return []scriptedTurn{
		{events: []provider.Event{textDelta(reply), doneEvent(provider.StopEndTurn, &message.Text{Text: reply})}},
	}
}

// pendingThinkTurns is quickTurns' single plain text-only turn with an
// initialDelay grafted onto it — see scriptedTurn's own doc comment for why:
// it gives the "Thinking…" pending-assistant indicator e2e scenario
// (ProvPendingThink) a deterministic window in which the turn is genuinely
// busy with nothing to show yet, without racing a real turn's own
// near-instant streaming start.
func pendingThinkTurns(delay time.Duration, reply string) []scriptedTurn {
	turns := quickTurns(reply)
	turns[0].initialDelay = delay
	return turns
}

// capTurns builds a single turn with MANY streaming text deltas (no tool
// call — keeps this fast, no real bash sleep needed) so its live event
// count comfortably and unambiguously overshoots any small tuned
// DETAIL_LIVE_EVENTS_CAP (see real_e2e.mjs's TUNING). This makes the "did
// liveEvents actually shrink back down after crossing the cap" e2e
// assertion a real, falsifiable regression check: a handful of events (as
// toolTurns' single turn produces) would coincidentally look "small" even
// with reconcileDetail's buffer-cap trigger removed outright, which
// wouldn't prove anything.
func capTurns(chunks int) []scriptedTurn {
	events := make([]provider.Event, 0, chunks+1)
	var full strings.Builder
	for i := 0; i < chunks; i++ {
		chunk := fmt.Sprintf("chunk%d ", i)
		events = append(events, textDelta(chunk))
		full.WriteString(chunk)
	}
	events = append(events, doneEvent(provider.StopEndTurn, &message.Text{Text: full.String()}))
	return []scriptedTurn{{events: events}}
}

// errorTurns builds a single scripted turn that streams a little text, then
// fails with a genuine provider error before ever reaching EventDone — the
// direct analog of a real provider connection dying mid-turn (see
// scriptedTurn's doc comment). No tool call is ever recorded, so
// engine/engine.go's runTurn returns the raw error unwrapped (not an
// interruptedTurnError), and errText survives unmolested through
// server/handlers.go's plugin.SanitizeSessionError (no credential-shaped
// substring, comfortably under its 256-rune cap) to land, byte for byte, in
// both the session.error and turn.end(outcome:"error") events' Error field —
// what the detail transcript's "error" entry (index.html's pushIfNewError)
// must render.
func errorTurns(partialText, errText string) []scriptedTurn {
	return []scriptedTurn{
		{events: []provider.Event{textDelta(partialText)}, err: errors.New(errText)},
	}
}

// toolTurns: turn 1 streams reasoning + text, then makes a real, briefly
// blocking bash tool call (StopToolUse); turn 2 (dispatched automatically by
// the engine once the tool result lands) streams a short final reply and
// ends the turn (StopEndTurn). Used by ProvToolBoard/ProvToolDetail — the
// board-phase-transition and running-tool-fold scenarios.
func toolTurns(reasoning, midText, callID string, sleepSeconds float64, finalText string) []scriptedTurn {
	return []scriptedTurn{
		{events: []provider.Event{
			reasoningDelta(reasoning),
			textDelta(midText),
			doneEvent(provider.StopToolUse,
				&message.Reasoning{Text: reasoning},
				&message.Text{Text: midText},
				toolCallPart(callID, sleepSeconds)),
		}},
		{events: []provider.Event{textDelta(finalText), doneEvent(provider.StopEndTurn, &message.Text{Text: finalText})}},
	}
}

// Stub is a running (box server, monitor static file server) pair plus its
// teardown.
type Stub struct {
	BoxBase     string // e.g. "http://127.0.0.1:54321" — a real harness serve-equivalent
	MonitorBase string // e.g. "http://127.0.0.1:54322" — serves the real, committed tools/monitor/index.html
	Token       string
	// UnauthBase is a SECOND, independent box — server.Options.RunToken ""
	// + Unauthenticated true, MonitorPage set — for embeddedConnectPlan's
	// auto-attempt-same-origin scenario against a genuinely token-optional
	// server (cmd/harness's loopback case (b)). It cannot be the same
	// server.Server as BoxBase: every OTHER scenario in this package
	// depends on BoxBase actually enforcing its RunToken.
	UnauthBase string

	boxAddr    string // fixed host:port the box listens on — reused across Kill/Restart
	boxHTTP    *http.Server
	boxLn      net.Listener
	monitorLn  net.Listener
	unauthLn   net.Listener
	unauthHTTP *http.Server
	srv        *server.Server
	unauthSrv  *server.Server
	sessionDir string
	unauthDir  string
	// messageFailures backs the /__control/fail-message injection point
	// (see armMessageFailure and flakyMessageInjector) — a test-only fault
	// the monitor's self-heal (index.html's maybeSelfHealDetail/
	// detailUnderpopulated) is meant to survive, never a real server
	// behavior. Shared across KillBox/RestartBox (the SAME injector wraps
	// whichever *http.Server is currently serving BoxBase).
	messageFailures *flakyMessageInjector
	mu              sync.Mutex
}

// flakyMessageInjector wraps the REAL server.Server (never modifying it —
// "no server changes" holds for the product code, exactly like KillBox/
// RestartBox's own doc comment) to inject a transient failure into a
// specific session's GET /session/{id}/message responses for a bounded
// window: real_e2e.mjs's self-heal scenario arms a deadline for one
// session, opens a deep-linked page against it, and observes that
// enterDetail's own initial history fetch (and any fetch racing it — the
// stream-open reconcile, enterDetail's own immediate follow-up check) all
// genuinely fail during that window, so ONLY maybeSelfHealDetail's
// pollOnce-driven trigger — the mechanism actually under test — is left to
// close the gap once the window lapses. Every other request (every other
// session's history, /health, /session, /event, POSTs) passes straight
// through untouched.
type flakyMessageInjector struct {
	inner http.Handler
	mu    sync.Mutex
	// deadlines[sessionID] is the time before which that session's GET
	// .../message requests are failed. Absent (zero Time) means "never
	// armed" — the common case for every scenario that isn't this one.
	deadlines map[string]time.Time
}

func newFlakyMessageInjector(inner http.Handler) *flakyMessageInjector {
	return &flakyMessageInjector{inner: inner, deadlines: map[string]time.Time{}}
}

// arm fails the given session's GET .../message requests for dur, starting
// now. A zero/negative dur disarms (deletes the deadline) rather than
// leaving a past-but-present entry around.
func (f *flakyMessageInjector) arm(sessionID string, dur time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if dur <= 0 {
		delete(f.deadlines, sessionID)
		return
	}
	f.deadlines[sessionID] = time.Now().Add(dur)
}

// messageSessionID extracts "<id>" from a "/session/<id>/message" path, or
// "" if the path doesn't match that exact shape — deliberately narrow (no
// wildcard/prefix matching) so this can never misfire on some OTHER route
// server.Server happens to register under /session/.
func messageSessionID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 3 && parts[0] == "session" && parts[2] == "message" && parts[1] != "" {
		return parts[1]
	}
	return ""
}

func (f *flakyMessageInjector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if sid := messageSessionID(r.URL.Path); sid != "" {
			f.mu.Lock()
			deadline, armed := f.deadlines[sid]
			f.mu.Unlock()
			if armed && time.Now().Before(deadline) {
				http.Error(w, "e2e-injected transient failure (self-heal scenario)", http.StatusInternalServerError)
				return
			}
		}
	}
	f.inner.ServeHTTP(w, r)
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
			scriptedTurn{events: []provider.Event{textDelta("queued reply landed"), doneEvent(provider.StopEndTurn, &message.Text{Text: "queued reply landed"})}},
		)},
		ProvStreamError:  &scriptedProvider{name: ProvStreamError, turns: errorTurns("starting the request", StreamErrorText)},
		ProvReconnectGap: &scriptedProvider{name: ProvReconnectGap, turns: quickTurns(ReconnectGapReply)},
		ProvLiveCap:      &scriptedProvider{name: ProvLiveCap, turns: capTurns(6)},
		ProvPendingThink: &scriptedProvider{name: ProvPendingThink, turns: pendingThinkTurns(400*time.Millisecond, PendingThinkReply)},
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
		// MonitorPage: this box also serves its own embedded copy at real
		// GET /monitor, exactly like cmd/harness's serveCmd wires it — see
		// the reconnect-gap/buffer-cap scenarios above for the tokened,
		// static-hosted path; the embeddedConnectPlan scenarios further
		// down in real_e2e.mjs load THIS route directly (BoxBase +
		// "/monitor[#t=...]") to prove the real thing, not a simulation.
		MonitorPage: monitor.Page,
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

	// unauthSrv: a SECOND, independent box for embeddedConnectPlan's
	// auto-attempt-same-origin scenario against cmd/harness's loopback
	// case (b) — RunToken "" + Unauthenticated true. No scripted providers
	// are needed (this scenario only proves connect/board-render with zero
	// typing, never runs a turn), so NewSession/LoadSession are non-nil
	// only to satisfy server.New's own requirement — never expected to be
	// called.
	unauthDir, err := os.MkdirTemp("", "monitor-e2e-unauth-sessions")
	if err != nil {
		os.RemoveAll(dir) //nolint:errcheck
		return nil, err
	}
	unauthSrv, err := server.New(server.Options{
		SessionDir:      unauthDir,
		Version:         "monitor-e2e-unauth",
		Unauthenticated: true,
		MonitorPage:     monitor.Page,
		NewSession: func(m message.ModelRef, workDir string, parentSession string) (*engine.Session, error) {
			return nil, errors.New("not exercised by the embeddedConnectPlan scenario")
		},
		LoadSession: func(id string) (*engine.Session, error) {
			return nil, errors.New("not exercised by the embeddedConnectPlan scenario")
		},
	})
	if err != nil {
		os.RemoveAll(dir)       //nolint:errcheck
		os.RemoveAll(unauthDir) //nolint:errcheck
		return nil, err
	}

	boxLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		srv.Close()       //nolint:errcheck
		unauthSrv.Close() //nolint:errcheck
		os.RemoveAll(dir)
		os.RemoveAll(unauthDir)
		return nil, err
	}
	monitorLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		boxLn.Close()     //nolint:errcheck
		srv.Close()       //nolint:errcheck
		unauthSrv.Close() //nolint:errcheck
		os.RemoveAll(dir)
		os.RemoveAll(unauthDir)
		return nil, err
	}
	unauthLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		boxLn.Close()     //nolint:errcheck
		monitorLn.Close() //nolint:errcheck
		srv.Close()       //nolint:errcheck
		unauthSrv.Close() //nolint:errcheck
		os.RemoveAll(dir)
		os.RemoveAll(unauthDir)
		return nil, err
	}

	messageFailures := newFlakyMessageInjector(srv)
	boxHTTP := &http.Server{Handler: messageFailures}
	go boxHTTP.Serve(boxLn) //nolint:errcheck
	unauthHTTP := &http.Server{Handler: unauthSrv}
	go unauthHTTP.Serve(unauthLn) //nolint:errcheck

	monitorDir, err := staticMonitorDir()
	if err != nil {
		boxLn.Close()     //nolint:errcheck
		monitorLn.Close() //nolint:errcheck
		unauthLn.Close()  //nolint:errcheck
		srv.Close()       //nolint:errcheck
		unauthSrv.Close() //nolint:errcheck
		os.RemoveAll(dir)
		os.RemoveAll(unauthDir)
		return nil, err
	}

	stub := &Stub{
		BoxBase:         "http://" + boxLn.Addr().String(),
		MonitorBase:     "http://" + monitorLn.Addr().String(),
		Token:           RunToken,
		UnauthBase:      "http://" + unauthLn.Addr().String(),
		boxAddr:         boxLn.Addr().String(),
		boxHTTP:         boxHTTP,
		boxLn:           boxLn,
		monitorLn:       monitorLn,
		unauthLn:        unauthLn,
		unauthHTTP:      unauthHTTP,
		srv:             srv,
		unauthSrv:       unauthSrv,
		sessionDir:      dir,
		unauthDir:       unauthDir,
		messageFailures: messageFailures,
	}

	// The monitor's own static-file listener also carries a tiny, TEST-ONLY
	// control plane (POST /__control/kill, /__control/restart, /__control/
	// fail-message) so real_e2e.mjs — a separate OS process from this one,
	// with no other way to reach Go method calls — can drive the reconnect
	// scenario's server-side kill/restart (see KillBox/RestartBox) and the
	// self-heal scenario's fault injection (see ArmMessageFailure). This is
	// scoped entirely to this e2e stub's own auxiliary listener; the REAL
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
	mux.HandleFunc("POST /__control/fail-message", func(w http.ResponseWriter, r *http.Request) {
		sid := r.URL.Query().Get("session")
		if sid == "" {
			http.Error(w, "missing session query param", http.StatusBadRequest)
			return
		}
		forMs, err := strconv.Atoi(r.URL.Query().Get("for_ms"))
		if err != nil {
			http.Error(w, "missing/invalid for_ms query param: "+err.Error(), http.StatusBadRequest)
			return
		}
		stub.ArmMessageFailure(sid, time.Duration(forMs)*time.Millisecond)
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
	// Reuse the SAME messageFailures injector (not a fresh one wrapping
	// s.srv directly) so an armed deadline survives a kill/restart —
	// matters only to the self-heal scenario, harmless to every other one
	// since messageFailures passes everything through when nothing's armed.
	s.boxHTTP = &http.Server{Handler: s.messageFailures}
	go s.boxHTTP.Serve(ln) //nolint:errcheck
	return nil
}

// ArmMessageFailure makes sessionID's GET /session/{id}/message responses
// fail with a real 500 for dur, starting now (see flakyMessageInjector's own
// doc comment) — the self-heal scenario's fault injection. A zero/negative
// dur disarms immediately. Safe to call at any time; takes effect on the
// very next matching request, in-flight or not yet started.
func (s *Stub) ArmMessageFailure(sessionID string, dur time.Duration) {
	s.messageFailures.arm(sessionID, dur)
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
	if s.unauthHTTP != nil {
		s.unauthHTTP.Close() //nolint:errcheck
	}
	s.mu.Unlock()
	if s.monitorLn != nil {
		s.monitorLn.Close() //nolint:errcheck
	}
	if s.srv != nil {
		s.srv.Close() //nolint:errcheck
	}
	if s.unauthSrv != nil {
		s.unauthSrv.Close() //nolint:errcheck
	}
	if s.sessionDir != "" {
		os.RemoveAll(s.sessionDir) //nolint:errcheck
	}
	if s.unauthDir != "" {
		os.RemoveAll(s.unauthDir) //nolint:errcheck
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
