// Tests for the goal-evaluator resilience work (Round 6, Task 1): a failed
// evaluator boundary is advisory (goal.eval_failed, keep-armed, backoff) below
// goalEvalFailureLimit consecutive failures, and only a durable, sustained
// outage clears the goal with a distinct sentinel error. See goal.go's
// package doc "Round 6" section and docs/plans/2026-07-20-goal-eval-
// resilience.md's "Invariants" list — each test below is named for the
// invariant it covers.
package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestEvaluateGoalStrictReaskOnUnparseable is invariant 1: an unparseable
// first evaluator reply must make the second attempt use the stricter system
// prompt, and a parseable second attempt must return a normal verdict with no
// goal.eval_failed record.
func TestEvaluateGoalStrictReaskOnUnparseable(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "work"}),
		},
		eval: [][]provider.Event{
			evalTurn("hmm, not sure"), // unparseable: attempt 1
			evalTurn("MET: looks good"),
		},
	}
	var evs []Event
	s := goalSession(t, prov, t.TempDir())
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatalf("PursueGoal error = %v", err)
	}
	if !res.Achieved || res.Turns != 1 {
		t.Fatalf("result = %+v, want achieved in 1 turn after the stricter re-ask parses", res)
	}

	var evalReqs []*provider.Request
	for _, rq := range prov.requests {
		if len(rq.Tools) == 0 {
			evalReqs = append(evalReqs, rq)
		}
	}
	if len(evalReqs) != 2 {
		t.Fatalf("evaluator requests = %d, want 2 (one per attempt)", len(evalReqs))
	}
	if len(evalReqs[0].System) == 0 || evalReqs[0].System[0] != goalEvaluatorSystem {
		t.Errorf("attempt 1 system = %v, want the ordinary goalEvaluatorSystem", evalReqs[0].System)
	}
	if len(evalReqs[1].System) == 0 || evalReqs[1].System[0] != goalEvaluatorStrictSystem {
		t.Errorf("attempt 2 system = %v, want goalEvaluatorStrictSystem", evalReqs[1].System)
	}
	if evalReqs[0].System[0] == evalReqs[1].System[0] {
		t.Error("attempt 1 and attempt 2 used the identical system prompt, want the second stricter")
	}

	for _, ev := range evs {
		if ev.Type == EventGoalEvalFailed {
			t.Errorf("goal.eval_failed emitted even though attempt 2 parsed: %+v", ev)
		}
	}
}

