package engine

import (
	"context"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// truncatedProviderErr builds the error shape every provider adapter now
// returns when a response stream dies before its terminal event (see
// provider.MarkStreamTruncated and the adapters' stream-read boundaries):
// the 2026-08-06 nimble-pizza incident's failure, where a gateway cut
// streams at a ~111s ceiling with HTTP 200 already sent.
func truncatedProviderErr() error {
	return provider.MarkStreamTruncated(io.EOF)
}

// TestPursueGoalStreamTruncatedShortBackoffThenRecovers is the red-first
// test for the goal-loop half of the truncation fix: a worker-turn error
// classified RetryableStreamTruncated must be retried — the incident's two
// truncations were followed by a fast, clean success minutes later — but on
// the SHORT deterministic-tier schedule (goalRetryDelay: 1s, 4s, ...), not
// the retryable tier's 5s→5min weather schedule: a stream ceiling is not
// weather, waiting longer does not raise it, and each doomed attempt is
// expensive (a full re-prompt), so the budget must stay small.
func TestPursueGoalStreamTruncatedShortBackoffThenRecovers(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })
	goalJitterFunc = func(max time.Duration) time.Duration { return 0 }

	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: goalStreamTruncatedMaxAttempts - 1, // recovers on the last in-budget attempt
			workerErr:  truncatedProviderErr(),
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

		start := time.Now()
		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil (stream truncation gets its own retry budget)", err)
		}
		if !res.Achieved || res.Turns != 1 {
			t.Fatalf("result = %+v, want achieved in 1 turn after retrying past %d truncations", res, goalStreamTruncatedMaxAttempts-1)
		}

		// The waits must follow the SHORT deterministic-tier schedule, one
		// wait per failed attempt — proving truncation neither rides the
		// long retryable weather schedule nor fails fast without retrying.
		var want time.Duration
		for attempt := 1; attempt <= goalStreamTruncatedMaxAttempts-1; attempt++ {
			want += goalRetryDelay(attempt)
		}
		if elapsed != want {
			t.Errorf("elapsed = %v, want exactly %v (goalRetryDelay schedule for %d failed attempts)", elapsed, want, goalStreamTruncatedMaxAttempts-1)
		}

		var stalled int
		for _, ev := range evs {
			if ev.Type != EventGoalStalled {
				continue
			}
			stalled++
			if !ev.GoalRetryable {
				t.Errorf("goal.stalled event %d: GoalRetryable = false, want true", stalled)
			}
			if ev.GoalRetryableClass != string(provider.RetryableStreamTruncated) {
				t.Errorf("goal.stalled event %d: GoalRetryableClass = %q, want %q", stalled, ev.GoalRetryableClass, provider.RetryableStreamTruncated)
			}
			if !ev.GoalWaiting {
				t.Errorf("goal.stalled event %d: GoalWaiting = false, want true (budget not exhausted)", stalled)
			}
		}
		if stalled != goalStreamTruncatedMaxAttempts-1 {
			t.Errorf("goal.stalled events = %d, want %d (one per failed attempt)", stalled, goalStreamTruncatedMaxAttempts-1)
		}
	})
}

// TestPursueGoalStreamTruncatedBudgetExhaustedParks: a stream that
// truncates on every attempt must exhaust goalStreamTruncatedMaxAttempts —
// not the deterministic tier's goalWorkerRetries+1 (the incident's actual
// bug: parked after ~5s), and not the retryable tier's 12-attempt/~30min
// budget (12 full re-prompts of a >100s turn at Opus rates) — and then
// PARK, exactly like every other exhaustion tier: goal still active,
// goal.parked journaled, IsGoalWorkerParked sentinel returned.
func TestPursueGoalStreamTruncatedBudgetExhaustedParks(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })
	goalJitterFunc = func(max time.Duration) time.Duration { return 0 }

	dir := t.TempDir()
	var s *Session
	var evs []Event
	var err error
	var elapsed time.Duration
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 1000, // never recovers
			workerErr:  truncatedProviderErr(),
		}
		s = goalSession(t, prov, dir)
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		start := time.Now()
		_, err = s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		elapsed = time.Since(start)
	})
	if err == nil {
		t.Fatal("PursueGoal with a permanently truncating stream succeeded, want a park")
	}
	if !IsGoalWorkerParked(err) {
		t.Fatalf("err = %v, want IsGoalWorkerParked", err)
	}
	if cond, ok := s.ActiveGoal(); !ok || cond != "cond" {
		t.Fatalf("ActiveGoal = %q, %v; want still active after a truncation-budget park", cond, ok)
	}

	// Waits happen between attempts only (the exhausting attempt returns
	// without waiting), on the short schedule.
	var want time.Duration
	for attempt := 1; attempt <= goalStreamTruncatedMaxAttempts-1; attempt++ {
		want += goalRetryDelay(attempt)
	}
	if elapsed != want {
		t.Errorf("elapsed = %v, want exactly %v", elapsed, want)
	}

	var stalled, parked int
	for _, ev := range evs {
		switch ev.Type {
		case EventGoalStalled:
			stalled++
			wantWaiting := stalled < goalStreamTruncatedMaxAttempts
			if ev.GoalWaiting != wantWaiting {
				t.Errorf("goal.stalled event %d: GoalWaiting = %v, want %v", stalled, ev.GoalWaiting, wantWaiting)
			}
		case EventGoalCleared:
			t.Error("goal.cleared emitted — truncation-budget exhaustion must park, never clear")
		case EventGoalParked:
			parked++
			if !ev.GoalRetryable {
				t.Error("goal.parked GoalRetryable = false, want true (truncation is a retryable class)")
			}
			if ev.GoalRetryableClass != string(provider.RetryableStreamTruncated) {
				t.Errorf("goal.parked GoalRetryableClass = %q, want %q", ev.GoalRetryableClass, provider.RetryableStreamTruncated)
			}
			if ev.GoalAttempts != goalStreamTruncatedMaxAttempts {
				t.Errorf("goal.parked GoalAttempts = %d, want %d", ev.GoalAttempts, goalStreamTruncatedMaxAttempts)
			}
		}
	}
	if stalled != goalStreamTruncatedMaxAttempts {
		t.Errorf("goal.stalled events = %d, want exactly %d", stalled, goalStreamTruncatedMaxAttempts)
	}
	if parked != 1 {
		t.Fatalf("goal.parked events = %d, want exactly 1", parked)
	}
}

