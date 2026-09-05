package engine

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// providerExhaustedErr builds a fake provider error marked permanent AND
// classified provider.ErrKindProviderExhausted, as if an adapter had
// regex-matched an account-level usage-limit rejection (see
// provider/anthropic/anthropic.go's parseUsageExhaustion and apiError).
// Reproduces the live incident fingerprint verbatim (box bx-01m0x8996):
// "[permanent] anthropic: You have reached your specified API usage
// limits. You will regain access on <date>."
func providerExhaustedErr() error {
	return provider.MarkPermanent(&provider.Error{
		Kind:        provider.ErrKindProviderExhausted,
		Raw:         "anthropic: You have reached your specified API usage limits. You will regain access on 2026-09-01 at 00:00 UTC.",
		RecoverHint: "2026-09-01 at 00:00 UTC",
	})
}

// TestPursueGoalProviderExhaustedRetriesThenRecovers is the red-first
// regression test for the second live-evidence defect on box bx-01m0x8996:
// "engine: goal worker turn parked after 1 permanent-tier attempt(s):
// [permanent] anthropic: You have reached your specified API usage
// limits...". Before this fix, provider.AsProviderExhausted(err) was never
// consulted — an exhausted error is wrapped provider.MarkPermanent (see
// provider.ErrKindProviderExhausted's doc comment: adapters mark it
// permanent for ordinary HTTP-retry purposes, since no short backoff
// schedule outlives a monthly quota), so promptTurnWithRetry's permanent
// fail-fast branch caught it and parked after exactly ONE attempt, with no
// resume path short of an operator DELETE + re-register.
//
// This proves the fix: an account wall that CLEARS within the
// goalProviderExhaustedMaxAttempts budget lets the worker turn — and the
// whole goal — complete normally, with no operator action of any kind. The
// fake provider fails 3 times with the exhausted classification before
// succeeding (more than one attempt, proving retry actually happened; well
// under goalProviderExhaustedMaxAttempts, proving the budget is generous
// enough to ride out a real recovery).
func TestPursueGoalProviderExhaustedRetriesThenRecovers(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })
	goalJitterFunc = func(max time.Duration) time.Duration { return 0 } // deterministic: exactly half the base delay

	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 3,
			workerErr:  providerExhaustedErr(),
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "all done"}),
			},
			eval: [][]provider.Event{
				evalTurn("MET: looks complete"),
			},
		}
		var evs []Event
		s := goalSession(t, prov, t.TempDir())
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil (a provider-exhausted wall that clears must resume on its own)", err)
		}
		if !res.Achieved || res.Turns != 1 {
			t.Fatalf("result = %+v, want achieved in 1 turn after retrying past 3 provider-exhausted errors", res)
		}
		if prov.workerCall != 4 {
			t.Errorf("worker provider calls = %d, want exactly 4 (3 failures + 1 success)", prov.workerCall)
		}

		var stalled int
		for _, ev := range evs {
			switch ev.Type {
			case EventGoalStalled:
				stalled++
				if !ev.GoalRetryable {
					t.Errorf("goal.stalled event %d: GoalRetryable = false, want true (a clearing account wall is retried, not fail-fast)", stalled)
				}
				if ev.GoalRetryableClass != string(goalClassProviderExhausted) {
					t.Errorf("goal.stalled event %d: GoalRetryableClass = %q, want %q", stalled, ev.GoalRetryableClass, goalClassProviderExhausted)
				}
				if !ev.GoalWaiting {
					t.Errorf("goal.stalled event %d: GoalWaiting = false, want true (budget not exhausted)", stalled)
				}
				// Review finding: err.Error() for a provider-exhausted
				// error is "[permanent] anthropic: ..." (see
				// providerExhaustedErr), which would self-contradict this
				// SAME record's GoalRetryable:true/GoalRetryableClass:
				// "provider_exhausted" fields above. The reason must
				// instead read like classifyGoalWorkerError's honest
				// classified text, exactly like goal.parked already does.
				if strings.Contains(ev.GoalReason, "[permanent]") {
					t.Errorf("goal.stalled event %d: GoalReason = %q, must not carry the raw [permanent]-tagged provider text — self-contradicts GoalRetryable=true", stalled, ev.GoalReason)
				}
				if ev.GoalReason != "provider account usage limit exhausted the retry budget" {
					t.Errorf("goal.stalled event %d: GoalReason = %q, want the same honest classified reason goal.parked uses", stalled, ev.GoalReason)
				}
			case EventGoalParked:
				t.Error("goal.parked emitted — a provider-exhausted wall that clears within budget must never park")
			}
		}
		if stalled != 3 {
			t.Errorf("goal.stalled events = %d, want 3 (one per failed attempt)", stalled)
		}
		if cond, ok := s.ActiveGoal(); ok {
			t.Errorf("ActiveGoal = %q, active after achievement, want inactive", cond)
		}
	})
}

