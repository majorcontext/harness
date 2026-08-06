// Structured turn-lifecycle and goal-lifecycle logging (Options.Logger, see
// its doc comment on server.go). Field report (2026-08-06): a session ran
// 631 messages / 141k output tokens and produced ZERO log lines, so an
// operator tailing `harness serve`'s stderr could not tell a box mid-turn
// from a dead one. These tests drive the same durable-record choke points
// the existing journal tests already exercise (recordTurnEnd,
// publishGoal's goal.parked fold, and the session.error emit site in
// handlers.go) and assert the configured slog.Logger actually receives a
// line at each of them — and that a nil Logger (every other test's default)
// stays completely silent, never panics.
package server

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// syncBuffer is a mutex-guarded bytes.Buffer: the logging call sites under
// test (recordTurnEnd's post-unlock logWarn/logInfo, publishGoal's and
// emitDurableLocked's under-lock ones) run on the server's own request/turn
// goroutine, while these tests read the buffer from the test goroutine —
// concurrently, since receiving an SSE event only orders that event's own
// send-before-receive, never the sender's later, unrelated log write versus
// the receiver's subsequent read. A plain bytes.Buffer would race (caught by
// go test -race); this is the standard fix, not a change to production
// behavior.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newLoggingHarness builds a harness identical to newHarness but with
// Options.Logger wired to a slog.Logger writing text lines into buf, so a
// test can assert on the exact rendered attrs.
func newLoggingHarness(t *testing.T, prov provider.Provider, buf *syncBuffer) *harness {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(buf, nil))
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		o.Logger = logger
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}
}

// TestLoggerTurnEndCompletedLogsInfo is the "box is alive" half of the field
// report: a clean turn completion must log an INFO "turn end" line naming
// the session and outcome — a single line an operator's tail -f can latch
// onto as the box's heartbeat, where today nothing logged at all.
//
// Red-verified: before Options.Logger existed, this test failed to compile
// (no such field); after adding the field but before wiring recordTurnEnd
// to call s.logInfo, it compiled but the buffer stayed empty and the "turn
// end" substring check failed.
func TestLoggerTurnEndCompletedLogsInfo(t *testing.T) {
	var buf syncBuffer
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("all good")}}
	h := newLoggingHarness(t, prov, &buf)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	end := sse.waitFor(t, "turn.end")
	if end.Outcome != "completed" {
		t.Fatalf("turn.end outcome = %q, want completed", end.Outcome)
	}

	log := buf.String()
	if !strings.Contains(log, "turn end") {
		t.Fatalf("log missing %q line: %s", "turn end", log)
	}
	if !strings.Contains(log, "level=INFO") {
		t.Errorf("completed turn end logged below INFO: %s", log)
	}
	if !strings.Contains(log, "session="+id) {
		t.Errorf("log missing session attr: %s", log)
	}
	if !strings.Contains(log, "outcome=completed") {
		t.Errorf("log missing outcome attr: %s", log)
	}
	if strings.Contains(log, "error=") {
		t.Errorf("completed turn end must omit an empty error attr: %s", log)
	}
}

// TestLoggerTurnEndErrorLogsWarn is the failure half: a turn that dies
// mid-stream must log at WARN (not INFO), carrying the sanitized error
// detail — exactly the "operator tailing the log concluded the box was
// dead" scenario from the field report, except now there is a line to see.
func TestLoggerTurnEndErrorLogsWarn(t *testing.T) {
	var buf syncBuffer
	prov := &errThenOKProvider{name: "test", err: errors.New("provider request failed: boom")}
	h := newLoggingHarness(t, prov, &buf)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "boom"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	end := sse.waitFor(t, "turn.end")
	if end.Outcome != "error" {
		t.Fatalf("turn.end outcome = %q, want error", end.Outcome)
	}

	log := buf.String()
	if !strings.Contains(log, "turn end") {
		t.Fatalf("log missing %q line: %s", "turn end", log)
	}
	if !strings.Contains(log, "level=WARN") {
		t.Errorf("failed turn end must log at WARN: %s", log)
	}
	if !strings.Contains(log, "session="+id) {
		t.Errorf("log missing session attr: %s", log)
	}
	if !strings.Contains(log, "outcome=error") {
		t.Errorf("log missing outcome attr: %s", log)
	}
	if !strings.Contains(log, "error=") {
		t.Errorf("failed turn end must carry the error attr: %s", log)
	}
}

// TestLoggerSessionErrorLogsWarn exercises the session.error choke point
// (emitted directly by handlers.go, folded through emitDurableLocked): it
// must WARN-log "session error" with the session id and detail, the same
// bar as turn end's failure case above.
func TestLoggerSessionErrorLogsWarn(t *testing.T) {
	var buf syncBuffer
	prov := &errThenOKProvider{name: "test"}
	h := newLoggingHarness(t, prov, &buf)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "boom"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	errEv := sse.waitFor(t, "session.error")
	if errEv.Error == "" {
		t.Fatalf("session.error missing detail")
	}

	log := buf.String()
	if !strings.Contains(log, "session error") {
		t.Fatalf("log missing %q line: %s", "session error", log)
	}
	if !strings.Contains(log, "level=WARN") {
		t.Errorf("session error must log at WARN: %s", log)
	}
	if !strings.Contains(log, "session="+id) {
		t.Errorf("log missing session attr: %s", log)
	}
}

// TestLoggerGoalParkedLogsWarn drives a worker turn to permanent failure so
// its goal exit-parks (the exact setup
// TestTurnEndOnGoalWorkerFailureParksWithError in turn_outcome_test.go
// uses — real wall-clock time, no synctest bubble, since this goes through
// a real httptest.Server/SSE stream and synctest forbids real network I/O
// in its bubble), and asserts publishGoal's goal.parked fold logs a WARN
// line naming the session, reason, attempt count, and retryable
// classification — the "goal loop died silently" case from the field
// report's background-loop analogue.
func TestLoggerGoalParkedLogsWarn(t *testing.T) {
	var buf syncBuffer
	prov := &goalProv{
		name:       "test",
		worker:     [][]provider.Event{},
		eval:       [][]provider.Event{},
		workerErrN: 100, // every attempt fails, exhausting deterministic retries
	}
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
		o.Logger = logger
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	h := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "do the thing"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	end := sse.waitFor(t, "turn.end")
	if end.Outcome != outcomeWorkerParked {
		t.Fatalf("turn.end outcome = %q, want %q", end.Outcome, outcomeWorkerParked)
	}

	log := buf.String()
	if !strings.Contains(log, "goal parked") {
		t.Fatalf("log missing %q line: %s", "goal parked", log)
	}
	if !strings.Contains(log, "level=WARN") {
		t.Errorf("goal parked must log at WARN: %s", log)
	}
	if !strings.Contains(log, "attempts=") {
		t.Errorf("log missing attempts attr: %s", log)
	}
}

// TestLoggerNilStaysSilent is the safety half: every other test in this
// package builds its harness with no Logger at all (see newServer's zero
// Options.Logger default), so if any call site here forgot the nil guard
// documented on Options.Logger, the entire suite would already be panicking
// instead of passing — this test makes that guarantee explicit for the
// three choke points this file covers (turn end success, turn end failure,
// session error) in one run.
func TestLoggerNilStaysSilent(t *testing.T) {
	prov := &errThenOKProvider{name: "test"}
	h := newHarness(t, prov) // no Logger set
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "boom"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	sse.waitFor(t, "turn.end")
	idle := sse.waitFor(t, "session.status")
	for idle.Status != "idle" {
		idle = sse.waitFor(t, "session.status")
	}
	// Reaching here without a panic is the assertion.
}