// TestPursueGoalEvaluatorUnparseableTwiceIsAdvisory is invariant 2's headline
// test (red-verified against the pre-Round-6 fatal path — see the commit
// message): two consecutive unparseable evaluator replies must NOT clear the
// goal or emit session.error, must journal goal.eval_failed with count 1, and
// the NEXT turn's directive must carry the evaluation-unavailable notice —
// never the raw error text, never a stale NOT-MET reason from an earlier
// turn.
//
// Run inside a synctest bubble: a failed boundary waits goalRetryDelay
// (keyed on the consecutive count) before continuing, a real timer this test
// would otherwise pay wall-clock time for (see AGENTS.md on synctest for all
// backoff timing).
func TestPursueGoalEvaluatorUnparseableTwiceIsAdvisory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 1"}),
				asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 2"}),
			},
			eval: [][]provider.Event{
				evalTurn("unclear"),        // turn 1, attempt 1: unparseable
				evalTurn("still unclear"),  // turn 1, attempt 2 (strict): unparseable
				evalTurn("MET: now it is"), // turn 2, attempt 1: parses
			},
		}
		hooks := &fakeHooks{}
		var evs []Event
		s := goalSession(t, prov, t.TempDir(), hooks)
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil (a failed boundary is advisory)", err)
		}
		if !res.Achieved || res.Turns != 2 {
			t.Fatalf("result = %+v, want achieved in 2 turns", res)
		}
		if msgs := sessionErrorMessages(t, hooks); len(msgs) != 0 {
			t.Errorf("session.error messages = %v, want none below the terminal horizon", msgs)
		}

		var failedCount int
		for _, ev := range evs {
			if ev.Type == EventGoalCleared {
				t.Error("goal.cleared emitted for a single failed boundary, want none")
			}
			if ev.Type == EventGoalEvalFailed {
				failedCount++
				if ev.GoalTurn != 1 {
					t.Errorf("goal.eval_failed GoalTurn = %d, want 1", ev.GoalTurn)
				}
				if ev.GoalEvalFailures != 1 {
					t.Errorf("goal.eval_failed GoalEvalFailures = %d, want 1", ev.GoalEvalFailures)
				}
				if !strings.Contains(ev.GoalReason, "unparseable") {
					t.Errorf("goal.eval_failed GoalReason = %q, want it to carry the evaluator error", ev.GoalReason)
				}
			}
		}
		if failedCount != 1 {
			t.Fatalf("goal.eval_failed events = %d, want 1", failedCount)
		}

		// Turn 2's directive (the guidance message appended after turn 1) must
		// carry the fixed evaluation-unavailable notice — never the raw
		// "unparseable" error text, and never a stale NOT-MET reason (there is
		// none yet in this test, but the notice must still appear explicitly).
		h := s.History()
		var directive2 string
		for _, m := range h {
			if m.Role == message.RoleUser && strings.Contains(m.Parts.Text(), "GOAL:") {
				directive2 = m.Parts.Text()
			}
		}
		if !strings.Contains(directive2, goalEvalUnavailableNotice) {
			t.Errorf("turn 2 directive = %q, want it to contain the evaluation-unavailable notice", directive2)
		}
		if strings.Contains(directive2, "unparseable") {
			t.Errorf("turn 2 directive = %q, leaks the raw evaluator error text", directive2)
		}
	})
}

// TestPursueGoalEvaluatorRetryableErrorRecoversWithinBoundary is invariant 3:
// a retryable-class evaluator provider error must be retried in-boundary on
// the goalRetryableBackoff schedule, and recovering mid-schedule must produce
// a normal verdict with no failed boundary at all.
//
// Run inside a synctest bubble and with goalJitterFunc pinned to zero (see
// TestPursueGoalRetryableErrorLongBackoffThenRecovers) so the elapsed backoff
// is exactly assertable.
func TestPursueGoalEvaluatorRetryableErrorRecoversWithinBoundary(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })
	goalJitterFunc = func(max time.Duration) time.Duration { return 0 }

	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "work"}),
			},
			eval: [][]provider.Event{
				evalTurn("MET: recovered"),
			},
			evalErrN: 3,
			evalErr:  retryableProviderErr(provider.RetryableOverloaded),
		}
		var evs []Event
		s := goalSession(t, prov, t.TempDir())
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		start := time.Now()
		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil (retryable evaluator errors are ridden out in-boundary)", err)
		}
		if !res.Achieved || res.Turns != 1 {
			t.Fatalf("result = %+v, want achieved in 1 turn", res)
		}

		var want time.Duration
		for attempt := 1; attempt <= 3; attempt++ {
			want += goalRetryableDelay(attempt) / 2
		}
		if elapsed != want {
			t.Errorf("elapsed = %v, want exactly %v (the retryable backoff schedule for 3 failed attempts)", elapsed, want)
		}

		for _, ev := range evs {
			if ev.Type == EventGoalEvalFailed {
				t.Errorf("goal.eval_failed emitted even though the boundary recovered in-boundary: %+v", ev)
			}
		}
	})
}