// TestPursueGoalProviderExhaustedBudgetExhaustedParksHonestly proves the
// other half of the fix: an account wall that outlasts the entire
// goalProviderExhaustedMaxAttempts budget (a quota that resets in days, not
// minutes) still must not pin the run slot forever — it parks, like every
// other exhausted worker retry tier —
// but the classification must be HONEST: "provider account usage limit
// exhausted the retry budget", never "permanent provider error and cannot
// succeed on retry" (which the pre-fix single-attempt fail-fast produced,
// and which is actively wrong for a wall that lifts on its own). The goal
// stays fully active, ready for a later external resume, exactly like every
// other parked tier.
func TestPursueGoalProviderExhaustedBudgetExhaustedParksHonestly(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })
	goalJitterFunc = func(max time.Duration) time.Duration { return 0 }

	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 1000, // never recovers within the test
			workerErr:  providerExhaustedErr(),
		}
		var evs []Event
		s := goalSession(t, prov, t.TempDir())
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if res != nil {
			t.Fatalf("result = %+v, want nil (an exit-park returns an error, not a GoalResult)", res)
		}
		if !IsGoalWorkerParked(err) {
			t.Fatalf("err = %v, want IsGoalWorkerParked", err)
		}
		if prov.workerCall != goalProviderExhaustedMaxAttempts {
			t.Errorf("worker provider calls = %d, want exactly %d (the provider-exhausted budget)", prov.workerCall, goalProviderExhaustedMaxAttempts)
		}

		if cond, ok := s.ActiveGoal(); !ok || cond != "cond" {
			t.Errorf("ActiveGoal = %q, %v; want the goal left ACTIVE for resume, not cleared", cond, ok)
		}

		var sawCleared bool
		var parked int
		for _, ev := range evs {
			switch ev.Type {
			case EventGoalCleared:
				sawCleared = true
			case EventGoalParked:
				parked++
				if ev.GoalReason != "provider account usage limit exhausted the retry budget" {
					t.Errorf("goal.parked GoalReason = %q, want the honest provider-exhausted reason, not a permanent-error one", ev.GoalReason)
				}
				if !ev.GoalRetryable {
					t.Error("goal.parked GoalRetryable = false, want true (an account wall is not a malformed, unretriable request)")
				}
				if ev.GoalAttempts != goalProviderExhaustedMaxAttempts {
					t.Errorf("goal.parked GoalAttempts = %d, want %d", ev.GoalAttempts, goalProviderExhaustedMaxAttempts)
				}
			}
		}
		if sawCleared {
			t.Error("goal.cleared emitted — a provider-exhausted budget exhaustion must park, never clear")
		}
		if parked != 1 {
			t.Fatalf("goal.parked events = %d, want exactly 1", parked)
		}
	})
}

// TestClassifyGoalWorkerErrorProviderExhaustedReason is a narrow unit check
// on classifyGoalWorkerError's new branch: the provider-exhausted class must
// render distinctly from both the generic retryable-weather message and the
// permanent-error one, regardless of what the permanent bool happens to be
// (mirrors the mutual-exclusion documented on goalWorkerParkedError.permanent
// — this class is always reached with permanent already false, but the
// function itself must not depend on the caller getting that right).
func TestClassifyGoalWorkerErrorProviderExhaustedReason(t *testing.T) {
	got := classifyGoalWorkerError(true, false, goalClassProviderExhausted)
	want := "provider account usage limit exhausted the retry budget"
	if got != want {
		t.Errorf("classifyGoalWorkerError(true, false, provider_exhausted) = %q, want %q", got, want)
	}
	if strings.Contains(got, "overloaded") || strings.Contains(got, string(provider.RetryableOverloaded)) {
		t.Errorf("classifyGoalWorkerError provider-exhausted reason = %q, must not read like ordinary weather", got)
	}
}
