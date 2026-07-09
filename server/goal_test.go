package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// goalProv serves both the worker model and the tool-less evaluator model from
// one registry entry, keying the two apart by the presence of tools. When
// blockWorker is set, the worker's first turn blocks until the prompt context
// is cancelled (for busy/abort/DELETE tests); the evaluator side stays scripted.
type goalProv struct {
	name        string
	mu          sync.Mutex
	worker      [][]provider.Event
	eval        [][]provider.Event
	wi, ei      int
	blockWorker bool
	started     chan struct{}
	startedOnce sync.Once

	// workerErrN, when > 0, makes the first workerErrN worker (tool-bearing)
	// Stream calls fail with a fake transient error instead of consuming a
	// scripted turn — exercises PursueGoal's retry path (see
	// engine/goal.go's goalProvider, same idea) deterministically.
	workerErrN   int
	workerErrHit int
}

func (p *goalProv) Name() string { return p.name }

func (p *goalProv) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
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
	if p.blockWorker {
		return &goalBlockingStream{ctx: ctx, p: p}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.workerErrHit < p.workerErrN {
		p.workerErrHit++
		return nil, errors.New("fake transient provider error")
	}
	if p.wi >= len(p.worker) {
		return &scriptedStream{}, nil
	}
	ev := p.worker[p.wi]
	p.wi++
	return &scriptedStream{events: ev}, nil
}

type goalBlockingStream struct {
	ctx context.Context
	p   *goalProv
}

func (s *goalBlockingStream) Next() (provider.Event, error) {
	s.p.startedOnce.Do(func() { close(s.p.started) })
	<-s.ctx.Done()
	return provider.Event{}, s.ctx.Err()
}

func (s *goalBlockingStream) Close() error { return nil }

func newGoalHarness(t *testing.T, prov provider.Provider) *harness {
	t.Helper()
	const token = "secret-run-token"
	dir := t.TempDir()
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: token, srv: srv, ts: ts}
}

func goalEvents(evs []Event) []Event {
	var out []Event
	for _, ev := range evs {
		switch ev.Type {
		case "goal.set", "goal.eval", "goal.stalled", "goal.achieved", "goal.cleared":
			out = append(out, ev)
		}
	}
	return out
}

