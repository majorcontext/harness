package engine

import (
	"context"
	"encoding/json"
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

// TestPursueGoalMixedClassInterleavingBudgetsIndependent proves the three
// retry counters in promptTurnWithRetry — deterministic, retryable
// ("weather"), and truncated — are tracked independently, not sharing one
// budget: an ALTERNATING sequence of failure classes within a single
// worker-turn attempt loop (truncated, overloaded, truncated) must be
// bounded by the SUM of the relevant tiers' per-class counters, each
// evaluated against its OWN ceiling, not have an unrelated class's failure
// count against — or exhaust — a different tier's budget. Neither the
// truncated tier (goalStreamTruncatedMaxAttempts == 3, so 2 failures leaves
// it short of exhaustion) nor the retryable tier (goalRetryableMaxAttempts
// == 12, so 1 failure is nowhere near exhaustion) is anywhere close to its
// own ceiling here, so recovery on the 4th attempt must succeed.
func TestPursueGoalMixedClassInterleavingBudgetsIndependent(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })
	goalJitterFunc = func(max time.Duration) time.Duration { return 0 }

	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 3,
			workerErrSeq: []error{
				truncatedProviderErr(),
				retryableProviderErr(provider.RetryableOverloaded),
				truncatedProviderErr(),
			},
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
			t.Fatalf("PursueGoal error = %v, want nil (each class's budget is far from exhausted)", err)
		}
		if !res.Achieved || res.Turns != 1 {
			t.Fatalf("result = %+v, want achieved in 1 turn after recovering from the interleaved failures", res)
		}

		// The independent-budget claim is exactly this schedule: the two
		// truncated failures each wait on the SHORT goalRetryDelay schedule
		// (attempt 1, then attempt 2 of the truncated tier alone — the
		// intervening overloaded failure does not advance it), and the one
		// overloaded failure waits on the jittered (here zeroed)
		// goalRetryableBackoff(1) — never the other tier's schedule, and
		// never a schedule keyed to the combined attempt count instead of
		// each tier's own.
		want := goalRetryDelay(1) + goalRetryableDelay(1)/2 + goalRetryDelay(2)
		if elapsed != want {
			t.Errorf("elapsed = %v, want exactly %v (goalRetryDelay(1) + goalRetryableDelay(1)/2 + goalRetryDelay(2))", elapsed, want)
		}

		var gotClasses []string
		for _, ev := range evs {
			if ev.Type != EventGoalStalled {
				continue
			}
			if !ev.GoalRetryable {
				t.Errorf("goal.stalled event: GoalRetryable = false, want true (class %q)", ev.GoalRetryableClass)
			}
			if !ev.GoalWaiting {
				t.Errorf("goal.stalled event: GoalWaiting = false, want true (no tier anywhere near exhausted)")
			}
			gotClasses = append(gotClasses, ev.GoalRetryableClass)
		}
		wantClasses := []string{
			string(provider.RetryableStreamTruncated),
			string(provider.RetryableOverloaded),
			string(provider.RetryableStreamTruncated),
		}
		if strings.Join(gotClasses, ",") != strings.Join(wantClasses, ",") {
			t.Errorf("goal.stalled classes = %v, want %v (the exact scripted interleaving)", gotClasses, wantClasses)
		}
	})
}