// TestPursueGoalEvaluatorRetryableBudgetExhaustedFailsBoundary is invariant
// 4's retryable half: exhausting the in-boundary retryable budget
// (goalRetryableMaxAttempts) must fail the boundary — the same observable
// shape as invariant 2's headline test — never clear the goal outright.
func TestPursueGoalEvaluatorRetryableBudgetExhaustedFailsBoundary(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })
	goalJitterFunc = func(max time.Duration) time.Duration { return 0 }

	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "work"}),
			},
			evalErrN: 1000, // never recovers within this boundary
			evalErr:  retryableProviderErr(provider.RetryableOverloaded),
		}
		var evs []Event
		s := goalSession(t, prov, t.TempDir())
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{MaxTurns: 1, Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil (a failed boundary is advisory even after retryable exhaustion)", err)
		}
		if res.Achieved || res.Reason != "max turns" {
			t.Errorf("result = %+v, want not achieved, reason \"max turns\"", res)
		}
		if cond, ok := s.ActiveGoal(); !ok || cond != "cond" {
			t.Errorf("ActiveGoal = %q, %v; want still active", cond, ok)
		}

		var failedCount int
		for _, ev := range evs {
			if ev.Type == EventGoalCleared {
				t.Error("goal.cleared emitted, want none (retryable exhaustion is still just a failed boundary)")
			}
			if ev.Type == EventGoalEvalFailed {
				failedCount++
			}
		}
		if failedCount != 1 {
			t.Errorf("goal.eval_failed events = %d, want 1", failedCount)
		}
		// goalRetryableMaxAttempts calls consumed for the one boundary.
		var evalReqs int
		for _, rq := range prov.requests {
			if len(rq.Tools) == 0 {
				evalReqs++
			}
		}
		if evalReqs != goalRetryableMaxAttempts {
			t.Errorf("evaluator requests = %d, want %d (goalRetryableMaxAttempts)", evalReqs, goalRetryableMaxAttempts)
		}
	})
}

// TestPursueGoalEvaluatorNonRetryableErrorFailsBoundaryImmediately is
// invariant 4's non-retryable half: a non-retryable provider error must fail
// the boundary immediately, with no in-boundary retry (no second evaluator
// call for that attempt index, no backoff wait) and no second (strict-prompt)
// attempt either — the retry budget is orthogonal to the unparseable-reask
// budget, and neither applies to a hard provider error.
//
// Run inside a synctest bubble: the failed boundary still waits the short
// goalRetryDelay before PursueGoal continues (see AGENTS.md on synctest for
// all backoff timing).
func TestPursueGoalEvaluatorNonRetryableErrorFailsBoundaryImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "work"}),
			},
			evalErrN: 1,
			evalErr:  errors.New("evaluator: 400 bad request"),
		}
		var evs []Event
		s := goalSession(t, prov, t.TempDir())
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{MaxTurns: 1, Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil", err)
		}
		if res.Achieved || res.Reason != "max turns" {
			t.Errorf("result = %+v, want not achieved, reason \"max turns\"", res)
		}

		var evalReqs int
		for _, rq := range prov.requests {
			if len(rq.Tools) == 0 {
				evalReqs++
			}
		}
		if evalReqs != 1 {
			t.Errorf("evaluator requests = %d, want exactly 1 (no retry for a non-retryable error)", evalReqs)
		}

		var failedCount int
		for _, ev := range evs {
			if ev.Type == EventGoalEvalFailed {
				failedCount++
				if !strings.Contains(ev.GoalReason, "bad request") {
					t.Errorf("goal.eval_failed GoalReason = %q, want it to carry the provider error", ev.GoalReason)
				}
			}
		}
		if failedCount != 1 {
			t.Errorf("goal.eval_failed events = %d, want 1", failedCount)
		}
	})
}