func TestGoalAchievedJournaled(t *testing.T) {
	prov := &goalProv{
		name:   "test",
		worker: [][]provider.Event{asstTurn("working"), asstTurn("done")},
		eval:   [][]provider.Event{asstTurn("NOT MET: needs a summary"), asstTurn("MET: summary present")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "write a summary"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}
	evs := sse.collectUntilIdle(t)

	got := goalEvents(evs)
	var types []string
	for _, ev := range got {
		if ev.Seq == 0 {
			t.Errorf("goal event %s has no seq (must be durable)", ev.Type)
		}
		types = append(types, ev.Type)
	}
	want := []string{"goal.set", "goal.eval", "goal.eval", "goal.achieved"}
	if len(types) != 4 || types[0] != want[0] || types[1] != want[1] || types[2] != want[2] || types[3] != want[3] {
		t.Fatalf("goal events = %v, want %v", types, want)
	}
	if got[0].GoalCondition != "write a summary" {
		t.Errorf("goal.set condition = %q", got[0].GoalCondition)
	}
	if got[1].GoalMet || got[1].GoalReason == "" || got[1].GoalTurn != 1 {
		t.Errorf("first goal.eval = %+v, want not met, reason, turn 1", got[1])
	}
	if !got[2].GoalMet || got[2].GoalTurn != 2 {
		t.Errorf("second goal.eval = %+v, want met, turn 2", got[2])
	}
	if !got[3].GoalMet && got[3].GoalReason == "" {
		t.Errorf("goal.achieved = %+v", got[3])
	}

	// Session JSON carries the goal summary (this process).
	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET session status %d", resp.StatusCode)
	}
	var sess struct {
		Goal *struct {
			Condition  string `json:"condition"`
			Active     bool   `json:"active"`
			Achieved   bool   `json:"achieved"`
			Turns      int    `json:"turns"`
			LastReason string `json:"last_reason"`
		} `json:"goal"`
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Goal == nil {
		t.Fatalf("session JSON missing goal: %s", data)
	}
	if sess.Goal.Condition != "write a summary" || sess.Goal.Active || sess.Goal.Turns != 2 {
		t.Errorf("goal = %+v, want condition set, inactive, turns 2", *sess.Goal)
	}
	// achieved must distinguish a completed goal from a cleared one so a
	// client bootstrapping from this record renders the chip correctly.
	if !sess.Goal.Achieved {
		t.Errorf("goal.achieved = false, want true for a completed goal")
	}
}

// TestGoalStalledJournaledAndActive scripts one transient worker-turn
// failure (retried, then succeeding) and asserts the wire contract the
// review finding says was missing: a durable goal.stalled record (non-zero
// seq) carrying the retry attempt number reaches the SSE stream, and the
// goal remains active throughout — goal.stalled is non-terminal, so Session
// JSON must still report active:true (and achieved:false) right after it,
// only flipping once the retried turn is actually evaluated MET.
func TestGoalStalledJournaledAndActive(t *testing.T) {
	prov := &goalProv{
		name:       "test",
		workerErrN: 1, // first attempt fails, second (retried) attempt succeeds
		worker:     [][]provider.Event{asstTurn("done")},
		eval:       [][]provider.Event{asstTurn("MET: looks complete")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}

	stalled := sse.waitFor(t, "goal.stalled")
	if stalled.Seq == 0 {
		t.Error("goal.stalled event has no seq (must be durable)")
	}
	if stalled.GoalAttempt != 1 {
		t.Errorf("goal.stalled GoalAttempt = %d, want 1", stalled.GoalAttempt)
	}
	if stalled.GoalReason == "" {
		t.Error("goal.stalled event missing GoalReason")
	}

	// The journal must carry the same durable record (not just the live
	// fanout) — read it right away, before the retried turn can complete.
	h.srv.mu.Lock()
	var journaled *Event
	for i := range h.srv.journal {
		ev := h.srv.journal[i]
		if ev.SessionID == id && ev.Type == "goal.stalled" {
			journaled = &ev
			break
		}
	}
	h.srv.mu.Unlock()
	if journaled == nil {
		t.Fatal("goal.stalled not found in the server journal")
	}
	if journaled.Seq == 0 {
		t.Error("journaled goal.stalled has no seq")
	}

	// A stall is non-terminal: the goal must still be active (and not yet
	// achieved) right after it, per the state machine in goal.go — read
	// Session JSON now, before the retry's turn has a chance to finish.
	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET session status %d", resp.StatusCode)
	}
	var sess struct {
		Goal *struct {
			Active   bool `json:"active"`
			Achieved bool `json:"achieved"`
			Attempt  int  `json:"attempt"`
		} `json:"goal"`
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Goal == nil {
		t.Fatalf("session JSON missing goal: %s", data)
	}
	if !sess.Goal.Active || sess.Goal.Achieved {
		t.Errorf("goal = %+v right after goal.stalled, want active:true achieved:false (a stall is non-terminal)", *sess.Goal)
	}
	if sess.Goal.Attempt != 1 {
		t.Errorf("goal.attempt = %d right after goal.stalled, want 1", sess.Goal.Attempt)
	}

	// The retried turn goes on to be evaluated MET, achieving the goal.
	evs := sse.collectUntilIdle(t)
	var achieved bool
	for _, ev := range goalEvents(evs) {
		if ev.Type == "goal.achieved" {
			achieved = true
		}
	}
	if !achieved {
		t.Fatalf("goal events after the stall = %v, want a goal.achieved", goalEvents(evs))
	}
}

func TestGoalBusyRejectsPromptAndGoal(t *testing.T) {
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
	<-prov.started // worker turn is now in flight; the goal occupies the session

	resp, _ = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi"}},
	})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("prompt_async during goal = %d, want 409", resp.StatusCode)
	}
	resp, _ = h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "again"})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second goal during goal = %d, want 409", resp.StatusCode)
	}

	// Release the goal loop so the goroutine unwinds before the test ends.
	resp, _ = h.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE goal = %d, want 204", resp.StatusCode)
	}
}

func TestGoalDeleteClearsAndStops(t *testing.T) {
	prov := &goalProv{
		name:        "test",
		blockWorker: true,
		started:     make(chan struct{}),
		eval:        [][]provider.Event{asstTurn("MET: ok")},
	}
	h := newGoalHarness(t, prov)
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

	evs := sse.collectUntilIdle(t)
	got := goalEvents(evs)
	var sawSet, sawCleared, sawAchieved bool
	for _, ev := range got {
		switch ev.Type {
		case "goal.set":
			sawSet = true
		case "goal.cleared":
			sawCleared = true
			if ev.Seq == 0 {
				t.Error("goal.cleared missing seq")
			}
		case "goal.achieved":
			sawAchieved = true
		}
	}
	if !sawSet || !sawCleared {
		t.Errorf("goal events = %v, want goal.set and goal.cleared", got)
	}
	if sawAchieved {
		t.Error("goal.achieved present, want the goal cleared before achievement")
	}

	// The worker only ran once (blocked, then cancelled): the loop stopped
	// turning, so the evaluator was never consulted.
	prov.mu.Lock()
	ei := prov.ei
	prov.mu.Unlock()
	if ei != 0 {
		t.Errorf("evaluator consulted %d times after DELETE, want 0", ei)
	}
}

