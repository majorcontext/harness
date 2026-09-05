package engine

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestPursueGoalEvaluatorTruncatedShortBackoffNotWeather is the red-first
// test for the evaluator-side counterpart of promptTurnWithRetry's
// truncated tier (see goalStreamTruncatedMaxAttempts and
// TestPursueGoalStreamTruncatedBudgetExhaustedParks in
// goal_stream_truncated_test.go): a response stream classified
// provider.RetryableStreamTruncated on the EVALUATOR call must retry on the
// SAME short, non-jittered schedule the worker's truncated tier uses
// (goalRetryDelay via waitGoalRetryBackoff), bounded by
// goalStreamTruncatedMaxAttempts — never the long jittered weather schedule
// (goalRetryableBackoff/goalRetryableMaxAttempts) runEvaluatorWithRetry used
// for every retryable class before this fix. Waiting longer cannot raise a
// stream ceiling (see goalStreamTruncatedMaxAttempts's doc comment), so an
// evaluator call that never stops truncating must fail its boundary fast,
// not burn up to ~30 minutes on it.
//
// A single failed boundary is advisory, not fatal (Round 6): PursueGoal
// journals goal.eval_failed and continues rather than clearing the goal, so
// this test scripts a second worker turn/evaluation that succeeds with MET
// to let the loop terminate deterministically instead of relying on
// MaxTurns.
func TestPursueGoalEvaluatorTruncatedShortBackoffNotWeather(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "working on it"}),
				asstTurn(provider.StopEndTurn, &message.Text{Text: "all done"}),
			},
			// The first boundary's evaluator call truncates on every one of
			// its goalStreamTruncatedMaxAttempts attempts; the second
			// boundary's call (evalDieN exhausted by then) falls through to
			// the scripted MET verdict below.
			evalDieN:      goalStreamTruncatedMaxAttempts,
			evalDieEvents: []provider.Event{{Type: provider.EventTextDelta, Text: "MET: premature"}},
			evalDieErr:    truncatedProviderErr(),
			eval: [][]provider.Event{
				evalTurn("MET: done"),
			},
		}
		var evs []Event
		var evalFailedAt time.Duration
		s := goalSession(t, prov, t.TempDir())
		start := time.Now()
		s.cfg.OnEvent = func(ev Event) {
			evs = append(evs, ev)
			// Capture elapsed at the moment the first boundary's failure is
			// journaled — after runEvaluatorWithRetry's own truncated-tier
			// waits, but BEFORE PursueGoal's separate post-failure
			// goalRetryDelay(evalFailures) wait that lets the worker
			// continue to the next turn. This isolates the schedule under test.
			if ev.Type == EventGoalEvalFailed && evalFailedAt == 0 {
				evalFailedAt = time.Since(start)
			}
		}

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil (a single failed boundary is advisory, not fatal)", err)
		}
		if !res.Achieved || res.Turns != 2 || res.Reason != "done" {
			t.Fatalf("result = %+v, want achieved in 2 turns via the second boundary's MET verdict", res)
		}

		if got := prov.evalCall; got != goalStreamTruncatedMaxAttempts+1 {
			t.Errorf("evaluator calls = %d, want %d (%d truncated attempts on the failed boundary, 1 on the succeeding one)",
				got, goalStreamTruncatedMaxAttempts+1, goalStreamTruncatedMaxAttempts)
		}

		// The failed boundary's evaluator retries must follow the SHORT
		// goalRetryDelay schedule (1s, 4s), one wait per failed attempt
		// short of the ceiling — never goalRetryableBackoff's long jittered
		// weather schedule.
		want := goalRetryDelay(1) + goalRetryDelay(2)
		if evalFailedAt != want {
			t.Errorf("time to goal.eval_failed = %v, want exactly %v (short goalRetryDelay schedule for %d failed truncated attempts, NOT the weather schedule)",
				evalFailedAt, want, goalStreamTruncatedMaxAttempts-1)
		}

		var evalFailed int
		for _, ev := range evs {
			switch ev.Type {
			case EventGoalEvalFailed:
				evalFailed++
				if ev.GoalEvalFailures != 1 {
					t.Errorf("goal.eval_failed GoalEvalFailures = %d, want 1", ev.GoalEvalFailures)
				}
			case EventGoalCleared:
				t.Error("goal.cleared emitted — a single failed boundary must stay advisory, never clear")
			}
		}
		if evalFailed != 1 {
			t.Fatalf("goal.eval_failed events = %d, want exactly 1", evalFailed)
		}
	})
}