// TestPursueGoalEvaluatorConsecutiveFailureCounting is invariant 5: fail,
// fail, SUCCEED (parsed NOT MET), fail must produce counts 1, 2, reset, 1 —
// the horizon is a streak, not a cumulative total — and the terminal must
// never fire across this sequence (well below goalEvalFailureLimit).
//
// Run inside a synctest bubble: three failed boundaries each wait the short
// goalRetryDelay (see AGENTS.md on synctest for all backoff timing).
func TestPursueGoalEvaluatorConsecutiveFailureCounting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 1"}),
				asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 2"}),
				asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 3"}),
				asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 4"}),
			},
			eval: [][]provider.Event{
				evalTurn("unclear 1a"), evalTurn("unclear 1b"), // turn 1: fail
				evalTurn("unclear 2a"), evalTurn("unclear 2b"), // turn 2: fail
				evalTurn("NOT MET: keep going"),                // turn 3: succeeds, resets
				evalTurn("unclear 4a"), evalTurn("unclear 4b"), // turn 4: fail
			},
		}
		var evs []Event
		s := goalSession(t, prov, t.TempDir())
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{MaxTurns: 4, Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil", err)
		}
		if res.Achieved || res.Reason != "max turns" {
			t.Errorf("result = %+v, want not achieved, reason \"max turns\"", res)
		}

		var counts []int
		var sawEval bool
		for _, ev := range evs {
			switch ev.Type {
			case EventGoalEvalFailed:
				counts = append(counts, ev.GoalEvalFailures)
			case EventGoalEval:
				sawEval = true
			case EventGoalCleared:
				t.Error("goal.cleared emitted, want none — well below the terminal horizon")
			}
		}
		want := []int{1, 2, 1}
		if len(counts) != len(want) {
			t.Fatalf("goal.eval_failed counts = %v, want %v", counts, want)
		}
		for i := range want {
			if counts[i] != want[i] {
				t.Errorf("goal.eval_failed count[%d] = %d, want %d (full sequence %v)", i, counts[i], want[i], counts)
			}
		}
		if !sawEval {
			t.Error("no goal.eval emitted for turn 3's successful (NOT MET) boundary")
		}
	})
}

// TestPursueGoalEvaluatorTerminalAfterConsecutiveFailureLimit is invariant 6:
// goalEvalFailureLimit CONSECUTIVE failed boundaries must clear the goal with
// the dedicated reason, return a distinct sentinel error type
// (*goalEvaluatorExhaustedError, recognizable via errors.As, never by
// string-matching), leave the session no longer goal-active, and emit
// exactly one session.error (the terminal must be loud).
//
// Run inside a synctest bubble: goalEvalFailureLimit-1 failed boundaries each
// wait the short goalRetryDelay, growing per the deterministic schedule (see
// AGENTS.md on synctest for all backoff timing) — real wall-clock time this
// would otherwise cost tens of seconds.
func TestPursueGoalEvaluatorTerminalAfterConsecutiveFailureLimit(t *testing.T) {
	dir := t.TempDir()
	var s *Session
	var evs []Event
	var err error
	hooks := &fakeHooks{}
	synctest.Test(t, func(t *testing.T) {
		worker := make([][]provider.Event, goalEvalFailureLimit)
		var eval [][]provider.Event
		for i := 0; i < goalEvalFailureLimit; i++ {
			worker[i] = asstTurn(provider.StopEndTurn, &message.Text{Text: fmt.Sprintf("turn %d", i+1)})
			eval = append(eval, evalTurn("unclear a"), evalTurn("unclear b"))
		}
		prov := &goalProvider{worker: worker, eval: eval}
		s = goalSession(t, prov, dir, hooks)
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		_, err = s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
	})
	if err == nil {
		t.Fatal("PursueGoal succeeded, want the terminal error")
	}
	var sentinel *goalEvaluatorExhaustedError
	if !errors.As(err, &sentinel) {
		t.Fatalf("err = %v (%T), want *goalEvaluatorExhaustedError", err, err)
	}
	if sentinel.failures != goalEvalFailureLimit {
		t.Errorf("sentinel.failures = %d, want %d", sentinel.failures, goalEvalFailureLimit)
	}
	if cond, ok := s.ActiveGoal(); ok {
		t.Fatalf("ActiveGoal = %q, still active at the terminal horizon — want cleared", cond)
	}

	var failedCount int
	var clearedReason string
	var clearedCount int
	for _, ev := range evs {
		switch ev.Type {
		case EventGoalEvalFailed:
			failedCount++
		case EventGoalCleared:
			clearedCount++
			clearedReason = ev.GoalReason
		case EventGoalAchieved:
			t.Error("goal.achieved emitted, want none")
		}
	}
	if failedCount != goalEvalFailureLimit {
		t.Errorf("goal.eval_failed events = %d, want %d", failedCount, goalEvalFailureLimit)
	}
	if clearedCount != 1 {
		t.Fatalf("goal.cleared events = %d, want 1", clearedCount)
	}
	wantReason := fmt.Sprintf("goal evaluator failed at %d consecutive turn boundaries", goalEvalFailureLimit)
	if clearedReason != wantReason {
		t.Errorf("goal.cleared reason = %q, want %q", clearedReason, wantReason)
	}

	if msgs := sessionErrorMessages(t, hooks); len(msgs) != 1 || msgs[0] != err.Error() {
		t.Errorf("session.error messages = %v, want exactly [%q] (the terminal must be loud)", msgs, err.Error())
	}

	loaded, err2 := LoadSession(s.cfg, s.ID)
	if err2 != nil {
		t.Fatal(err2)
	}
	if cond, ok := loaded.ActiveGoal(); ok {
		t.Errorf("resumed ActiveGoal = %q, active after the terminal clear — zombie goal survives reload", cond)
	}
}