// TestGoalDeleteClearBeforeIdleRace reproduces the CI flake deterministically
// instead of relying on luck: it forces the worst-case interleaving, on every
// run, unconditionally — not gated behind any observed ordering — via a hook
// installed right after handleGoalDelete's own ClearGoal call and right
// before its own cancel call (see the seam comment there).
//
// The hook is handed the loop's cancel func directly and does two things,
// every time, no branching: (1) fires cancel itself immediately — the
// earliest structurally possible point, before the handler's own (idempotent,
// now redundant) call to it — which frees the goal-loop worker to unwind, and
// (2) rides that unwind out to completion, polling (no sleep, no arbitrary
// timeout) until the terminal session.status idle record actually lands in
// the journal, before returning and letting the handler finish. That gives
// the worker unbounded time to race all the way to "idle" while the clear
// step is the *only* thing that has run so far — the exact worst case the
// historical bug hit: if clearing happened after cancelling, the worker could
// reach idle first, and an SSE collector that reads until idle (the wire
// contract every client relies on) would see goal.set but never goal.cleared.
//
// Because the hook fires only after ClearGoal has already returned (program
// order within handleGoalDelete, not a race), goal.cleared is always already
// durable at the moment the hook forces the worker's unwind to completion —
// that is the structural guarantee under test (see engine.Session.ClearGoal
// and handleGoalDelete). The hook asserts it directly, immediately, with no
// polling: this is the part that catches a regression to the old
// cancel-then-clear order, where the equivalent seam would fire before
// ClearGoal has run.
//
// Verified against the pre-fix ordering: temporarily swapping handleGoalDelete
// back to cancel-then-clear (keeping this hook fixed in its source position,
// between the two operations, exactly as the historical code's fix relocated
// it) makes this test fail reliably — see the commit message for the red/green
// transcript. Restoring clear-then-cancel makes it pass reliably.
func TestGoalDeleteClearBeforeIdleRace(t *testing.T) {
	prov := &goalProv{
		name:        "test",
		blockWorker: true,
		started:     make(chan struct{}),
		eval:        [][]provider.Event{asstTurn("MET: ok")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, _ := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d", resp.StatusCode)
	}
	<-prov.started

	journalHasClearedLocked := func() bool {
		for _, ev := range h.srv.journal {
			if ev.SessionID == id && ev.Type == "goal.cleared" {
				return true
			}
		}
		return false
	}
	journalHasIdle := func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		for _, ev := range h.srv.journal {
			if ev.SessionID == id && ev.Type == evtSessionStatus && ev.Status == "idle" {
				return true
			}
		}
		return false
	}

	h.srv.goalDeleteRace = func(cancel context.CancelFunc) {
		// The structural invariant, checked directly rather than inferred:
		// by the time this seam fires (after handleGoalDelete's ClearGoal
		// call, in program order), goal.cleared must already be durable.
		// t.Errorf is goroutine-safe (unlike Fatalf/FailNow) — this hook runs
		// on the HTTP handler's goroutine, not the test goroutine.
		h.srv.mu.Lock()
		cleared := journalHasClearedLocked()
		h.srv.mu.Unlock()
		if !cleared {
			t.Errorf("goal.cleared not yet journaled when handleGoalDelete reaches its cancel step — clear must happen before cancel, never after")
		}

		// Now force the historical race's worst case unconditionally: fire
		// the worker's unblock as early as possible and give it unbounded
		// time (poll, no sleep) to race all the way to the terminal idle
		// record before this handler is allowed to proceed any further.
		if cancel != nil {
			cancel()
		}
		for !journalHasIdle() {
			runtime.Gosched()
		}
	}

	resp, _ = h.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE goal = %d, want 204", resp.StatusCode)
	}

	evs := sse.collectUntilIdle(t)
	got := goalEvents(evs)
	var sawCleared bool
	for _, ev := range got {
		if ev.Type == "goal.cleared" {
			sawCleared = true
		}
	}
	if !sawCleared {
		t.Fatalf("goal.cleared not observed before the terminal idle record (goal events = %v) — cleared must be journaled before idle, never after", got)
	}
}

func TestGoalDeleteUnknownAndIdempotent(t *testing.T) {
	prov := &goalProv{name: "test"}
	h := newGoalHarness(t, prov)

	resp, _ := h.do("DELETE", "/session/ses_nope/goal", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE unknown goal = %d, want 404", resp.StatusCode)
	}

	id := h.createSession("test/m1")
	resp, _ = h.do("DELETE", "/session/"+id+"/goal", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE with no active goal = %d, want 204", resp.StatusCode)
	}
}

func TestGoalRequiresConfiguredEvaluator(t *testing.T) {
	// A harness without a configured evaluator model must reject goals clearly.
	prov := &scriptedProvider{name: "test"}
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST goal without evaluator = %d, want 400: %s", resp.StatusCode, data)
	}
}

func TestGoalMissingCondition(t *testing.T) {
	prov := &goalProv{name: "test"}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")
	resp, _ := h.do("POST", "/session/"+id+"/goal", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST goal with empty condition = %d, want 400", resp.StatusCode)
	}
}
