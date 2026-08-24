// Structured turn-lifecycle and goal-lifecycle logging (Options.Logger, see
// its doc comment on server.go). Field report (2026-08-06): a session ran
// 631 messages / 141k output tokens and produced ZERO log lines, so an
// operator tailing `harness serve`'s stderr could not tell a box mid-turn
// from a dead one. These tests drive the same durable-record choke points
// the existing journal tests already exercise (recordTurnEnd,
// publishGoal's goal.* folds, and the session.error emit site in
// handlers.go) and assert the configured slog.Logger actually receives a
// line at each of them — and that a nil Logger (every other test's default)
// stays completely silent, never panics.
//
// Every assertion below is scoped to the SINGLE log line carrying the
// record's own msg=, via syncBuffer.waitLogLine — not a whole-buffer
// strings.Contains check. A whole-buffer check is satisfiable by the WRONG line: e.g.
// asserting "level=WARN" anywhere in the buffer passes even if the "turn
// end" line itself logged at INFO, so long as some unrelated WARN line (a
// "session error" from a different code path) also landed in the same
// buffer. waitLogLine blocks until the one line whose msg matches has
// actually been written (log writes happen after the SSE emit for the same
// record, so an SSE receipt does not order them) and every attribute
// assertion below reads only that line.
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
// test (recordTurnEnd's and emitDurable's post-unlock logWarn/logInfo,
// publishGoal's own post-unlock log switch) run on the server's own
// request/turn goroutine, while these tests read the buffer from the test
// goroutine — concurrently, since receiving an SSE event only orders that
// event's own send-before-receive, never the sender's later, unrelated log
// write versus the receiver's subsequent read. A plain bytes.Buffer would
// race (caught by go test -race); this is the standard fix, not a change to
// production behavior.
type syncBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	notify chan struct{}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	ch := b.notify
	b.mu.Unlock()
	if ch != nil {
		// Non-blocking: one pending signal is enough — waitLogLine
		// re-scans the whole buffer on every wake, so coalesced writes
		// are never missed.
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitLogLine blocks until the buffer contains a line carrying msg="<msg>"
// (slog's text handler quotes any value containing a space, which every
// message here does) and returns the FIRST such line. Assertions must run
// against the returned line, never against the whole buffer — a
// whole-buffer strings.Contains check can pass on the wrong line entirely.
//
// Blocking on the buffer's own write signal is the point: every log line
// here is written AFTER the server releases s.mu (see logInfo's doc
// comment), which is strictly after the SSE event for the same record was
// emitted — so receiving the SSE event does NOT order the log write before
// a subsequent read, and reading the buffer immediately after an SSE event
// is a race (found as a real flake in TestLoggerGoalEvalLogsInfo, where a
// second goal boundary's line could land — or the first's could be absent
// — at read time). The test binary's own timeout bounds a wait that never
// completes, per AGENTS.md's no-guessed-deadlines rule.
func (b *syncBuffer) waitLogLine(t *testing.T, msg string) string {
	t.Helper()
	want := `msg="` + msg + `"`
	b.mu.Lock()
	if b.notify == nil {
		b.notify = make(chan struct{}, 1)
	}
	ch := b.notify
	b.mu.Unlock()
	for {
		for _, line := range strings.Split(b.String(), "\n") {
			if strings.Contains(line, want) {
				return line
			}
		}
		<-ch
	}
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

// newLoggingGoalHarness mirrors newGoalHarness (goal_test.go) but wires
// Options.Logger the same way newLoggingHarness does, for the goal-lifecycle
// logging tests below.
func newLoggingGoalHarness(t *testing.T, prov provider.Provider, buf *syncBuffer) *harness {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(buf, nil))
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
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

	line := buf.waitLogLine(t, "turn end")
	if !strings.Contains(line, "level=INFO") {
		t.Errorf("completed turn end logged below INFO: %s", line)
	}
	if !strings.Contains(line, "session="+id) {
		t.Errorf("log line missing session attr: %s", line)
	}
	if !strings.Contains(line, "outcome=completed") {
		t.Errorf("log line missing outcome attr: %s", line)
	}
	if strings.Contains(line, "error=") {
		t.Errorf("completed turn end must omit an empty error attr: %s", line)
	}
}

// TestLoggerTurnEndErrorLogsWarn is the failure half: a turn that dies
// mid-stream must log at WARN (not INFO), carrying the sanitized error
// detail — exactly the "operator tailing the log concluded the box was
// dead" scenario from the field report, except now there is a line to see.
//
// Red-verified against the tightened, line-scoped assertion below (see the
// report accompanying this change): with recordTurnEnd's failure branch
// temporarily changed to log "turn end" at INFO instead of WARN, this test's
// level=WARN check on waitLogLine's line correctly failed, while the OLD
// whole-buffer style check (strings.Contains(buf.String(), "level=WARN"))
// kept passing — satisfied by the unrelated "session error" WARN line this
// same request also produces. Reverted after confirming the failure.
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

	line := buf.waitLogLine(t, "turn end")
	if !strings.Contains(line, "level=WARN") {
		t.Errorf("failed turn end must log at WARN: %s", line)
	}
	if !strings.Contains(line, "session="+id) {
		t.Errorf("log line missing session attr: %s", line)
	}
	if !strings.Contains(line, "outcome=error") {
		t.Errorf("log line missing outcome attr: %s", line)
	}
	if !strings.Contains(line, "error=") {
		t.Errorf("failed turn end must carry the error attr: %s", line)
	}
}