// TestPursueGoalEvaluatorFailureDiscardedWhenGoalUpdatedMidCall is invariant
// 7 (the generation guard): an evaluator failure racing a concurrent
// UpdateGoal must not journal goal.eval_failed for the now-stale generation,
// and the consecutive-failure counter must be left untouched by it (the
// discarded attempt never happened for this streak's purposes). It uses
// goalProvider's onEvalStream hook to fire the UpdateGoal synchronously from
// inside the blocked evaluator call — no real concurrency needed, since
// Stream runs on the same goroutine as PursueGoal.
func TestPursueGoalEvaluatorFailureDiscardedWhenGoalUpdatedMidCall(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 1"}),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 2"}),
		},
		eval: [][]provider.Event{
			evalTurn("MET: satisfied under the new condition"),
		},
		evalErrN: 1,
		evalErr:  errors.New("evaluator exploded"),
	}
	var evs []Event
	s := goalSession(t, prov, t.TempDir())
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }
	prov.onEvalStream = func(call int) {
		if call == 1 {
			if err := s.UpdateGoal("new condition"); err != nil {
				t.Fatal(err)
			}
		}
	}

	res, err := s.PursueGoal(context.Background(), "orig condition", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatalf("PursueGoal error = %v, want nil", err)
	}
	if !res.Achieved || res.Turns != 2 {
		t.Fatalf("result = %+v, want achieved in 2 turns (turn 1's failure discarded as stale, turn 2 achieves)", res)
	}

	for _, ev := range evs {
		if ev.Type == EventGoalEvalFailed {
			t.Errorf("goal.eval_failed emitted for a failure that raced a concurrent UpdateGoal, want it discarded silently: %+v", ev)
		}
	}
}

