// Tests for Task 2 of the goal-evaluator resilience work (Round 6): the
// server-side outcome mapping, journal folds, and Session JSON surfacing of
// engine/goal.go's advisory goal.eval_failed boundaries and its bounded
// evaluator_exhausted terminal. See docs/plans/2026-07-20-goal-eval-
// resilience.md's "Invariants" list — invariants 8 and 9 are this file's.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// evalFailuresView decodes the Session JSON fields these tests need,
// including eval_failures — mirrors goal_paused_test.go's pausedGoalView.
type evalFailuresView struct {
	State string `json:"state"`
	Goal  *struct {
		Active       bool   `json:"active"`
		Achieved     bool   `json:"achieved"`
		Condition    string `json:"condition"`
		EvalFailures int    `json:"eval_failures"`
	} `json:"goal"`
}

// getEvalFailuresViewDirect drives handleGet in-process (no httptest.Server,
// whose real listener a synctest bubble forbids) and decodes the eval-failures
// Session view — the direct-handler counterpart to the terminal test's inline
// GET, shared by both synctest-converted tests below.
func getEvalFailuresViewDirect(t *testing.T, srv *Server, id string) evalFailuresView {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/session/"+id, nil)
	req.SetPathValue("id", id)
	srv.handleGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET session status %d: %s", rec.Code, rec.Body)
	}
	var v evalFailuresView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestGoalEvaluatorExhaustedTerminalOutcome is invariant 8's terminal half:
// goalEvalFailureLimit (5) consecutive failed evaluator boundaries must map
// to turn.end outcome "evaluator_exhausted" (not the generic "error"),
// preceded by goal.cleared (carrying the dedicated reason) and followed by
// session.error and then the terminal session.status idle record — the exact
// order runGoal's doc comment promises (goal.cleared always precedes idle;
// this test additionally pins session.error and turn.end between the two).
//
// Every evaluator failure is deterministic (a plain, non-retryable error) so
// the run is fully scripted, but engine/goal.go still waits a real backoff
// between boundaries 1-4 (goalRetryDelay: 1s, 4s, 16s, 30s — over 50s of
// real wall-clock time) before the 5th failure reaches the terminal. Per
// AGENTS.md's synctest rule for timer-dependent logic, this runs on fake
// time inside a synctest bubble, driving handleGoal directly with an
// httptest.ResponseRecorder (no real listener — real network I/O does not
// work in a bubble) exactly like TestWaitTimeoutReturnsCleanly does.
//
// Red-verified: with turnEndOutcome's engine.IsGoalEvaluatorExhausted check
// removed, this test fails — turn.end outcome reads "error" instead of
// "evaluator_exhausted" (see the commit message for the transcript).
func TestGoalEvaluatorExhaustedTerminalOutcome(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		worker := make([][]provider.Event, goalEvalFailureLimitForTest)
		for i := range worker {
			worker[i] = asstTurn(fmt.Sprintf("turn %d", i+1))
		}
		prov := &goalProv{
			name:     "test",
			worker:   worker,
			evalErrN: goalEvalFailureLimitForTest,
			evalErr:  errors.New("evaluator down"),
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

		srv.wg.Wait() // runGoal's defer s.wg.Done() fires once the loop (and its backoff waits) finish

		srv.mu.Lock()
		var evs []Event
		for _, ev := range srv.journal {
			if ev.SessionID == id {
				evs = append(evs, ev)
			}
		}
		srv.mu.Unlock()

		idx := map[string]int{}
		var failedCount int
		var clearedReason, turnEndOutcomeGot, turnEndError string
		for i, ev := range evs {
			switch ev.Type {
			case "goal.cleared":
				if _, ok := idx["goal.cleared"]; !ok {
					idx["goal.cleared"] = i
					clearedReason = ev.GoalReason
				}
			case evtSessionError:
				if _, ok := idx[evtSessionError]; !ok {
					idx[evtSessionError] = i
				}
			case evtTurnEnd:
				if _, ok := idx[evtTurnEnd]; !ok {
					idx[evtTurnEnd] = i
					turnEndOutcomeGot = ev.Outcome
					turnEndError = ev.Error
				}
			case evtSessionStatus:
				if ev.Status == "idle" {
					if _, ok := idx["idle"]; !ok {
						idx["idle"] = i
					}
				}
			case "goal.eval_failed":
				failedCount++
			}
		}
		for _, want := range []string{"goal.cleared", evtSessionError, evtTurnEnd, "idle"} {
			if _, ok := idx[want]; !ok {
				t.Fatalf("missing expected event %q in journal: %+v", want, evs)
			}
		}
		if !(idx["goal.cleared"] < idx[evtSessionError] && idx[evtSessionError] < idx[evtTurnEnd] && idx[evtTurnEnd] < idx["idle"]) {
			t.Errorf("event order = goal.cleared:%d session.error:%d turn.end:%d idle:%d, want strictly increasing",
				idx["goal.cleared"], idx[evtSessionError], idx[evtTurnEnd], idx["idle"])
		}

		wantReason := fmt.Sprintf("goal evaluator failed at %d consecutive turn boundaries", goalEvalFailureLimitForTest)
		if clearedReason != wantReason {
			t.Errorf("goal.cleared reason = %q, want %q", clearedReason, wantReason)
		}
		if turnEndOutcomeGot != outcomeEvaluatorExhausted {
			t.Errorf("turn.end outcome = %q, want %q", turnEndOutcomeGot, outcomeEvaluatorExhausted)
		}
		if turnEndError == "" {
			t.Error("turn.end missing sanitized error detail")
		}
		if failedCount != goalEvalFailureLimitForTest {
			t.Errorf("goal.eval_failed events = %d, want %d", failedCount, goalEvalFailureLimitForTest)
		}

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/session/"+id, nil)
		req.SetPathValue("id", id)
		srv.handleGet(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET session status %d: %s", rec.Code, rec.Body)
		}
		var sess evalFailuresView
		if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
			t.Fatal(err)
		}
		if sess.Goal == nil || sess.Goal.Active {
			t.Errorf("goal after the terminal = %+v, want inactive (cleared)", sess.Goal)
		}
		if sess.Goal != nil && sess.Goal.EvalFailures != 0 {
			t.Errorf("eval_failures after the terminal clear = %d, want 0 (reset by goal.cleared)", sess.Goal.EvalFailures)
		}
		if sess.State != "idle" {
			t.Errorf("state after the terminal = %q, want idle", sess.State)
		}
	})
}

// goalEvalFailureLimitForTest mirrors engine's unexported goalEvalFailureLimit
// (5): this package cannot reference it directly, and re-deriving it from a
// magic number in each test would risk silent drift if the engine constant
// ever changes. Bumping the engine constant without updating this one would
// only under- or over-run the scripted worker/eval slices here, which fails
// loudly (goalProv's scriptedStream falls back to an empty turn once its
// slice is exhausted, immediately producing an unparseable/empty verdict) —
// never silently passes for the wrong reason.
const goalEvalFailureLimitForTest = 5

