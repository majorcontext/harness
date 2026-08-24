package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// gateProvider is a goal-loop provider whose worker turn blocks in Next until
// released, then completes with a scripted assistant message — unlike
// goalProv's blockWorker (which blocks forever, only unblocked by context
// cancellation), this lets a test observe the goal ACTIVE (worker in flight)
// and then drive it all the way to goal.achieved, deterministically, with no
// sleeps: the test synchronizes on workerStarted/workerReleased channels.
type gateProvider struct {
	name string

	mu   sync.Mutex
	eval [][]provider.Event
	ei   int

	workerStarted  chan struct{}
	workerReleased chan struct{}
	once           sync.Once
}

func (p *gateProvider) Name() string { return p.name }

func (p *gateProvider) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if len(req.Tools) == 0 { // evaluator (tool-less)
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.ei >= len(p.eval) {
			return &scriptedStream{}, nil
		}
		ev := p.eval[p.ei]
		p.ei++
		return &scriptedStream{events: ev}, nil
	}
	return &gateStream{ctx: ctx, p: p}, nil
}

type gateStream struct {
	ctx context.Context
	p   *gateProvider
}

func (s *gateStream) Next() (provider.Event, error) {
	s.p.once.Do(func() { close(s.p.workerStarted) })
	select {
	case <-s.ctx.Done():
		return provider.Event{}, s.ctx.Err()
	case <-s.p.workerReleased:
		msg := &message.Message{ID: "msg_gate_done", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "done"}}}
		return provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}, nil
	}
}

func (s *gateStream) Close() error { return nil }