// TestClearGoalMidFailingBoundariesStopsCleanly is invariant 10: an operator
// ClearGoal (DELETE /goal) arriving after several failed boundaries have
// already accumulated — but before the terminal horizon — must still stop
// the loop cleanly (Achieved=false, Reason "goal cleared", no error, no
// session.error), same as ClearGoal always has. Uses goalProvider's
// onEvalStream hook to call ClearGoal synchronously mid-run, from inside the
// evaluator call for the boundary that would otherwise be the third
// consecutive failure — no real concurrency needed.
//
// Run inside a synctest bubble: the two failed boundaries before the clear
// each wait the short goalRetryDelay (see AGENTS.md on synctest for all
// backoff timing).
func TestClearGoalMidFailingBoundariesStopsCleanly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 1"}),
				asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 2"}),
				asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 3"}),
			},
			eval: [][]provider.Event{
				evalTurn("unclear 1a"), evalTurn("unclear 1b"), // turn 1: fails (count 1)
				evalTurn("unclear 2a"), evalTurn("unclear 2b"), // turn 2: fails (count 2)
				evalTurn("unclear 3a"), evalTurn("unclear 3b"), // turn 3: ClearGoal fires mid-call, then fails
			},
		}
		hooks := &fakeHooks{}
		var evs []Event
		s := goalSession(t, prov, t.TempDir(), hooks)
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }
		prov.onEvalStream = func(call int) {
			// Turn 3's first attempt is the 5th evaluator call overall (2 per
			// failed turn x 2 turns, +1). Clear right as it starts.
			if call == 5 {
				if !s.ClearGoal() {
					t.Fatal("ClearGoal returned false for an active goal")
				}
			}
		}

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil (a caller-initiated clear is a clean stop)", err)
		}
		if res.Achieved {
			t.Fatalf("result = %+v, want a clean stop", res)
		}
		if res.Reason != "goal cleared" {
			t.Errorf("result.Reason = %q, want %q", res.Reason, "goal cleared")
		}
		if msgs := sessionErrorMessages(t, hooks); len(msgs) != 0 {
			t.Errorf("session.error messages = %v, want none for an operator clear", msgs)
		}

		var clearedReason string
		var clearedCount, failedCount int
		for _, ev := range evs {
			switch ev.Type {
			case EventGoalCleared:
				clearedCount++
				clearedReason = ev.GoalReason
			case EventGoalEvalFailed:
				failedCount++
			}
		}
		if clearedCount != 1 {
			t.Fatalf("goal.cleared events = %d, want 1", clearedCount)
		}
		// An operator clear (ClearGoal with no reason) must be distinguishable
		// from the automatic terminal clear (which always carries a
		// "consecutive turn boundaries" reason) — see clearGoal's doc comment.
		if clearedReason != "" {
			t.Errorf("goal.cleared reason = %q, want empty (operator clear, not the automatic terminal)", clearedReason)
		}
		// Turns 1 and 2 both failed and were journaled before the clear; turn
		// 3's failure raced (and lost to) the clear, so it must not add a third.
		if failedCount != 2 {
			t.Errorf("goal.eval_failed events = %d, want 2 (turn 3's raced failure must not be journaled after the clear)", failedCount)
		}
		if cond, ok := s.ActiveGoal(); ok {
			t.Errorf("ActiveGoal = %q, still active after ClearGoal", cond)
		}
	})
}

// TestIsGoalEvaluatorExhausted is the exported-hook test for Task 2 (server):
// a caller outside this package (server/journal.go's turnEndOutcome) needs to
// recognize the terminal sentinel without reaching into the unexported
// goalEvaluatorExhaustedError type or string-matching GoalReason — mirrors
// provider.IsContextOverflow's shape and its own test (provider/errors_test.go).
func TestIsGoalEvaluatorExhausted(t *testing.T) {
	if IsGoalEvaluatorExhausted(nil) {
		t.Error("nil classified as evaluator-exhausted")
	}
	if IsGoalEvaluatorExhausted(errors.New("boom")) {
		t.Error("plain error classified as evaluator-exhausted")
	}
	exhausted := &goalEvaluatorExhaustedError{err: errors.New("provider down"), failures: goalEvalFailureLimit}
	if !IsGoalEvaluatorExhausted(exhausted) {
		t.Error("goalEvaluatorExhaustedError not classified as evaluator-exhausted")
	}
	// errors.As sees through a wrapper too.
	wrapped := fmt.Errorf("goal loop: %w", exhausted)
	if !IsGoalEvaluatorExhausted(wrapped) {
		t.Error("wrapped goalEvaluatorExhaustedError not classified as evaluator-exhausted")
	}
}