// TestGoalEvalFailedAdvisoryDuringRunNoSessionErrorOrTurnEnd is invariant 8's
// sub-terminal half, plus invariant 9's "rises" half: a single failed
// evaluator boundary — well below goalEvalFailureLimit — must be advisory
// only. No session.error, no turn.end while the loop is still running, and
// Session.goal stays active (state goal-running) with eval_failures counting
// the streak; the very next boundary parses a verdict (MET), which resets
// eval_failures to 0 and lets the loop finish normally.
//
// Only one failed boundary is scripted, so the loop pays one backoff wait
// (goalRetryDelay(1) == 1s) between the failed and the retried, MET boundary.
// Per AGENTS.md's synctest rule for timer-dependent logic, this runs on fake
// time inside a synctest bubble — driving handleGoal directly with an
// httptest.ResponseRecorder and reading the journal instead of an SSE stream
// (no real listener; real network I/O does not work in a bubble) exactly like
// the terminal test above — so the backoff costs no real wall-clock time. The
// mid-run checkpoint is made deterministic by synctest.Wait: it returns once
// the loop is durably blocked in that fake-time backoff, i.e. strictly after
// the failed boundary and strictly before the retried boundary lands.
func TestGoalEvalFailedAdvisoryDuringRunNoSessionErrorOrTurnEnd(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProv{
			name:     "test",
			worker:   [][]provider.Event{asstTurn("try 1"), asstTurn("try 2")},
			evalErrN: 1,
			evalErr:  errors.New("evaluator down"),
			eval:     [][]provider.Event{asstTurn("MET: done")},
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
			t.Fatalf("POST goal status %d: %s", grec.Code, grec.Body)
		}

		// The loop runs its single failed evaluator boundary, then blocks in
		// the fake-time retry backoff before the retried (MET) boundary; that
		// durable block is where synctest.Wait returns — the deterministic
		// mid-run checkpoint the original test raced against a real 1s wait.
		synctest.Wait()

		srv.mu.Lock()
		var failed *Event
		var sawSessionError, sawTurnEnd bool
		for i := range srv.journal {
			ev := srv.journal[i]
			if ev.SessionID != id {
				continue
			}
			switch ev.Type {
			case "goal.eval_failed":
				if failed == nil {
					e := ev
					failed = &e
				}
			case evtSessionError:
				sawSessionError = true
			case evtTurnEnd:
				sawTurnEnd = true
			}
		}
		srv.mu.Unlock()

		if failed == nil {
			t.Fatal("no goal.eval_failed event journaled")
		}
		if failed.Seq == 0 {
			t.Error("goal.eval_failed event has no seq (must be durable)")
		}
		if failed.GoalEvalFailures != 1 {
			t.Errorf("goal.eval_failed GoalEvalFailures = %d, want 1", failed.GoalEvalFailures)
		}
		// State right after the failed boundary, before the retried evaluation
		// (which succeeds) can land: no session.error, no turn.end yet.
		if sawSessionError {
			t.Error("session.error journaled after a single failed boundary (below the terminal), want none")
		}
		if sawTurnEnd {
			t.Error("turn.end journaled after a single failed boundary while the loop is still running, want none")
		}

		mid := getEvalFailuresViewDirect(t, srv, id)
		if mid.Goal == nil || !mid.Goal.Active {
			t.Fatalf("goal right after goal.eval_failed = %+v, want active", mid.Goal)
		}
		if mid.Goal.EvalFailures != 1 {
			t.Errorf("goal.eval_failures right after goal.eval_failed = %d, want 1", mid.Goal.EvalFailures)
		}
		if mid.State != "goal-running" {
			t.Errorf("state right after goal.eval_failed = %q, want goal-running (advisory failure, loop keeps working)", mid.State)
		}

		// Let the loop finish: the retried boundary parses MET, achieving the
		// goal — eval_failures resets to 0 (goal.eval, then goal.achieved).
		// runGoal's own completed branch does emit turn.end (outcome
		// "completed") once the loop actually finishes, but no session.error: a
		// goal that recovered and achieved is never the loud terminal.
		srv.wg.Wait()

		srv.mu.Lock()
		var achieved bool
		for i := range srv.journal {
			ev := srv.journal[i]
			if ev.SessionID != id {
				continue
			}
			switch ev.Type {
			case "goal.achieved":
				achieved = true
			case evtSessionError:
				t.Error("session.error emitted for a goal that recovered and achieved, want none")
			case evtTurnEnd:
				if ev.Outcome != "completed" {
					t.Errorf("turn.end outcome for an achieved goal = %q, want completed", ev.Outcome)
				}
			}
		}
		srv.mu.Unlock()
		if !achieved {
			t.Fatal("goal never achieved after the recovered boundary, want a goal.achieved")
		}

		after := getEvalFailuresViewDirect(t, srv, id)
		if after.Goal == nil || !after.Goal.Achieved {
			t.Fatalf("goal after achievement = %+v, want achieved", after.Goal)
		}
		if after.Goal.EvalFailures != 0 {
			t.Errorf("eval_failures after achievement = %d, want 0 (reset by the parsed verdict)", after.Goal.EvalFailures)
		}
	})
}