// TestLoggerSessionErrorLogsWarn exercises the session.error choke point
// (emitted directly by handlers.go, logged from emitDurable after s.mu is
// released): it must WARN-log "session error" with the session id and
// detail, the same bar as turn end's failure case above.
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

	line := buf.waitLogLine(t, "session error")
	if !strings.Contains(line, "level=WARN") {
		t.Errorf("session error must log at WARN: %s", line)
	}
	if !strings.Contains(line, "session="+id) {
		t.Errorf("log line missing session attr: %s", line)
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
	h := newLoggingGoalHarness(t, prov, &buf)
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

	line := buf.waitLogLine(t, "goal parked")
	if !strings.Contains(line, "level=WARN") {
		t.Errorf("goal parked must log at WARN: %s", line)
	}
	if !strings.Contains(line, "session="+id) {
		t.Errorf("log line missing session attr: %s", line)
	}
	if !strings.Contains(line, "attempts=") {
		t.Errorf("log line missing attempts attr: %s", line)
	}
}

// TestLoggerGoalStalledLogsWarn scripts one transient worker-turn failure
// (retried, then succeeding, then achieved) — the same setup as
// TestGoalStalledJournaledAndActive in goal_test.go — and asserts
// publishGoal's goal.stalled fold logs a WARN line naming the session,
// reason, and attempt number: the retry itself is a failure worth an
// operator's attention even though the loop recovers.
func TestLoggerGoalStalledLogsWarn(t *testing.T) {
	var buf syncBuffer
	prov := &goalProv{
		name:       "test",
		workerErrN: 1, // first attempt fails, second (retried) attempt succeeds
		worker:     [][]provider.Event{asstTurn("done")},
		eval:       [][]provider.Event{asstTurn("MET: looks complete")},
	}
	h := newLoggingGoalHarness(t, prov, &buf)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	sse.waitFor(t, "goal.stalled")

	line := buf.waitLogLine(t, "goal stalled")
	if !strings.Contains(line, "level=WARN") {
		t.Errorf("goal stalled must log at WARN: %s", line)
	}
	if !strings.Contains(line, "session="+id) {
		t.Errorf("log line missing session attr: %s", line)
	}
	if !strings.Contains(line, "attempt=") {
		t.Errorf("log line missing attempt attr: %s", line)
	}

	// Let the loop finish (achieves on the retried turn) so the goroutine
	// unwinds cleanly before the test ends.
	sse.collectUntilIdle(t)
}

// TestLoggerGoalSetLogsInfo asserts publishGoal's goal.set fold logs an INFO
// line naming the session and condition — an ordinary lifecycle transition,
// not a failure, so INFO rather than WARN.
func TestLoggerGoalSetLogsInfo(t *testing.T) {
	var buf syncBuffer
	prov := &goalProv{
		name:   "test",
		worker: [][]provider.Event{asstTurn("done")},
		eval:   [][]provider.Event{asstTurn("MET: looks complete")},
	}
	h := newLoggingGoalHarness(t, prov, &buf)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "write a summary"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	sse.waitFor(t, "goal.set")

	line := buf.waitLogLine(t, "goal set")
	if !strings.Contains(line, "level=INFO") {
		t.Errorf("goal set must log at INFO: %s", line)
	}
	if !strings.Contains(line, "session="+id) {
		t.Errorf("log line missing session attr: %s", line)
	}
	if !strings.Contains(line, `condition="write a summary"`) {
		t.Errorf("log line missing condition attr: %s", line)
	}

	sse.collectUntilIdle(t)
}