// waitForWaiterCount polls (no sleep, runtime.Gosched only — same pattern as
// TestGoalDeleteClearBeforeIdleRace's journalHasIdle poll) until the server's
// waiter registry reaches want, or fails the test if it never does within a
// generous number of scheduling rounds.
func waitForWaiterCount(t *testing.T, srv *Server, want int) {
	t.Helper()
	for i := 0; i < 200000; i++ {
		srv.mu.Lock()
		n := len(srv.waiters)
		srv.mu.Unlock()
		if n == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("waiter count never reached %d", want)
}

// TestCompositeStateGoalRunningDuringBlockedWorker is the red-first test for
// DELIVER (1): while a goal loop's worker turn is blocked mid-stream (the
// session is unambiguously occupied by the goal), Session JSON and
// /session/status must both report state=goal-running — never plain "idle"
// — regardless of the momentary busy/idle status value. This is the state
// the composite field exists to make unambiguous.
func TestCompositeStateGoalRunningDuringBlockedWorker(t *testing.T) {
	prov := &goalProv{
		name:        "test",
		blockWorker: true,
		started:     make(chan struct{}),
		eval:        [][]provider.Event{asstTurn("MET: ok")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")
	t.Cleanup(func() {
		h.srv.mu.Lock()
		st := h.srv.sessions[id]
		var cancel context.CancelFunc
		if st != nil {
			cancel = st.cancel
		}
		h.srv.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	})

	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // worker turn now blocked mid-stream; the goal occupies the session

	// Session detail: state must be goal-running, status stays the legacy
	// busy/idle value untouched (backward compat).
	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var sess struct {
		Status string `json:"status"`
		State  string `json:"state"`
		Goal   *struct {
			Active bool `json:"active"`
		} `json:"goal"`
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.State != "goal-running" {
		t.Errorf("session state = %q, want goal-running", sess.State)
	}
	if sess.State == "idle" {
		t.Error("session state must never read idle while a goal is active")
	}
	if sess.Goal == nil || !sess.Goal.Active {
		t.Errorf("goal = %+v, want active", sess.Goal)
	}

	// /session/status must agree.
	resp, data = h.do("GET", "/session/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET status %d: %s", resp.StatusCode, data)
	}
	var statuses map[string]struct {
		Type  string `json:"type"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &statuses); err != nil {
		t.Fatal(err)
	}
	if statuses[id].State != "goal-running" {
		t.Errorf("/session/status state = %q, want goal-running: %s", statuses[id].State, data)
	}

	resp, _ = h.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE goal = %d, want 204", resp.StatusCode)
	}
}

// TestCompositeStateBusyDuringForcedIdlePause is the integration-level red
// test for the nimble-pizza incident (2026-08-06, live): a session whose
// goal is worker-parked (pause_reason "worker_failure", forcesIdlePause==
// true) read state="idle" even while an ordinary prompt turn was actively
// streaming — an operator watching the monitor concluded the box was dead
// mid-turn. forceIdle exists because no loop drives the GOAL; it must never
// claim a running TURN isn't running. Mirrors
// TestCompositeStateGoalRunningDuringBlockedWorker's shape (same dual GET
// /session + GET /session/status check) but for the forceIdle case instead
// of the plain goal-active case, and
// TestAutoArmAfterRestartResetsPausePresentation's blockWorkerAfter
// mid-turn-checkpoint technique.
//
// Red-verified: against the pre-fix compositeState (forceIdle checked
// before running, unconditionally returning "idle"), the mid-turn
// assertions below fail — state reads "idle" while an ordinary prompt turn
// is provably still in flight.
func TestCompositeStateBusyDuringForcedIdlePause(t *testing.T) {
	prov := &goalProv{
		name:       "test",
		workerErrN: goalWorkerRetriesForTest + 1,
		workerErr:  permanentWorkerErr(),
		// The first goalWorkerRetriesForTest+1 tool-bearing calls are the
		// goal loop's own failing attempts (workerErrN, counted before the
		// blockWorkerAfter gate since goalProv.Stream checks blockWorkerAfter
		// first but only blocks calls PAST the threshold) — they exhaust the
		// deterministic retry budget and exit-park the goal. The very next
		// tool-bearing call — the plain resume prompt's own turn, below —
		// is the one blockWorkerAfter actually blocks, giving a
		// deterministic mid-turn checkpoint.
		blockWorkerAfter: goalWorkerRetriesForTest + 1,
		started:          make(chan struct{}),
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")

	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t) // the park's own turn-end and idle

	mid := h.getPausedGoalView(id)
	if mid.Goal == nil || !mid.Goal.Active || !mid.Goal.Paused || mid.Goal.PauseReason != "worker_failure" {
		t.Fatalf("goal right after park = %+v, want active/paused/worker_failure", mid.Goal)
	}
	if mid.State != "idle" {
		t.Fatalf("state right after park (nothing running) = %q, want idle", mid.State)
	}

	// An ordinary prompt now starts against the still-parked goal. Its own
	// turn is the 4th tool-bearing call overall, which blockWorkerAfter
	// blocks mid-stream — a deterministic window in which the goal is
	// provably still worker-failure-paused AND the turn is provably still
	// running.
	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hello"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt status %d: %s", resp.StatusCode, data)
	}
	var pr promptAsyncResponse
	if err := json.Unmarshal(data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Status != "started" {
		t.Fatalf("prompt response = %+v, want status=started (session was idle)", pr)
	}
	<-prov.started // the prompt's own worker call is now in flight and blocked

	running := h.getPausedGoalView(id)
	if running.Goal == nil || !running.Goal.Active || !running.Goal.Paused || running.Goal.PauseReason != "worker_failure" {
		t.Fatalf("goal while the prompt turn is in flight = %+v, want unchanged active/paused/worker_failure", running.Goal)
	}
	if running.State != "busy" {
		t.Errorf("state while an ordinary prompt turn is actively streaming = %q, want busy (a forced-idle goal pause must never mask a running turn)", running.State)
	}

	// /session/status must agree — the same compositeStateFor source.
	resp, data = h.do("GET", "/session/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET status %d: %s", resp.StatusCode, data)
	}
	var statuses map[string]struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &statuses); err != nil {
		t.Fatal(err)
	}
	if statuses[id].State != "busy" {
		t.Errorf("/session/status state = %q, want busy: %s", statuses[id].State, data)
	}

	// Teardown: this blocked call belongs to the plain prompt's own turn,
	// not the goal loop, so clearing the goal alone doesn't cancel it —
	// clear first (so the turn's own tail never auto-arms a fresh loop once
	// it ends), then abort to release the blocked stream. Mirrors
	// TestAutoArmAfterRestartResetsPausePresentation's identical teardown
	// shape.
	resp, _ = h.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear goal status %d", resp.StatusCode)
	}
	_, _ = h.do("POST", "/session/"+id+"/abort", nil)
}

// TestWaitReturnsImmediatelyWhenConditionPreHolds covers the immediate-return
// branch for both `until` values: a freshly created session is already idle
// (until=idle holds at once) and has never had a goal (until=goal-done holds
// at once, trivially — there is nothing to wait for).
func TestWaitReturnsImmediatelyWhenConditionPreHolds(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	resp, data := h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("wait until=idle status %d: %s", resp.StatusCode, data)
	}
	var wr waitJSON
	if err := json.Unmarshal(data, &wr); err != nil {
		t.Fatal(err)
	}
	if wr.State != "idle" {
		t.Errorf("wait until=idle state = %q, want idle", wr.State)
	}
	if wr.Goal != nil {
		t.Errorf("wait until=idle goal = %+v, want nil (never set)", wr.Goal)
	}

	resp, data = h.do("GET", "/session/"+id+"/wait?until=goal-done&timeout_s=5", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("wait until=goal-done status %d: %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &wr); err != nil {
		t.Fatal(err)
	}
	if wr.Goal != nil {
		t.Errorf("wait until=goal-done goal = %+v, want nil (never set)", wr.Goal)
	}

	// No waiter left registered after either immediate return.
	h.srv.mu.Lock()
	n := len(h.srv.waiters)
	h.srv.mu.Unlock()
	if n != 0 {
		t.Errorf("waiters registered after immediate return = %d, want 0", n)
	}
}

// TestWaitRejectsBadParams covers the 400 branches: an unknown `until` value
// and a non-positive timeout_s.
func TestWaitRejectsBadParams(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	id := h.createSession("test/m1")

	resp, _ := h.do("GET", "/session/"+id+"/wait?until=bogus", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad until = %d, want 400", resp.StatusCode)
	}
	resp, _ = h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=0", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("timeout_s=0 = %d, want 400", resp.StatusCode)
	}
	resp, _ = h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=-1", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("timeout_s=-1 = %d, want 400", resp.StatusCode)
	}
	resp, _ = h.do("GET", "/session/ses_nope/wait?until=idle", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown session = %d, want 404", resp.StatusCode)
	}
}

// TestParseWaitTimeout is the red-first table test for the int64-overflow
// bug: parseWaitTimeout used to cap the *Duration* (time.Duration(n) *
// time.Second) rather than the integer seconds, so a large enough timeout_s
// overflowed int64 into a negative Duration before the cap comparison ever
// ran — silently defeating the documented "capped at 300s, never rejected
// for being too large" contract with an effectively-zero wait instead.
// 10000000000 (1e10) is exactly such a value: 1e10 * 1e9 = 1e19, which wraps
// past math.MaxInt64 (~9.22e18) into a negative int64.
func TestParseWaitTimeout(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want time.Duration
		err  bool
	}{
		{"empty defaults", "", defaultWaitTimeout, false},
		{"typical value", "5", 5 * time.Second, false},
		{"exactly the cap", "300", maxWaitTimeout, false},
		{"just over the cap", "301", maxWaitTimeout, false},
		{"zero is invalid", "0", 0, true},
		{"negative is invalid", "-1", 0, true},
		{"non-integer is invalid", "soon", 0, true},
		{"overflow value caps rather than going negative", "10000000000", maxWaitTimeout, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseWaitTimeout(tt.raw)
			if tt.err {
				if err == nil {
					t.Fatalf("parseWaitTimeout(%q) = %v, nil, want an error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWaitTimeout(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("parseWaitTimeout(%q) = %v, want %v", tt.raw, got, tt.want)
			}
			if got < 0 {
				t.Errorf("parseWaitTimeout(%q) = %v, want non-negative", tt.raw, got)
			}
		})
	}
}

// TestWaitColdSessionDoesNotLoad verifies the existence check /wait uses is
// the same cheap resident-or-on-disk stat handleAbort and handleGoalDelete
// use (see TestAbortColdSessionIsIdempotent), not the full-deserializing
// s.lookup: a session that exists only on disk (never touched by this
// process) must resolve to a 200 idle response without becoming resident.
func TestWaitColdSessionDoesNotLoad(t *testing.T) {
	dir := t.TempDir()

	// Seed a session on disk only (a prior process wrote its log).
	seedProv := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("hi")}}
	seed := engine.NewSession(engine.Config{
		Providers:  provider.Registry{"test": seedProv},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	})
	if _, err := seed.Prompt(context.Background(), "seed"); err != nil {
		t.Fatal(err)
	}
	id := seed.ID

	h := newHarnessDir(t, dir, &scriptedProvider{name: "test"})

	resp, data := h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=5", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wait on cold session status = %d, want 200: %s", resp.StatusCode, data)
	}
	var wr waitJSON
	if err := json.Unmarshal(data, &wr); err != nil {
		t.Fatal(err)
	}
	if wr.State != "idle" {
		t.Errorf("wait on cold session state = %q, want idle", wr.State)
	}

	// The existence check must not have pulled the session into memory.
	h.srv.mu.Lock()
	_, loaded := h.srv.sessions[id]
	h.srv.mu.Unlock()
	if loaded {
		t.Errorf("wait on a cold session loaded it into memory")
	}
}

// TestWaitUnblocksOnPromptIdle covers the plain-prompt symmetry that
// TestWaitUnblocksOnGoalCompletion and TestWaitUnblocksOnGoalCleared leave
// untested: a wait registered while a PLAIN prompt_async (no goal at all) is
// still in flight must be woken by the same durable-event fanout when that
// prompt completes and the session goes idle — not just when a goal
// terminates. gateProvider's worker branch (tool-bearing Stream call) fires
// for an ordinary prompt too, since every session's default tools (bash and
// friends) make req.Tools non-empty; only the goal evaluator call is
// tool-less. waitForWaiterCount proves the waiter was actually registered
// before release, so the release below races the notification path, not the
// immediate-check fast path. The elapsed-time bound is what actually makes
// this test fail on a broken notify rather than merely surviving to its own
// generous timeout_s=30 and reporting the (by-then genuinely idle) state
// anyway — see the mutation-check note in the commit message.
func TestWaitUnblocksOnPromptIdle(t *testing.T) {
	prov := &gateProvider{
		name:           "test",
		workerStarted:  make(chan struct{}),
		workerReleased: make(chan struct{}),
	}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hello"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST prompt_async status %d: %s", resp.StatusCode, data)
	}
	<-prov.workerStarted // prompt turn in flight; session is busy

	type result struct {
		resp *http.Response
		data []byte
	}
	waitDone := make(chan result, 1)
	start := time.Now()
	go func() {
		resp, data := h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=30", nil)
		waitDone <- result{resp, data}
	}()

	// Block until the wait handler has actually registered its waiter, so the
	// release below races the notification path, not the immediate-check
	// fast path.
	waitForWaiterCount(t, h.srv, 1)

	close(prov.workerReleased) // prompt turn completes -> session goes idle

	res := <-waitDone
	elapsed := time.Since(start)
	if res.resp.StatusCode != 200 {
		t.Fatalf("wait status %d: %s", res.resp.StatusCode, res.data)
	}
	var wr waitJSON
	if err := json.Unmarshal(res.data, &wr); err != nil {
		t.Fatal(err)
	}
	if wr.State != "idle" {
		t.Errorf("wait response state = %q, want idle", wr.State)
	}
	// A working notify resolves this near-instantly; surviving anywhere close
	// to the 30s timeout_s means the wake relied on the timer, not the fanout
	// (waitSnapshot re-derives fresh state regardless of path, so state alone
	// can't tell the two apart — see the mutation-check note in the commit
	// message).
	if elapsed >= 5*time.Second {
		t.Errorf("wait took %v, want well under its 30s timeout (should be woken by notify, not the deadline)", elapsed)
	}

	h.srv.mu.Lock()
	n := len(h.srv.waiters)
	h.srv.mu.Unlock()
	if n != 0 {
		t.Errorf("waiters left registered after completion = %d, want 0", n)
	}
}

// TestWaitUnblocksOnGoalCompletion is the red-first test for DELIVER (2)'s
// central promise: a long-poll registered WHILE the goal is still active is
// woken by the existing durable-event fanout (never server-side polling) the
// moment goal.achieved lands, and its response carries the achieved terminal.
func TestWaitUnblocksOnGoalCompletion(t *testing.T) {
	prov := &gateProvider{
		name:           "test",
		eval:           [][]provider.Event{asstTurn("MET: looks complete")},
		workerStarted:  make(chan struct{}),
		workerReleased: make(chan struct{}),
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	<-prov.workerStarted // worker turn in flight; the goal is active

	type result struct {
		resp *http.Response
		data []byte
	}
	waitDone := make(chan result, 1)
	go func() {
		resp, data := h.do("GET", "/session/"+id+"/wait?until=goal-done&timeout_s=30", nil)
		waitDone <- result{resp, data}
	}()

	// Block until the wait handler has actually registered its waiter, so the
	// release below races the notification path, not the immediate-check
	// fast path — this is what proves the long-poll unblocks via the
	// subscriber/notify mechanism rather than happening to already be done.
	waitForWaiterCount(t, h.srv, 1)

	close(prov.workerReleased) // worker turn completes -> evaluator MET -> goal.achieved

	res := <-waitDone
	if res.resp.StatusCode != 200 {
		t.Fatalf("wait status %d: %s", res.resp.StatusCode, res.data)
	}
	var wr waitJSON
	if err := json.Unmarshal(res.data, &wr); err != nil {
		t.Fatal(err)
	}
	if wr.State == "goal-running" {
		t.Errorf("wait response state = %q, want not goal-running (goal is done)", wr.State)
	}
	if wr.Goal == nil {
		t.Fatal("wait response missing goal")
	}
	if wr.Goal.Active {
		t.Error("wait response goal.active = true, want false (terminal)")
	}
	if !wr.Goal.Achieved {
		t.Error("wait response goal.achieved = false, want true (this terminal was achievement)")
	}

	h.srv.mu.Lock()
	n := len(h.srv.waiters)
	h.srv.mu.Unlock()
	if n != 0 {
		t.Errorf("waiters left registered after completion = %d, want 0", n)
	}
}

// TestWaitUnblocksOnGoalCleared covers the other goal-done terminal: DELETE
// /session/{id}/goal (cleared, not achieved) must also wake a registered
// wait, with goal.achieved false distinguishing it from
// TestWaitUnblocksOnGoalCompletion's achieved terminal.
func TestWaitUnblocksOnGoalCleared(t *testing.T) {
	prov := &goalProv{
		name:        "test",
		blockWorker: true,
		started:     make(chan struct{}),
		eval:        [][]provider.Event{asstTurn("MET: ok")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // goal active, worker blocked indefinitely

	type result struct {
		resp *http.Response
		data []byte
	}
	waitDone := make(chan result, 1)
	go func() {
		resp, data := h.do("GET", "/session/"+id+"/wait?until=goal-done&timeout_s=30", nil)
		waitDone <- result{resp, data}
	}()
	waitForWaiterCount(t, h.srv, 1)

	resp, _ = h.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE goal = %d, want 204", resp.StatusCode)
	}

	res := <-waitDone
	if res.resp.StatusCode != 200 {
		t.Fatalf("wait status %d: %s", res.resp.StatusCode, res.data)
	}
	var wr waitJSON
	if err := json.Unmarshal(res.data, &wr); err != nil {
		t.Fatal(err)
	}
	if wr.State == "goal-running" {
		t.Errorf("wait response state = %q, want not goal-running (goal cleared)", wr.State)
	}
	if wr.Goal == nil {
		t.Fatal("wait response missing goal")
	}
	if wr.Goal.Active {
		t.Error("wait response goal.active = true, want false (cleared is terminal)")
	}
	if wr.Goal.Achieved {
		t.Error("wait response goal.achieved = true, want false (this terminal was a clear, not an achievement)")
	}
}

// TestWaitTimeoutReturnsCleanly is the red-first test for the timeout path:
// a goal that never completes (worker permanently blocked, released only by
// context cancellation) must make GET /wait return cleanly — 200, current
// (unmet) state, no hang, no error — once timeout_s elapses. Run inside a
// synctest bubble on fake time, driving the handler directly (blocking-stream
// pattern from server/goal_test.go): no raw sleeps.
func TestWaitTimeoutReturnsCleanly(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProv{
			name:        "test",
			blockWorker: true,
			started:     make(chan struct{}),
			eval:        [][]provider.Event{asstTurn("MET: ok")},
		}
		srv := newServer(t, dir, prov, 0, func(o *Options) {
			o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
		})
		id := createSessionDirect(t, srv, "test/m1")

		grec := httptest.NewRecorder()
		greq := httptest.NewRequest("POST", "/session/"+id+"/goal", strings.NewReader(`{"condition":"cond"}`))
		greq.SetPathValue("id", id)
		srv.handleGoal(grec, greq)
		if grec.Code != http.StatusAccepted {
			t.Fatalf("goal status %d: %s", grec.Code, grec.Body)
		}
		<-prov.started // worker turn now blocked; the goal never completes on its own

		wrec := httptest.NewRecorder()
		wreq := httptest.NewRequest("GET", "/session/"+id+"/wait?until=goal-done&timeout_s=1", nil)
		wreq.SetPathValue("id", id)
		waitDone := make(chan struct{})
		go func() {
			defer close(waitDone)
			srv.handleWait(wrec, wreq)
		}()

		synctest.Wait() // the wait handler parks on its timer + waiter channel
		select {
		case <-waitDone:
			t.Fatal("wait returned before its 1s timeout")
		default:
		}

		// Every goroutine is durably blocked (goal worker on ctx.Done/release,
		// wait handler on its timer/waiter channel): fake time advances to the
		// 1s deadline and the wait handler's timer fires.
		<-waitDone

		if wrec.Code != http.StatusOK {
			t.Fatalf("wait timeout status = %d, want 200: %s", wrec.Code, wrec.Body)
		}
		var wr waitJSON
		if err := json.Unmarshal(wrec.Body.Bytes(), &wr); err != nil {
			t.Fatal(err)
		}
		if wr.State != "goal-running" {
			t.Errorf("wait timeout state = %q, want goal-running (still active)", wr.State)
		}
		if wr.Goal == nil || !wr.Goal.Active {
			t.Errorf("wait timeout goal = %+v, want active", wr.Goal)
		}

		// No waiter leak after the timeout return.
		srv.mu.Lock()
		n := len(srv.waiters)
		srv.mu.Unlock()
		if n != 0 {
			t.Errorf("waiters left registered after timeout = %d, want 0", n)
		}

		// Unwind the still-blocked goal loop goroutine before the bubble ends
		// (see AGENTS.md: a goroutine parked at bubble end is a leak) —
		// cancel its context and drain the WaitGroup, both pure channel/sync
		// operations, no sleeps.
		srv.mu.Lock()
		st := srv.sessions[id]
		var cancel context.CancelFunc
		if st != nil {
			cancel = st.cancel
		}
		srv.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		srv.Drain(context.Background())
	})
}

// TestWaitDisconnectDoesNotLeakWaiter proves the waiter registry is cleaned
// up even when the client goes away before the condition holds or the
// timeout fires: cancelling the request context must unregister the waiter
// promptly, with no server-side polling to notice the disconnect.
func TestWaitDisconnectDoesNotLeakWaiter(t *testing.T) {
	prov := &goalProv{
		name:        "test",
		blockWorker: true,
		started:     make(chan struct{}),
		eval:        [][]provider.Event{asstTurn("MET: ok")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")
	t.Cleanup(func() {
		h.srv.mu.Lock()
		st := h.srv.sessions[id]
		var cancel context.CancelFunc
		if st != nil {
			cancel = st.cancel
		}
		h.srv.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	})

	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // goal active, never completes on its own: the wait below must block

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", h.ts.URL+"/session/"+id+"/wait?until=goal-done&timeout_s=300", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	respCh := make(chan error, 1)
	go func() {
		resp, err := h.ts.Client().Do(req)
		if err == nil {
			resp.Body.Close()
		}
		respCh <- err
	}()

	waitForWaiterCount(t, h.srv, 1) // the long-poll is registered and blocked

	cancel() // client disconnects
	if err := <-respCh; err == nil {
		t.Fatal("client request returned nil error after ctx cancellation, want a cancellation error")
	}

	// The server must notice the disconnect and unregister the waiter without
	// any server-side polling driving that cleanup — poll only the test's own
	// observation of server state (same no-sleep pattern as elsewhere here).
	waitForWaiterCount(t, h.srv, 0)

	resp, _ = h.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE goal = %d, want 204", resp.StatusCode)
	}
}

// TestWaitUntilIdleReturnsImmediatelyForBootResumedQueue is the regression
// guard for a live review finding on the fix in
// TestWaitUntilIdleDoesNotWakeEarlyOnQueuedFollowUp (queue_test.go): gating
// until=idle naively on "is the queue non-empty" is wrong for a session
// resumed after a restart with a prompt still durably queued and nothing
// running — AGENTS.md: "Boot never auto-dispatches a resumed queue... it
// sits there until the next natural drain trigger (an idle prompt, the next
// tool-call boundary inside a running turn, or a goal loop's next turn
// boundary)." That session has no pending drain trigger at all and is
// genuinely idle right now; the earlier, rejected version of this fix made
// GET /session/{id}/wait?until=idle block for the full timeout instead of
// returning immediately.
//
// Reproduces a genuine restart, the same technique
// TestRestartRecoveryEngagesWithoutTouchingCrashedChildDirectly uses: h1's
// occupying turn is left blocked forever (simulating "kill -9 1" — never
// released, never closed) with a second prompt durably queued behind it,
// then h2 is a fully independent server built over the same on-disk
// session dir (simulating the restart). h2 never runs a turn for this
// session at all, so if the fix were wrong, nothing would ever resolve the
// false-busy reading — the wait would block for its full timeout and still
// come back wrong, not just late.
func TestWaitUntilIdleReturnsImmediatelyForBootResumedQueue(t *testing.T) {
	dir := t.TempDir()

	// newHarnessDir (not multiProviderHarnessInDir): its NewSession/LoadSession
	// closures wire engine.Config.OnEvent to srv.Publish (see newServer,
	// server_test.go) — required for engine/queue.go's own
	// EventPromptQueued to ever reach the durable server-level journal this
	// test depends on. multiProviderHarnessInDir's closures deliberately
	// leave OnEvent unwired (fine for the lineage/SessionManager tests it
	// otherwise serves) and so never journal prompt.queued at all.
	prov1 := newBlockingProvider("test")
	h1 := newHarnessDir(t, dir, prov1)
	id := h1.createSession("test/m1")

	resp, data := h1.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov1.started // "first" is now genuinely mid-turn, blocked — never released below

	resp, data = h1.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "second"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second prompt status %d: %s", resp.StatusCode, data)
	}
	var qr promptAsyncResponse
	if err := json.Unmarshal(data, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "queued" || qr.Queued != 1 {
		t.Fatalf("second prompt response = %+v, want status=queued queued=1", qr)
	}

	// Simulate "kill -9 1": h1 is abandoned here, never closed cleanly,
	// "first" never released. h2 is a fully independent server (its own
	// SessionManager, its own provider instance) sharing only the on-disk
	// session dir — this session's own queueDrainPending never existed in
	// h2's process; loadJournal never sets it (see Server.queueDrainPending's
	// own doc comment).
	prov2 := newBlockingProvider("test")
	h2 := newHarnessDir(t, dir, prov2)

	// Direct, deterministic assertion on the exact mechanism at stake —
	// the same technique TestWaitUntilIdleDoesNotWakeEarlyOnQueuedFollowUp
	// uses — rather than inferring correctness from HTTP response latency,
	// which synctest's fake-time fast-forwarding would make indistinguishable
	// from a real immediate return anyway.
	if state, _ := h2.srv.waitSnapshot(id); state != "idle" {
		t.Fatalf("waitSnapshot after restart = %q, want idle (a resumed queue has no pending drain trigger)", state)
	}

	resp, data = h2.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wait until=idle status %d: %s", resp.StatusCode, data)
	}
	var wr waitJSON
	if err := json.Unmarshal(data, &wr); err != nil {
		t.Fatal(err)
	}
	if wr.State != "idle" {
		t.Errorf("wait until=idle state = %q, want idle", wr.State)
	}

	// GET /session/{id}'s own State field is deliberately untouched by this
	// whole fix (idle-with-queue stays "idle" there too, matching
	// TestIdlePromptWithQueueGoesFIFO) — reconfirm both endpoints agree and
	// the queue itself is still genuinely non-empty (nothing silently
	// dropped it).
	sess := h2.getSessionJSON(id)
	if sess.State != "idle" || sess.Queued != 1 {
		t.Errorf("GET session after restart = state=%q queued=%d, want state=idle queued=1", sess.State, sess.Queued)
	}
}
