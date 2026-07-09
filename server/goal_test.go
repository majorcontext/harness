package server

import (
	"context"
	"encoding/json"
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
		case "goal.set", "goal.eval", "goal.achieved", "goal.cleared":
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
// instead of relying on luck: it forces the worst-case interleaving where the
// goal-loop worker's context-cancellation unwind (which ends in the terminal
// session.status idle record) gets unbounded time to race ahead of whatever
// the DELETE handler does next, via a hook installed between the handler's
// two operations (cancel and clear, in whichever order they run).
//
// If cancellation happens before the clear is journaled, the worker is free
// to run all the way to "idle" while the handler is paused — reproducing
// exactly the symptom this test guards against: an SSE collector that reads
// until session.status idle would see goal.set but never goal.cleared,
// because goal.cleared was journaled after the idle record it should have
// preceded. The guarantee under test (see engine.Session.ClearGoal and
// handleGoalDelete): goal.cleared is always journaled before the
// session.status idle that ends that goal's occupancy.
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

	// Installed at the seam between handleGoalDelete's cancel and clear
	// operations (see the seam comment there). cancelledAlready reports,
	// deterministically (the handler runs the two operations sequentially
	// on one goroutine), whether cancel() has already been invoked at this
	// point. When it has, the worker goroutine is now free to race forward
	// to the terminal idle record — so this gives it unbounded time (poll,
	// no sleep, no arbitrary timeout) to actually get there before letting
	// the handler continue, forcing the CI flake's worst case every run
	// instead of leaving it to luck. When cancel has not run yet (the fixed
	// ordering: clear happens first), there is nothing to race against yet,
	// so this returns immediately.
	h.srv.goalDeleteRace = func(cancelledAlready bool) {
		if !cancelledAlready {
			return
		}
		for {
			h.srv.mu.Lock()
			idle := false
			for _, ev := range h.srv.journal {
				if ev.SessionID == id && ev.Type == evtSessionStatus && ev.Status == "idle" {
					idle = true
					break
				}
			}
			h.srv.mu.Unlock()
			if idle {
				return
			}
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