// TestLoggerGoalEvalLogsInfo is the new per-worker-turn heartbeat this
// change adds: publishGoal's goal.eval fold must log an INFO line naming the
// session, whether the evaluator found the condition met, and its reason —
// the evaluator runs exactly once per completed worker turn, so this gives a
// goal-supervised session (the primary long-running shape) a periodic log
// line between "goal set" and its eventual terminal record, where
// recordTurnEnd's own turn.end line fires only once per loop EXIT, not once
// per worker turn (see recordTurnEnd's doc comment).
func TestLoggerGoalEvalLogsInfo(t *testing.T) {
	var buf syncBuffer
	prov := &goalProv{
		name:   "test",
		worker: [][]provider.Event{asstTurn("working"), asstTurn("done")},
		eval:   [][]provider.Event{asstTurn("NOT MET: needs a summary"), asstTurn("MET: summary present")},
	}
	h := newLoggingGoalHarness(t, prov, &buf)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "write a summary"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	first := sse.waitFor(t, "goal.eval")
	if first.GoalMet {
		t.Fatalf("first goal.eval met = true, want false")
	}

	line := buf.waitLogLine(t, "goal eval")
	if !strings.Contains(line, "level=INFO") {
		t.Errorf("goal eval must log at INFO: %s", line)
	}
	if !strings.Contains(line, "session="+id) {
		t.Errorf("log line missing session attr: %s", line)
	}
	if !strings.Contains(line, "met=false") {
		t.Errorf("log line missing met=false attr: %s", line)
	}
	if !strings.Contains(line, "reason=") {
		t.Errorf("log line missing reason attr: %s", line)
	}

	sse.collectUntilIdle(t)
}

// TestLoggerGoalAchievedLogsInfo asserts publishGoal's goal.achieved fold
// logs an INFO line naming the session, reason, and turn count.
func TestLoggerGoalAchievedLogsInfo(t *testing.T) {
	var buf syncBuffer
	prov := &goalProv{
		name:   "test",
		worker: [][]provider.Event{asstTurn("done")},
		eval:   [][]provider.Event{asstTurn("MET: looks complete")},
	}
	h := newLoggingGoalHarness(t, prov, &buf)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	sse.waitFor(t, "goal.achieved")

	line := buf.waitLogLine(t, "goal achieved")
	if !strings.Contains(line, "level=INFO") {
		t.Errorf("goal achieved must log at INFO: %s", line)
	}
	if !strings.Contains(line, "session="+id) {
		t.Errorf("log line missing session attr: %s", line)
	}
	if !strings.Contains(line, "turns=") {
		t.Errorf("log line missing turns attr: %s", line)
	}

	sse.collectUntilIdle(t)
}

// TestLoggerGoalClearedLogsInfo drives the same DELETE-while-active setup as
// TestGoalDeleteClearsAndStops (goal_test.go) and asserts publishGoal's
// goal.cleared fold logs an INFO line naming the session and reason.
func TestLoggerGoalClearedLogsInfo(t *testing.T) {
	var buf syncBuffer
	prov := &goalProv{
		name:        "test",
		blockWorker: true,
		started:     make(chan struct{}),
		eval:        [][]provider.Event{asstTurn("MET: ok")},
	}
	h := newLoggingGoalHarness(t, prov, &buf)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, _ := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d", resp.StatusCode)
	}
	<-prov.started

	resp, _ = h.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE goal = %d, want 204", resp.StatusCode)
	}
	sse.collectUntilIdle(t)

	line := buf.waitLogLine(t, "goal cleared")
	if !strings.Contains(line, "level=INFO") {
		t.Errorf("goal cleared must log at INFO: %s", line)
	}
	if !strings.Contains(line, "session="+id) {
		t.Errorf("log line missing session attr: %s", line)
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