// TestPursueGoalTruncatedAfterToolExecutionParksImmediately mirrors
// TestPursueGoalRetryableErrorAfterToolExecutionStillGated for the
// truncated class specifically: the non-idempotency tool gate in
// promptTurnWithRetry (see its doc comment) pre-empts EVERY retry tier,
// including the truncated one added alongside the deterministic and
// retryable("weather") tiers — a truncated failure on a LATER model call
// within an attempt that already executed a tool must stop retrying
// immediately and park on attempt 1, never entering the truncated tier's
// own (short) backoff schedule.
func TestPursueGoalTruncatedAfterToolExecutionParksImmediately(t *testing.T) {
	var toolRuns int
	testTool := Tool{
		Def: provider.ToolDef{Name: "test_tool", Description: "test", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			toolRuns++
			return message.Parts{&message.Text{Text: "ok"}}, nil
		},
	}
	prov := &goalProvider{
		failWorkerCall: 2, // the SECOND worker call fails, after the first ran a tool
		workerErr:      truncatedProviderErr(),
		worker: [][]provider.Event{
			asstTurn(provider.StopToolUse, toolCall("tc1", "test_tool", `{}`)),
		},
	}
	var evs []Event
	s := NewSession(Config{
		Providers:    provider.Registry{prov.Name(): prov},
		Model:        message.ModelRef{Provider: prov.Name(), Model: "m1"},
		System:       []string{"base"},
		SessionDir:   t.TempDir(),
		Instructions: &InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
		Tools:        []Tool{testTool},
	})
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	_, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
	if err == nil {
		t.Fatal("PursueGoal succeeded, want error (the worker call after tool execution always fails)")
	}
	if !IsGoalWorkerParked(err) {
		t.Fatalf("err = %v, want IsGoalWorkerParked", err)
	}
	if toolRuns != 1 {
		t.Errorf("tool executions = %d, want exactly 1 (a retry must not re-run it)", toolRuns)
	}
	if prov.workerCall != 2 {
		t.Errorf("worker provider calls = %d, want exactly 2 (no third attempt after a post-tool-execution failure, even for the truncated class)", prov.workerCall)
	}

	var stalled, parked int
	for _, ev := range evs {
		switch ev.Type {
		case EventGoalStalled:
			stalled++
		case EventGoalParked:
			parked++
			if !ev.GoalRetryable {
				t.Errorf("goal.parked event: GoalRetryable = false, want true (stream truncation is a retryable class)")
			}
			if ev.GoalRetryableClass != string(provider.RetryableStreamTruncated) {
				t.Errorf("goal.parked event: GoalRetryableClass = %q, want %q", ev.GoalRetryableClass, provider.RetryableStreamTruncated)
			}
			if ev.GoalAttempts != 1 {
				t.Errorf("goal.parked event: GoalAttempts = %d, want 1 (the gate stops after the first attempt)", ev.GoalAttempts)
			}
		}
	}
	if stalled != 1 {
		t.Errorf("goal.stalled events = %d, want 1 (retries stop after the first, post-tool-execution failure)", stalled)
	}
	if parked != 1 {
		t.Errorf("goal.parked events = %d, want 1", parked)
	}
	if cond, ok := s.ActiveGoal(); !ok || cond != "cond" {
		t.Errorf("ActiveGoal = %q, %v; want still active (parked, not cleared) after the non-idempotency gate stops retrying", cond, ok)
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

// TestGoalEvaluatorHangingStreamCutByWatchdog: the idle-stream watchdog
// must guard the EVALUATOR's stream too, not just worker turns — a
// permanently silent evaluator stream otherwise wedges PursueGoal forever
// while holding the server's run slot: no goal.eval_failed, no turn.end,
// and the prompt queue never drains (review finding on the watchdog work;
// strictly worse than the wedge the watchdog exists to bound). Here eval
// call 1 hangs forever; the watchdog cuts it at the default 5m, the
// in-boundary retry consumes the scripted verdict, and the goal achieves.
func TestGoalEvaluatorHangingStreamCutByWatchdog(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })
	goalJitterFunc = func(max time.Duration) time.Duration { return 0 }

	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "did the work"}),
			},
			evalHangN: 1,
			eval: [][]provider.Event{
				evalTurn("MET: done"),
			},
		}
		s := goalSession(t, prov, t.TempDir())

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil (the watchdog must cut the hanging evaluator stream)", err)
		}
		if !res.Achieved || res.Reason != "done" {
			t.Fatalf("result = %+v, want achieved via the post-cut retry verdict", res)
		}
	})
}

// TestCompactHangingSummarizerCutByWatchdog: same guard for the compaction
// summarizer stream — maybeAutoCompact runs at the top of every Prompt, so
// a permanently silent summarizer stream would otherwise wedge the very
// turn it was trying to protect.
func TestCompactHangingSummarizerCutByWatchdog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &hangingSummarizerProvider{
			turns: [][]provider.Event{
				compactTurn("one", provider.Usage{InputTokens: 10, OutputTokens: 5}),
				compactTurn("two", provider.Usage{InputTokens: 20, OutputTokens: 5}),
				compactTurn("three", provider.Usage{InputTokens: 30, OutputTokens: 5}),
			},
			hangOn: 4, // the summarization call
		}
		s := NewSession(Config{
			Providers: provider.Registry{"test": prov},
			Model:     message.ModelRef{Provider: "test", Model: "m1"},
		})
		runTurns(t, s, 3)
		before := len(s.History())

		start := time.Now()
		_, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
		if err == nil {
			t.Fatal("Compact with a hanging summarizer stream succeeded, want a watchdog cut")
		}
		if got := time.Since(start); got != 5*time.Minute {
			t.Errorf("elapsed = %v, want exactly 5m (the default idle timeout)", got)
		}
		if got := len(s.History()); got != before {
			t.Fatalf("history = %d messages after failed compact, want %d untouched", got, before)
		}
	})
}

// hangingSummarizerProvider serves scripted turns, then returns a
// ctx-blocking hangingStream for call number hangOn (1-indexed).
type hangingSummarizerProvider struct {
	name   string
	turns  [][]provider.Event
	hangOn int
	call   int
}

func (p *hangingSummarizerProvider) Name() string { return "test" }

func (p *hangingSummarizerProvider) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	p.call++
	if p.call == p.hangOn {
		return &hangingStream{ctx: ctx}, nil
	}
	if p.call-1 >= len(p.turns) {
		return nil, io.ErrUnexpectedEOF
	}
	return &scriptedStream{events: p.turns[p.call-1]}, nil
}
