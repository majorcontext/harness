package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/majorcontext/harness/provider"
)

// TestTurnEndOnGoalMaxTurnsIsNotCompleted is the red-first regression test
// for PR #55 review finding (1): runGoal used to key turn.end purely on
// PursueGoal's error being nil, but PursueGoal returns a NIL error with
// Achieved:false when MaxTurns is exhausted (engine/goal.go's terminal
// "return &GoalResult{Achieved: false, ..., Reason: "max turns"}, nil") —
// so a goal that gave up after burning its turn budget was journaled as
// turn.end{outcome:"completed"} and surfaced as last_turn={completed},
// telling a poller "idle because done" for a goal that was never met. That
// is exactly the ambiguity this primitive exists to remove.
//
// This must record a distinct, non-"completed" outcome instead.
func TestTurnEndOnGoalMaxTurnsIsNotCompleted(t *testing.T) {
	prov := &goalProv{
		name:   "test",
		worker: [][]provider.Event{asstTurn("try 1"), asstTurn("try 2")},
		eval:   [][]provider.Event{asstTurn("NOT MET: nope"), asstTurn("NOT MET: still nope")},
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "impossible", "max_turns": 2})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}

	evs := sse.collectUntilIdle(t)
	var end *Event
	for i := range evs {
		if evs[i].Type == "turn.end" {
			end = &evs[i]
		}
	}
	if end == nil {
		t.Fatalf("no turn.end record observed for a max-turns-exhausted goal: %v", evs)
	}
	if end.Outcome == "completed" {
		t.Fatalf("turn.end outcome = %q, want anything but \"completed\" — the goal was never achieved (max turns exhausted)", end.Outcome)
	}
	if end.Outcome == "" {
		t.Fatalf("turn.end outcome is empty")
	}

	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var sess struct {
		LastTurn *lastTurnJSONForTest `json:"last_turn"`
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.LastTurn == nil {
		t.Fatalf("session JSON missing last_turn: %s", data)
	}
	if sess.LastTurn.Outcome == "completed" {
		t.Errorf("last_turn.outcome = %q, want anything but completed", sess.LastTurn.Outcome)
	}
}

// clearRaceProv drives one worker turn to completion normally, then blocks
// the (tool-less) evaluator call until the test releases it — giving the
// test a deterministic window to clear the goal directly (bypassing DELETE
// /goal, which also cancels the loop's context) so PursueGoal takes the
// "cleared while an evaluation was in flight" path: evaluateGoal succeeds,
// but recordGoalEval reports the goal is no longer active, and PursueGoal
// returns (Achieved:false, Reason:"goal cleared", nil) — a nil error, just
// like the max-turns case, but this one must be a clean-stop suppression of
// turn.end entirely, matching the documented "DELETE goal does not emit
// turn.end" contract.
type clearRaceProv struct {
	name string

	mu          sync.Mutex
	workerCalls int

	evalStarted chan struct{}
	evalRelease chan struct{}
	startedOnce sync.Once
}

func (p *clearRaceProv) Name() string { return p.name }

func (p *clearRaceProv) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	if len(req.Tools) == 0 { // evaluator: tool-less by construction
		return &clearRaceEvalStream{p: p}, nil
	}
	p.mu.Lock()
	p.workerCalls++
	p.mu.Unlock()
	return &scriptedStream{events: asstTurn("did the work")}, nil
}

type clearRaceEvalStream struct {
	p      *clearRaceProv
	events []provider.Event
	i      int
}

func (s *clearRaceEvalStream) Next() (provider.Event, error) {
	if s.events == nil {
		s.p.startedOnce.Do(func() { close(s.p.evalStarted) })
		<-s.p.evalRelease
		s.events = asstTurn("MET: sure, looks done")
	}
	if s.i >= len(s.events) {
		return provider.Event{}, io.EOF
	}
	ev := s.events[s.i]
	s.i++
	return ev, nil
}

func (s *clearRaceEvalStream) Close() error { return nil }

// TestTurnEndSuppressedOnGoalClearedInFlight is the red-first regression
// test for the second half of review finding (1): when ClearGoal wins a
// race against an in-flight evaluator call, PursueGoal returns a nil error
// with Achieved:false, Reason:"goal cleared" — indistinguishable, by error
// value alone, from the max-turns case, but semantically a cancellation:
// the goal was cleared, not completed and not exhausted. No turn.end record
// must be emitted at all for this path — DELETE /goal's documented contract
// ("does not emit turn.end") must hold even when the clear happens to race
// an in-flight evaluation rather than going through the context-cancel path.
func TestTurnEndSuppressedOnGoalClearedInFlight(t *testing.T) {
	prov := &clearRaceProv{
		name:        "test",
		evalStarted: make(chan struct{}),
		evalRelease: make(chan struct{}),
	}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")

	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "cond"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
	}

	<-prov.evalStarted // the evaluator call is now blocked in flight

	// Clear the goal directly — NOT via DELETE /goal, which would also
	// cancel the loop's context and take the (already-correct)
	// context.Canceled path. This reaches the OTHER nil-error path.
	h.srv.mu.Lock()
	st := h.srv.sessions[id]
	h.srv.mu.Unlock()
	if st == nil {
		t.Fatalf("session %s not resident", id)
	}
	st.sess.ClearGoal()

	close(prov.evalRelease) // let the blocked evaluator call return "MET"

	evs := sse.collectUntilIdle(t)
	var sawCleared, sawTurnEnd bool
	for _, ev := range evs {
		switch ev.Type {
		case "goal.cleared":
			sawCleared = true
		case "turn.end":
			sawTurnEnd = true
		}
	}
	if !sawCleared {
		t.Fatalf("goal.cleared not observed: %v", evs)
	}
	if sawTurnEnd {
		t.Errorf("turn.end was emitted for a goal cleared in flight, want none (matches the documented DELETE-does-not-emit-turn.end contract): %v", evs)
	}

	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var sess struct {
		LastTurn *lastTurnJSONForTest `json:"last_turn"`
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.LastTurn != nil {
		t.Errorf("last_turn = %+v, want nil (no turn.end for a goal cleared in flight)", sess.LastTurn)
	}
}