// TestEvalFailureStreakResetsOnConditionUpdate is the regression test for the
// evalFailures/evalFailuresGen pairing: the CONSECUTIVE-failure streak must
// reset whenever a turn's own fresh snapshot generation no longer matches
// the generation the streak was last accumulated against (an UpdateGoal
// landed in between), exactly mirroring the server's own fold
// (server/journal.go's EventGoalUpdated case resets GoalSummary.evalFailures
// to 0 "since the streak is measured against a condition").
//
// Uses scriptedGoalUpdateProvider's afterCall (see
// TestPursueGoalPicksUpUpdatedConditionNextTurn) to fire UpdateGoal
// deterministically right after turn 3's WORKER call returns — i.e. after
// turns 1 and 2 have each failed a boundary cleanly (counts 1, 2) against
// the original generation, but before turn 3's own evaluator calls run.
// That timing makes turn 3's own boundary failure race the update and get
// discarded as stale (the same generation-guard every other stale-discard
// site in this loop already relies on — see goalStatus) — turn 3 contributes
// no goal.eval_failed record at all — and turns 4-8 then fail cleanly
// against the NEW generation.
//
// Pre-fix (evalFailures carried across the update with no generation check),
// the streak instead continues from 2: turn 4 already logs count 3, and the
// terminal fires two turns early, at turn 6, with only 5 total
// goal.eval_failed records ([1,2,3,4,5]) instead of the honest 7
// ([1,2,1,2,3,4,5]) a FULL fresh 5-failure horizon against the new
// condition requires. This is the exact failure this test red-verifies
// against.
//
// Run inside a synctest bubble: 7 failed boundaries each wait goalRetryDelay
// (see AGENTS.md on synctest for all backoff timing).
func TestEvalFailureStreakResetsOnConditionUpdate(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		const totalTurns = 8 // 2 pre-update + 1 discarded-by-the-race + 5 fresh post-update
		worker := make([][]provider.Event, totalTurns)
		var eval [][]provider.Event
		for i := 0; i < totalTurns; i++ {
			worker[i] = asstTurn(provider.StopEndTurn, &message.Text{Text: fmt.Sprintf("turn %d", i+1)})
			eval = append(eval, evalTurn("unclear a"), evalTurn("unclear b")) // unparseable both attempts: every boundary fails
		}
		prov := &scriptedGoalUpdateProvider{worker: worker, eval: eval}
		hooks := &fakeHooks{}
		var evs []Event
		s := goalSession(t, prov, dir, hooks)
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }
		prov.afterCall = func(n int) {
			// call 7 is turn 3's worker call (turn 1: calls 1-3, turn 2: calls
			// 4-6, turn 3's worker: call 7) — right after it returns, before
			// turn 3's own evaluator calls (8, 9) run.
			if n == 7 {
				if err := s.UpdateGoal("the new condition"); err != nil {
					t.Fatalf("UpdateGoal = %v", err)
				}
			}
		}

		_, err := s.PursueGoal(context.Background(), "the original condition", GoalOptions{Evaluator: evalModel})
		if err == nil {
			t.Fatal("PursueGoal succeeded, want the terminal error")
		}
		var sentinel *goalEvaluatorExhaustedError
		if !errors.As(err, &sentinel) {
			t.Fatalf("err = %v (%T), want *goalEvaluatorExhaustedError", err, err)
		}
		if sentinel.failures != goalEvalFailureLimit {
			t.Errorf("sentinel.failures = %d, want %d", sentinel.failures, goalEvalFailureLimit)
		}

		var counts []int
		for _, ev := range evs {
			if ev.Type == EventGoalEvalFailed {
				counts = append(counts, ev.GoalEvalFailures)
			}
		}
		want := []int{1, 2, 1, 2, 3, 4, 5}
		if len(counts) != len(want) {
			t.Fatalf("goal.eval_failed counts = %v, want %v (streak must reset to a FULL fresh %d-failure horizon after the mid-loop UpdateGoal)", counts, want, goalEvalFailureLimit)
		}
		for i := range want {
			if counts[i] != want[i] {
				t.Fatalf("goal.eval_failed counts = %v, want %v (streak must reset to a FULL fresh %d-failure horizon after the mid-loop UpdateGoal)", counts, want, goalEvalFailureLimit)
			}
		}

		if msgs := sessionErrorMessages(t, hooks); len(msgs) != 1 || msgs[0] != err.Error() {
			t.Errorf("session.error messages = %v, want exactly [%q] (the terminal must be loud)", msgs, err.Error())
		}
	})
}