// TestGoalEvalFailuresSurviveRestart is invariant 9's replay half: a session
// killed while its goal is active with a non-zero (but sub-terminal)
// consecutive eval-failure streak must reproduce the exact same
// eval_failures count after a fresh process replays the journal — the same
// "restart never loses durable state" guarantee goal_paused_test.go's
// TestGoalPausedRestartYieldsIdleAndUsable already establishes for
// paused/active/condition.
//
// Both scripted evaluator boundaries fail (MaxTurns=2, so the loop stops by
// exhausting its turn budget, never by achieving or being cleared) — the
// last durable goal.* record is goal.eval_failed(count=2), with nothing
// after it to reset the streak, so eval_failures must read 2 both before and
// after the restart. Two boundaries pay two backoff waits
// (goalRetryDelay(1)+goalRetryDelay(2) == 1s+4s == 5s of real wall-clock
// time); per AGENTS.md's synctest rule this runs on fake time inside a bubble,
// driving handleGoal directly with an httptest.ResponseRecorder and reading
// state through handleGet (no real listener; real network I/O does not work in
// a bubble) so those backoffs cost nothing. The restart is a SECOND newServer
// over the SAME dir — ADOPT — whose New reconciles the on-disk journal
// synchronously (file I/O, which the terminal test above already exercises in
// a bubble); srv1.wg.Wait guarantees every durable goal.eval_failed record is
// flushed before the second server replays it.
func TestGoalEvalFailuresSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProv{
			name:     "test",
			worker:   [][]provider.Event{asstTurn("try 1"), asstTurn("try 2")},
			evalErrN: 2,
			evalErr:  errors.New("evaluator down"),
		}
		mutate := func(o *Options) {
			o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
		}
		srv1 := newServer(t, dir, prov, 0, mutate)
		id := createSessionDirect(t, srv1, "test/m1")

		grec := httptest.NewRecorder()
		greq := httptest.NewRequest("POST", "/session/"+id+"/goal", strings.NewReader(`{"condition":"cond","max_turns":2}`))
		greq.SetPathValue("id", id)
		srv1.handleGoal(grec, greq)
		if grec.Code != http.StatusAccepted {
			t.Fatalf("POST goal status %d: %s", grec.Code, grec.Body)
		}

		srv1.wg.Wait() // both failed boundaries + their fake-time backoffs, then max-turns exhaustion

		before := getEvalFailuresViewDirect(t, srv1, id)
		if before.Goal == nil || !before.Goal.Active {
			t.Fatalf("before restart, goal = %+v, want active (max turns exhausted, never cleared)", before.Goal)
		}
		if before.Goal.EvalFailures != 2 {
			t.Fatalf("before restart, eval_failures = %d, want 2", before.Goal.EvalFailures)
		}

		if err := srv1.Close(); err != nil {
			t.Fatalf("closing first server: %v", err)
		}

		srv2 := newServer(t, dir, prov, 0, mutate)

		after := getEvalFailuresViewDirect(t, srv2, id)
		if after.Goal == nil || !after.Goal.Active {
			t.Fatalf("after restart, goal = %+v, want active", after.Goal)
		}
		if after.Goal.EvalFailures != 2 {
			t.Errorf("after restart, eval_failures = %d, want 2 (journal replay must reproduce the fold)", after.Goal.EvalFailures)
		}

		srv2.Drain(context.Background())
	})
}