// TestGoalEvaluatorTruncatedStreamNeverParsedAsVerdict: the evaluator's
// stream-consumption loop used errors.Is(err, io.EOF) to detect normal
// end-of-iteration — but a TRUNCATED stream's classified error (see
// provider.MarkStreamTruncated) wraps the underlying io.EOF, so errors.Is
// matched it too and the partial text already streamed was leniently parsed
// as a complete verdict. A cut mid-"MET: ..." must never achieve a goal on
// a verdict the evaluator never finished: the truncation must surface as an
// eval-call error (riding the evaluator's own in-boundary retryable
// backoff) and the NEXT, complete verdict must decide.
func TestGoalEvaluatorTruncatedStreamNeverParsedAsVerdict(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })
	goalJitterFunc = func(max time.Duration) time.Duration { return 0 }

	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "did the work"}),
			},
			evalDieN:      1,
			evalDieEvents: []provider.Event{{Type: provider.EventTextDelta, Text: "MET: premature"}},
			evalDieErr:    truncatedProviderErr(),
			eval: [][]provider.Event{
				evalTurn("MET: done"),
			},
		}
		s := goalSession(t, prov, t.TempDir())

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil", err)
		}
		if !res.Achieved {
			t.Fatalf("result = %+v, want achieved via the complete second verdict", res)
		}
		if res.Reason != "done" {
			t.Errorf("Reason = %q, want %q (the truncated partial verdict %q must never be parsed)", res.Reason, "done", "premature")
		}
	})
}

// truncatingAtCallProvider serves scripted turns like scriptedProvider but
// returns a dyingStream — dieEvents then dieErr, never EventDone — for call
// number dieOn (1-indexed).
type truncatingAtCallProvider struct {
	name      string
	turns     [][]provider.Event
	dieOn     int
	dieEvents []provider.Event
	dieErr    error
	call      int
}

func (p *truncatingAtCallProvider) Name() string { return p.name }

func (p *truncatingAtCallProvider) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	p.call++
	if p.call == p.dieOn {
		return &dyingStream{events: p.dieEvents, err: p.dieErr}, nil
	}
	if p.call-1 >= len(p.turns) {
		return nil, io.ErrUnexpectedEOF
	}
	return &scriptedStream{events: p.turns[p.call-1]}, nil
}

// TestCompactTruncatedSummaryNeverFolds: Compact's summarizer-stream loop
// shared the evaluator's errors.Is(err, io.EOF) bug above — a summarization
// stream cut mid-summary parsed the partial text as THE summary and folded
// real history into it: silent data loss, the worst possible outcome for a
// best-effort maintenance operation. A truncated summarizer stream must
// fail the Compact call (the turn proceeds uncompacted) and leave history
// untouched.
func TestCompactTruncatedSummaryNeverFolds(t *testing.T) {
	prov := &truncatingAtCallProvider{
		name: "test",
		turns: [][]provider.Event{
			compactTurn("one", provider.Usage{InputTokens: 10, OutputTokens: 5}),
			compactTurn("two", provider.Usage{InputTokens: 20, OutputTokens: 5}),
			compactTurn("three", provider.Usage{InputTokens: 30, OutputTokens: 5}),
		},
		dieOn:     4, // the summarization call
		dieEvents: []provider.Event{{Type: provider.EventTextDelta, Text: "PARTIAL SUM"}},
		dieErr:    truncatedProviderErr(),
	}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 3)
	before := len(s.History())

	_, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err == nil {
		t.Fatal("Compact with a truncated summarizer stream succeeded, want error")
	}
	after := s.History()
	if len(after) != before {
		t.Fatalf("history = %d messages after failed compact, want %d untouched", len(after), before)
	}
	for _, m := range after {
		if strings.Contains(m.Parts.Text(), "PARTIAL SUM") {
			t.Fatal("partial summary text entered history despite the truncation")
		}
	}
}
