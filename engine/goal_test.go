package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// sessionErrorMessages returns the message property of every session.error
// event recorded by hooks, in order.
func sessionErrorMessages(t *testing.T, hooks *fakeHooks) []string {
	t.Helper()
	var msgs []string
	for _, ev := range hooks.events {
		if ev.Type != plugin.EventSessionError {
			continue
		}
		var props plugin.SessionErrorProperties
		if err := json.Unmarshal(ev.Properties, &props); err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, props.Message)
	}
	return msgs
}

// goalProvider serves both the worker model and the evaluator model from one
// registry entry. It keys the two apart by the presence of tools: the worker
// loop always offers built-in tools, while the goal evaluator's one-shot
// request is deliberately tool-less. Each side is scripted independently.
type goalProvider struct {
	worker     [][]provider.Event
	eval       [][]provider.Event
	wi, ei     int
	requests   []*provider.Request
	failCtx    bool  // when true, honor ctx cancellation in Stream
	failWorker error // when set, every worker call fails with this error

	// workerErrN, when > 0, makes the first workerErrN worker calls fail with
	// workerErr instead of consuming a scripted turn — a fake transient
	// provider failure (rate limit, momentary 5xx) so the retry path can be
	// exercised deterministically. Each failing call still counts against
	// wi's position: the scripted worker turns are consumed only once the
	// failures are exhausted.
	workerErrN   int
	workerErrHit int
	workerErr    error

	// workerCall counts every worker (tool-bearing) Stream call made so far
	// (1-indexed once incremented). failWorkerCall, when > 0, makes exactly
	// that call fail with workerErr instead of consuming a scripted turn or
	// checking workerErrN — used to simulate a provider error on a LATER
	// model call within the same Prompt invocation, i.e. after an earlier
	// call in that attempt already executed a tool.
	workerCall     int
	failWorkerCall int
}

func (p *goalProvider) Name() string { return "test" }

func (p *goalProvider) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if p.failCtx {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if p.failWorker != nil && len(req.Tools) != 0 {
		return nil, p.failWorker
	}
	p.requests = append(p.requests, req)
	if len(req.Tools) == 0 {
		// Evaluator request (tool-less).
		if p.ei >= len(p.eval) {
			return &scriptedStream{}, nil
		}
		ev := p.eval[p.ei]
		p.ei++
		return &scriptedStream{events: ev}, nil
	}
	p.workerCall++
	if p.failWorkerCall > 0 && p.workerCall == p.failWorkerCall {
		err := p.workerErr
		if err == nil {
			err = errors.New("fake transient provider error")
		}
		return nil, err
	}
	if p.workerErrHit < p.workerErrN {
		p.workerErrHit++
		err := p.workerErr
		if err == nil {
			err = errors.New("fake transient provider error")
		}
		return nil, err
	}
	if p.wi >= len(p.worker) {
		return &scriptedStream{}, nil
	}
	ev := p.worker[p.wi]
	p.wi++
	return &scriptedStream{events: ev}, nil
}

// evalTurn is a complete evaluator reply carrying a single text block.
func evalTurn(text string) []provider.Event {
	msg := &message.Message{ID: "msg_eval", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: text}}}
	return []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}}
}

// goalSession builds a session for goal-loop tests. An optional Hooks
// (fakeHooks in practice) may be passed to observe emitted plugin events.
func goalSession(t *testing.T, prov provider.Provider, dir string, hooks ...Hooks) *Session {
	t.Helper()
	var h Hooks
	if len(hooks) > 0 {
		h = hooks[0]
	}
	return NewSession(Config{
		Providers:    provider.Registry{prov.Name(): prov},
		Model:        message.ModelRef{Provider: prov.Name(), Model: "m1"},
		System:       []string{"base"},
		SessionDir:   dir,
		Instructions: &InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
		Hooks:        h,
	})
}

var evalModel = message.ModelRef{Provider: "test", Model: "eval"}

func TestPursueGoalAchievedSecondTurn(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "working on it"}),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "all done"}),
		},
		eval: [][]provider.Event{
			evalTurn("NOT MET: the summary is missing"),
			evalTurn("MET: the summary is present"),
		},
	}
	var evs []Event
	s := goalSession(t, prov, t.TempDir())
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	res, err := s.PursueGoal(context.Background(), "write a summary", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Achieved || res.Turns != 2 {
		t.Fatalf("result = %+v, want achieved in 2 turns", res)
	}
	if !strings.Contains(res.Reason, "present") {
		t.Errorf("reason = %q", res.Reason)
	}

	// History: user(condition), asst, user(guidance), asst.
	h := s.History()
	if len(h) != 4 {
		t.Fatalf("history len = %d: %+v", len(h), h)
	}
	// The second directive (the guidance message) must carry the evaluator reason.
	guidance := h[2].Parts.Text()
	if h[2].Role != message.RoleUser || !strings.Contains(guidance, "the summary is missing") {
		t.Errorf("guidance message = %q, want it to include the NOT MET reason", guidance)
	}

	// Goal events, in order.
	var got []string
	for _, ev := range evs {
		if strings.HasPrefix(ev.Type, "goal.") {
			got = append(got, ev.Type)
		}
	}
	want := []string{EventGoalSet, EventGoalEval, EventGoalEval, EventGoalAchieved}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("goal events = %v, want %v", got, want)
	}

	// Evaluator requests are tool-less and use the evaluator model.
	var evalReqs int
	for _, rq := range prov.requests {
		if len(rq.Tools) == 0 {
			evalReqs++
			if rq.Model != evalModel {
				t.Errorf("evaluator request model = %v, want %v", rq.Model, evalModel)
			}
			if rq.MaxTokens != 256 {
				t.Errorf("evaluator MaxTokens = %d, want 256", rq.MaxTokens)
			}
		}
	}
	if evalReqs != 2 {
		t.Errorf("evaluator requests = %d, want 2", evalReqs)
	}
}

// TestPursueGoalWorkerReasoningEmptyProviderData is the round-2 forensic
// regression guard reconstructed at the goal-loop level: the actual shape
// the incident logs show (two complete goal-supervised turns, then death
// mid-turn with "json: error calling MarshalJSON for type json.RawMessage:
// unexpected end of JSON input", surfacing as goal.stalled on every retry
// attempt because the same poisoned in-memory history got resent
// unchanged). The worker's assistant message here carries a Reasoning part
// with a present-but-zero-length provider_data entry — the map-indirected
// twin of the ToolCall.Arguments footgun #42 fixed, left unguarded by that
// fix (see message.ProviderData's doc comment) — appended mid-loop, exactly
// where the incident sessions died. Before the fix this turn's own
// s.append (persistMessage's json.Marshal) failed, and — because that
// failure is swallowed into PersistErr rather than returned — the *next*
// worker call re-sent the same now-poisoned in-memory history to the
// provider's transcoder, which is where the incident's observed error
// actually surfaced. The goal must complete normally, not stall.
func TestPursueGoalWorkerReasoningEmptyProviderData(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn,
				&message.Reasoning{
					Text:         "thinking it through",
					ProviderData: message.ProviderData{"test": json.RawMessage{}},
				},
				&message.Text{Text: "working on it"}),
			asstTurn(provider.StopEndTurn,
				&message.Reasoning{
					Text:         "thinking some more",
					ProviderData: message.ProviderData{"test": json.RawMessage{}},
				},
				&message.Text{Text: "all done"}),
		},
		eval: [][]provider.Event{
			evalTurn("NOT MET: the summary is missing"),
			evalTurn("MET: the summary is present"),
		},
	}
	s := goalSession(t, prov, t.TempDir())

	res, err := s.PursueGoal(context.Background(), "write a summary", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if !res.Achieved || res.Turns != 2 {
		t.Fatalf("result = %+v, want achieved in 2 turns", res)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v, want nil", err)
	}

	loaded, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got, want := historyJSON(t, loaded.History()), historyJSON(t, s.History()); got != want {
		t.Errorf("loaded history = %s\nwant %s", got, want)
	}
}

func TestPursueGoalRequiresEvaluator(t *testing.T) {
	prov := &goalProvider{}
	hooks := &fakeHooks{}
	s := goalSession(t, prov, t.TempDir(), hooks)
	_, err := s.PursueGoal(context.Background(), "do it", GoalOptions{})
	if err == nil {
		t.Fatal("PursueGoal with zero evaluator succeeded, want error")
	}
	if msgs := sessionErrorMessages(t, hooks); len(msgs) != 1 || msgs[0] != err.Error() {
		t.Errorf("session.error messages = %v, want [%q]", msgs, err.Error())
	}
}

// TestPursueGoalRegisterErrorEmitsSessionError covers the goal-loop-specific
// error path that never reaches Prompt: RegisterGoal failing because a goal
// is already active.
func TestPursueGoalRegisterErrorEmitsSessionError(t *testing.T) {
	prov := &goalProvider{}
	hooks := &fakeHooks{}
	s := goalSession(t, prov, t.TempDir(), hooks)
	if err := s.RegisterGoal("already running"); err != nil {
		t.Fatal(err)
	}

	_, err := s.PursueGoal(context.Background(), "do it", GoalOptions{Evaluator: evalModel})
	if err == nil {
		t.Fatal("PursueGoal with a goal already active succeeded, want error")
	}
	if msgs := sessionErrorMessages(t, hooks); len(msgs) != 1 || msgs[0] != err.Error() {
		t.Errorf("session.error messages = %v, want [%q]", msgs, err.Error())
	}
}

func TestPursueGoalMaxTurns(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "try 1"}),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "try 2"}),
		},
		eval: [][]provider.Event{
			evalTurn("NOT MET: nope"),
			evalTurn("NOT MET: still nope"),
		},
	}
	s := goalSession(t, prov, t.TempDir())
	res, err := s.PursueGoal(context.Background(), "impossible", GoalOptions{MaxTurns: 2, Evaluator: evalModel})
	if err != nil {
		t.Fatal(err)
	}
	if res.Achieved {
		t.Fatalf("result = %+v, want not achieved", res)
	}
	if res.Turns != 2 || res.Reason != "max turns" {
		t.Errorf("result = %+v, want turns=2 reason=%q", res, "max turns")
	}
	// A goal that exhausted its turns without achieving stays active for resume.
	if cond, ok := s.ActiveGoal(); !ok || cond != "impossible" {
		t.Errorf("ActiveGoal = %q, %v; want the condition, active", cond, ok)
	}
}

func TestPursueGoalUnparseableTwice(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "work"}),
		},
		eval: [][]provider.Event{
			evalTurn("I am not sure about this"),
			evalTurn("still rambling with no verdict"),
		},
	}
	hooks := &fakeHooks{}
	s := goalSession(t, prov, t.TempDir(), hooks)
	_, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
	if err == nil {
		t.Fatal("PursueGoal with two unparseable evaluations succeeded, want error")
	}
	// The worker turn (Prompt) succeeded; the error comes from evaluateGoal,
	// a goal-loop-specific error path that never goes through Prompt's own
	// session.error emission. Exactly one session.error, for this error.
	if msgs := sessionErrorMessages(t, hooks); len(msgs) != 1 || msgs[0] != err.Error() {
		t.Errorf("session.error messages = %v, want [%q]", msgs, err.Error())
	}
}

// TestPursueGoalUnparseableTwiceClearsGoal reproduces the round-3 forensic
// finding directly (ses_01kx3ts0pjfap950bmr9b2js0b.jsonl): the worker turn
// succeeds, but the evaluator returns unparseable output twice in a row.
// Before the fix, PursueGoal returned that error bare and left the goal
// active forever — turns=0, no goal.eval, nothing durable explaining the
// silence beyond a single session.error. The evaluator-failure edge must
// obey the exact same no-zombie guarantee as a permanently failing worker
// turn: clear the goal, carrying the error as the reason, before returning.
func TestPursueGoalUnparseableTwiceClearsGoal(t *testing.T) {
	dir := t.TempDir()
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "work"}),
		},
		eval: [][]provider.Event{
			evalTurn("I am not sure about this"),
			evalTurn("still rambling with no verdict"),
		},
	}
	var evs []Event
	s := goalSession(t, prov, dir)
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	_, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
	if err == nil {
		t.Fatal("PursueGoal with two unparseable evaluations succeeded, want error")
	}

	// No zombie: the goal must no longer be active in memory.
	if cond, ok := s.ActiveGoal(); ok {
		t.Fatalf("ActiveGoal = %q, still active after permanent evaluator failure — zombie goal", cond)
	}

	var sawCleared bool
	for _, ev := range evs {
		if ev.Type == EventGoalCleared {
			sawCleared = true
			if !strings.Contains(ev.GoalReason, err.Error()) && !strings.Contains(ev.GoalReason, "unparseable") {
				t.Errorf("goal.cleared GoalReason = %q, want it to carry the evaluator error", ev.GoalReason)
			}
		}
		if ev.Type == EventGoalAchieved {
			t.Error("goal.achieved emitted after an evaluator failure, want none")
		}
	}
	if !sawCleared {
		t.Fatal("no goal.cleared event emitted after permanent evaluator failure")
	}

	// Nor on disk: a resumed session must not see an active goal either —
	// the exact check that would have caught ses_01kx3ts0pjfap950bmr9b2js0b
	// staying active forever.
	loaded, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cond, ok := loaded.ActiveGoal(); ok {
		t.Errorf("resumed ActiveGoal = %q, active after permanent evaluator failure — zombie goal survives reload", cond)
	}
}

func TestPursueGoalUnparseableThenRecovers(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "work"}),
		},
		eval: [][]provider.Event{
			evalTurn("hmm, unclear"),        // unparseable
			evalTurn("MET: looks good now"), // retry parses
		},
	}
	s := goalSession(t, prov, t.TempDir())
	res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Achieved || res.Turns != 1 {
		t.Fatalf("result = %+v, want achieved in 1 turn after a retry", res)
	}
}

// TestPursueGoalWorkerFailureEmitsOnce originally covered a single
// (non-cancellation) provider failure on the worker turn Prompt call inside
// the goal loop, asserting that Prompt's own session.error emission was the
// only one and PursueGoal returned that same error immediately. That premise
// no longer holds: PursueGoal now retries a failing worker turn up to
// goalWorkerRetries additional times (promptTurnWithRetry) before giving up,
// and a single failure is retried rather than returned. What the retry fix
// preserves is the *permanent*-failure case — every attempt exhausted — which
// must still (a) return an error that wraps the underlying provider error,
// and (b) never leave a zombie active goal. This test now exercises that
// case with goalProvider's persistent failWorker (every worker call fails,
// standing in for a fault that never clears — as opposed to workerErrN's
// bounded, eventually-recovering failures used by the retry test below), and
// asserts the resulting session.error count precisely rather than assuming
// "once".
//
// Run inside a synctest bubble: a permanent failure runs the full
// goalWorkerRetries+1 attempts, waiting the real backoff schedule between
// them (see TestPursueGoalRetriesTransientWorkerError) — free on fake time,
// costly on the wall clock (see AGENTS.md on time.Sleep-free tests).
func TestPursueGoalWorkerFailureEmitsOnce(t *testing.T) {
	workerErr := errors.New("worker provider exploded")
	hooks := &fakeHooks{}
	var s *Session
	var err error
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{failWorker: workerErr}
		s = goalSession(t, prov, t.TempDir(), hooks)
		_, err = s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
	})
	if !errors.Is(err, workerErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, workerErr)
	}

	// No zombie: every attempt failed, so the goal must have been cleared.
	if cond, ok := s.ActiveGoal(); ok {
		t.Fatalf("ActiveGoal = %q, still active after permanent failure — zombie goal", cond)
	}

	// promptTurnWithRetry makes goalWorkerRetries+1 attempts total (the
	// initial try plus goalWorkerRetries retries) before giving up — nothing
	// here executes a tool call (failWorker fails Stream itself, before any
	// tool runs), so the non-idempotency early-stop never triggers and all
	// goalWorkerRetries+1 attempts run. Each attempt is a full s.Prompt call,
	// and Prompt's own streamTurn-error path (engine.go) calls
	// emitSessionError once per failing call; PursueGoal's retry/clear code
	// does not additionally call emitSessionError itself for this path (see
	// the loop in PursueGoal and promptTurnWithRetry) — the clear only
	// journals goal.cleared, a distinct event. So the total session.error
	// count for a permanent failure is exactly one per attempt:
	// goalWorkerRetries+1, all carrying the same underlying error text.
	const wantEmits = goalWorkerRetries + 1
	msgs := sessionErrorMessages(t, hooks)
	if len(msgs) != wantEmits {
		t.Fatalf("session.error messages = %v (%d), want exactly %d (one per worker attempt)", msgs, len(msgs), wantEmits)
	}
	for _, m := range msgs {
		if m != workerErr.Error() {
			t.Errorf("session.error message = %q, want %q", m, workerErr.Error())
		}
	}
}

// TestPursueGoalRetriesTransientWorkerError reproduces the fix for the
// zombie-goal forensic finding: a worker-turn error (the fake providerâs
// first two calls fail, standing in for a transient provider hiccup such as
// a rate limit or a momentary 5xx while handling a large tool result) must
// not kill the loop outright. PursueGoal retries the same turn up to
// goalWorkerRetries times, recording a goal.stalled event for every failed
// attempt, and succeeds once the provider recovers.
//
// Run inside a synctest bubble: PursueGoal now waits a capped exponential
// backoff (goalRetryDelay) between retries, and the test asserts the exact
// elapsed schedule (1s + 4s) — on fake time that costs nothing, on the wall
// clock it would cost 5 real seconds per run of the suite (see AGENTS.md).
func TestPursueGoalRetriesTransientWorkerError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 2, // fails twice, succeeds on the 3rd (final) attempt
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
			t.Fatalf("PursueGoal error = %v, want nil (transient errors must be retried)", err)
		}
		if !res.Achieved || res.Turns != 1 {
			t.Fatalf("result = %+v, want achieved in 1 turn after retrying past the transient errors", res)
		}
		if want := time.Second + 4*time.Second; elapsed != want {
			t.Errorf("elapsed = %v, want exactly %v (the 1s, 4s backoff schedule between the two failed attempts)", elapsed, want)
		}

		var stalled int
		for _, ev := range evs {
			if ev.Type == EventGoalStalled {
				stalled++
				if ev.GoalReason == "" {
					t.Error("goal.stalled event missing GoalReason")
				}
			}
		}
		if stalled != 2 {
			t.Errorf("goal.stalled events = %d, want 2 (one per failed attempt)", stalled)
		}
		if cond, ok := s.ActiveGoal(); ok {
			t.Errorf("ActiveGoal = %q, active after achievement, want inactive", cond)
		}
	})
}

// TestPursueGoalWorkerFailsPermanentlyClearsGoal reproduces the "zombie goal"
// forensic finding directly: when a worker turn keeps failing past the
// retry budget, PursueGoal must not just return an error and leave the goal
// active forever (the bug that left ses_41813d5a411c2ba5's goal active for
// nearly 7 hours until a human manually cleared it). It must clear the goal,
// carrying the failure reason on the goal.cleared record/event, before
// returning.
func TestPursueGoalWorkerFailsPermanentlyClearsGoal(t *testing.T) {
	dir := t.TempDir()
	var s *Session
	var evs []Event
	var err error
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 1000, // never recovers
			workerErr:  errors.New("provider: connection reset by peer"),
		}
		s = goalSession(t, prov, dir)
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		_, err = s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
	})
	if err == nil {
		t.Fatal("PursueGoal with a permanently failing worker succeeded, want error")
	}
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Errorf("error = %v, want it to carry the underlying provider error", err)
	}

	// No zombie: the goal must no longer be active in memory.
	if cond, ok := s.ActiveGoal(); ok {
		t.Fatalf("ActiveGoal = %q, still active after permanent failure — zombie goal", cond)
	}

	var sawCleared bool
	var stalled int
	for _, ev := range evs {
		switch ev.Type {
		case EventGoalStalled:
			stalled++
		case EventGoalCleared:
			sawCleared = true
			if !strings.Contains(ev.GoalReason, "connection reset by peer") {
				t.Errorf("goal.cleared GoalReason = %q, want it to carry the error", ev.GoalReason)
			}
		}
	}
	if !sawCleared {
		t.Fatal("no goal.cleared event emitted after permanent worker failure")
	}
	if stalled != goalWorkerRetries+1 {
		t.Errorf("goal.stalled events = %d, want %d (one per attempt)", stalled, goalWorkerRetries+1)
	}

	// Nor on disk: a resumed session must not see an active goal either.
	loaded, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cond, ok := loaded.ActiveGoal(); ok {
		t.Errorf("resumed ActiveGoal = %q, active after permanent failure — zombie goal survives reload", cond)
	}
}

func TestPursueGoalContextCancel(t *testing.T) {
	prov := &goalProvider{
		failCtx: true,
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "work"}),
		},
		eval: [][]provider.Event{evalTurn("MET: fine")},
	}
	hooks := &fakeHooks{}
	s := goalSession(t, prov, t.TempDir(), hooks)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.PursueGoal(ctx, "cond", GoalOptions{Evaluator: evalModel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// context.Canceled is a deliberate stop (abort/drain/DELETE-goal), not a
	// failure — it must emit no session.error at all, from either Prompt (which
	// observes it first, inside streamTurn) or PursueGoal (which merely
	// propagates it). Asserting zero here is also what rules out the failure
	// mode where both layers treat the same cancellation as an error and each
	// emits its own session.error for it.
	if msgs := sessionErrorMessages(t, hooks); len(msgs) != 0 {
		t.Errorf("session.error messages = %v, want none for a cancelled context", msgs)
	}
}

func TestGoalRecordsResumeActive(t *testing.T) {
	dir := t.TempDir()
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "try 1"}),
		},
		eval: [][]provider.Event{evalTurn("NOT MET: keep going")},
	}
	s := goalSession(t, prov, dir)
	cfg := s.cfg
	// One turn, max 1 → not achieved, goal remains active in the log.
	if _, err := s.PursueGoal(context.Background(), "ongoing goal", GoalOptions{MaxTurns: 1, Evaluator: evalModel}); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	cond, ok := loaded.ActiveGoal()
	if !ok || cond != "ongoing goal" {
		t.Errorf("resumed ActiveGoal = %q, %v; want active with the condition", cond, ok)
	}
}

func TestGoalAchievedNotActiveOnResume(t *testing.T) {
	dir := t.TempDir()
	prov := &goalProvider{
		worker: [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "done"})},
		eval:   [][]provider.Event{evalTurn("MET: complete")},
	}
	s := goalSession(t, prov, dir)
	cfg := s.cfg
	if _, err := s.PursueGoal(context.Background(), "finished goal", GoalOptions{Evaluator: evalModel}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ActiveGoal(); ok {
		t.Error("ActiveGoal still active after achievement")
	}
	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.ActiveGoal(); ok {
		t.Error("resumed ActiveGoal active after achievement, want inactive")
	}
}

func TestClearBetweenRegisterAndLoop(t *testing.T) {
	// The round-3 race: registration is synchronous in the caller (e.g. the
	// HTTP handler), so a ClearGoal landing before the loop goroutine runs
	// must win — the loop then starts, sees the goal gone, and exits without
	// running a single turn or writing any post-cleared records.
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"} // any Prompt would fail: no turns scripted
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	})
	if err := s.RegisterGoal("the condition"); err != nil {
		t.Fatal(err)
	}
	if !s.ClearGoal() {
		t.Fatal("ClearGoal should clear a registered goal")
	}
	res, err := s.PursueGoal(context.Background(), "the condition", GoalOptions{
		Registered: true,
		Evaluator:  message.ModelRef{Provider: "test", Model: "judge"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Achieved || res.Turns != 0 || res.Reason != "goal cleared" {
		t.Fatalf("result = %+v, want cleared with zero turns", res)
	}
	if len(prov.requests) != 0 {
		t.Fatalf("provider saw %d requests, want 0", len(prov.requests))
	}
	// Log ends set -> cleared with nothing after.
	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, "goal.set") || !strings.Contains(log, "goal.cleared") {
		t.Fatalf("log missing set/cleared records: %s", log)
	}
	if strings.Contains(log, "goal.eval") || strings.Contains(log, "goal.achieved") {
		t.Fatalf("log has post-clear goal records: %s", log)
	}
}

func TestRegisterGoalRejectsSecondActive(t *testing.T) {
	s := NewSession(Config{})
	if err := s.RegisterGoal("one"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterGoal("two"); err == nil {
		t.Fatal("second RegisterGoal should error while a goal is active")
	}
}

func TestClearGoalRecordsAndResets(t *testing.T) {
	dir := t.TempDir()
	prov := &goalProvider{
		worker: [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "try"})},
		eval:   [][]provider.Event{evalTurn("NOT MET: nope")},
	}
	s := goalSession(t, prov, dir)
	cfg := s.cfg
	var cleared int
	s.cfg.OnEvent = func(ev Event) {
		if ev.Type == EventGoalCleared {
			cleared++
		}
	}
	if _, err := s.PursueGoal(context.Background(), "goalx", GoalOptions{MaxTurns: 1, Evaluator: evalModel}); err != nil {
		t.Fatal(err)
	}
	if !s.ClearGoal() {
		t.Fatal("ClearGoal returned false for an active goal")
	}
	if cleared != 1 {
		t.Errorf("goal.cleared events = %d, want 1", cleared)
	}
	if _, ok := s.ActiveGoal(); ok {
		t.Error("ActiveGoal still active after ClearGoal")
	}
	// Idempotent: a second clear does nothing.
	if s.ClearGoal() {
		t.Error("second ClearGoal returned true, want false (no active goal)")
	}
	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.ActiveGoal(); ok {
		t.Error("resumed ActiveGoal active after clear, want inactive")
	}
}

// blockingEvalProvider serves the worker turn immediately but parks the
// evaluator's Stream call on a channel released by the test, so a test can
// race ClearGoal against an in-flight evaluation. entered is closed the
// moment the evaluator request arrives, letting the test know it is safe to
// call ClearGoal.
type blockingEvalProvider struct {
	worker  [][]provider.Event
	wi      int
	entered chan struct{}
	release chan struct{}
	evalOut string

	// evalErr, when set, makes the evaluator call fail with this error once
	// released instead of returning evalOut's scripted verdict — used to
	// race a concurrent ClearGoal against an evaluator call that then fails
	// with a genuine (non-cancellation) provider error.
	evalErr error

	once sync.Once
}

func (p *blockingEvalProvider) Name() string { return "test" }

func (p *blockingEvalProvider) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if len(req.Tools) == 0 {
		p.once.Do(func() { close(p.entered) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if p.evalErr != nil {
			return nil, p.evalErr
		}
		return &scriptedStream{events: evalTurn(p.evalOut)}, nil
	}
	ev := p.worker[p.wi]
	p.wi++
	return &scriptedStream{events: ev}, nil
}

// TestClearGoalDuringPendingEvaluationIsCleanStop reproduces the concurrency
// finding: a ClearGoal (DELETE /goal) racing an in-flight evaluation must not
// let that evaluation's verdict land in the journal after goal.cleared, and
// PursueGoal must treat the race as a clean stop rather than an achievement.
func TestClearGoalDuringPendingEvaluationIsCleanStop(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	prov := &blockingEvalProvider{
		worker:  [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "working"})},
		entered: entered,
		release: release,
		evalOut: "MET: looks done",
	}
	s := goalSession(t, prov, dir)

	type outcome struct {
		res *GoalResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := s.PursueGoal(context.Background(), "goalx", GoalOptions{Evaluator: evalModel})
		done <- outcome{res, err}
	}()

	<-entered // the evaluation is in flight, blocked on release

	if !s.ClearGoal() {
		t.Fatal("ClearGoal returned false for an active goal")
	}

	releaseOnce.Do(func() { close(release) }) // let the pending MET verdict land

	out := <-done
	if out.err != nil {
		t.Fatalf("PursueGoal error = %v", out.err)
	}
	if out.res.Achieved {
		t.Fatalf("result = %+v, want a clean stop (Achieved=false) since the goal was cleared mid-evaluation", out.res)
	}

	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, `"type":"goal.cleared"`) {
		t.Fatalf("session log missing goal.cleared record:\n%s", log)
	}
	if strings.Contains(log, `"type":"goal.achieved"`) {
		t.Errorf("session log contains goal.achieved after a ClearGoal raced an in-flight evaluation:\n%s", log)
	}
	if strings.Contains(log, `"type":"goal.eval"`) {
		t.Errorf("session log contains a goal.eval record for an evaluation that resolved after ClearGoal:\n%s", log)
	}
}

// TestClearGoalDuringPendingEvaluatorFailureIsCleanStop reproduces the race
// asymmetry finding: a ClearGoal (DELETE /goal) racing an in-flight
// evaluator call that then fails with a genuine (non-cancellation) provider
// error must be treated exactly like the same race on the worker-turn path
// (see TestPursueGoalWorkerFailsPermanentlyClearsGoal's goalActiveWith guard)
// — a clean stop, with no error returned and no session.error emitted. The
// goal was already cleared by the time the evaluator failure is observed, so
// there is nothing left to clear and nothing to journal as a failure: a
// deliberately-cleared goal is not an error condition regardless of which
// half of the loop the clear raced with.
func TestClearGoalDuringPendingEvaluatorFailureIsCleanStop(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	evalErr := errors.New("evaluator provider exploded")
	prov := &blockingEvalProvider{
		worker:  [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "working"})},
		entered: entered,
		release: release,
		evalErr: evalErr,
	}
	hooks := &fakeHooks{}
	s := goalSession(t, prov, dir, hooks)

	type outcome struct {
		res *GoalResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := s.PursueGoal(context.Background(), "goalx", GoalOptions{Evaluator: evalModel})
		done <- outcome{res, err}
	}()

	<-entered // the evaluation is in flight, blocked on release

	if !s.ClearGoal() {
		t.Fatal("ClearGoal returned false for an active goal")
	}

	releaseOnce.Do(func() { close(release) }) // let the pending evaluator failure land

	out := <-done
	if out.err != nil {
		t.Fatalf("PursueGoal error = %v, want nil (goal already cleared concurrently, same as the worker-turn path)", out.err)
	}
	if out.res == nil || out.res.Achieved {
		t.Fatalf("result = %+v, want a clean stop (Achieved=false, no error)", out.res)
	}
	if out.res.Reason != "goal cleared" {
		t.Errorf("result.Reason = %q, want %q", out.res.Reason, "goal cleared")
	}

	if msgs := sessionErrorMessages(t, hooks); len(msgs) != 0 {
		t.Errorf("session.error emitted for an evaluator failure racing a concurrent ClearGoal: %v", msgs)
	}

	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, `"type":"goal.cleared"`) {
		t.Fatalf("session log missing goal.cleared record:\n%s", log)
	}
	if strings.Count(log, `"type":"goal.cleared"`) != 1 {
		t.Errorf("session log has more than one goal.cleared record (evaluator-failure path re-cleared an already-cleared goal):\n%s", log)
	}
	if strings.Contains(log, evalErr.Error()) {
		t.Errorf("session log carries the evaluator error even though the goal was already cleared:\n%s", log)
	}
}

// TestGoalEventsEmitWhileLockHeld pins the follow-up ordering fix:
// recordGoalEval, achieveGoal, and ClearGoal must emit their goal.* event
// while still holding s.mu, not after releasing it — otherwise the emitted
// event order (-> server journal/SSE seqs) can invert relative to the
// journaled log order under the same ClearGoal-vs-evaluation race exercised
// by TestClearGoalDuringPendingEvaluationIsCleanStop.
//
// A deterministic "red" reproduction of the actual inversion (two goroutines
// racing so that goroutine B's write+emit both complete while goroutine A is
// paused between its own write and its own emit) is not constructible from
// outside the package without adding a production-only test seam: which
// goroutine's emit callback runs first, once both are runnable, is a plain
// scheduler race with no available happens-before edge to pin from a test.
// Forcing it via a real second sync.Mutex.Lock from inside the OnEvent
// callback would risk a genuine runtime self-deadlock (fatal, not a clean
// test failure) if the fix were absent. So instead this pins the invariant
// that makes the inversion impossible in the first place — the event fires
// synchronously inside the same critical section as the log write — using a
// non-blocking sync.Mutex.TryLock from the test goroutine, which can never
// block or deadlock either way.
func TestGoalEventsEmitWhileLockHeld(t *testing.T) {
	tests := []struct {
		name string
		want string
		run  func(s *Session)
	}{
		{
			name: "recordGoalEval",
			want: EventGoalEval,
			run: func(s *Session) {
				if err := s.RegisterGoal("cond"); err != nil {
					t.Fatal(err)
				}
				s.recordGoalEval(true, "reason", 1)
			},
		},
		{
			name: "achieveGoal",
			want: EventGoalAchieved,
			run: func(s *Session) {
				if err := s.RegisterGoal("cond"); err != nil {
					t.Fatal(err)
				}
				s.achieveGoal("reason", 1)
			},
		},
		{
			name: "ClearGoal",
			want: EventGoalCleared,
			run: func(s *Session) {
				if err := s.RegisterGoal("cond"); err != nil {
					t.Fatal(err)
				}
				s.ClearGoal()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			s := goalSession(t, &goalProvider{}, dir)

			entered := make(chan struct{})
			release := make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			s.cfg.OnEvent = func(ev Event) {
				if ev.Type != tt.want {
					return
				}
				close(entered)
				<-release
			}

			done := make(chan struct{})
			go func() {
				tt.run(s)
				close(done)
			}()

			<-entered // the target event's handler is now parked inside emit

			// Non-blocking: if the write's lock had already been released
			// before emit (the pre-fix bug), TryLock succeeds here and must
			// be undone immediately so the emitter's later Unlock doesn't
			// panic on an unlocked mutex. If the fix holds, s.mu is still
			// held by the emitting goroutine and TryLock reports that
			// honestly — no blocking, no risk of deadlock, either way.
			if s.mu.TryLock() {
				s.mu.Unlock()
				t.Fatalf("s.mu was free while the %s event handler was still running: emit must happen inside the same critical section as the log write, not after its Unlock", tt.want)
			}

			close(release) // let the emitter finish
			<-done
		})
	}
}

// TestGoalRetryDelaySchedule pins the documented capped-exponential backoff
// schedule (1s, 4s, ...) as a pure function of the attempt number, so the
// schedule is asserted independent of PursueGoal's plumbing.
func TestGoalRetryDelaySchedule(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, time.Second},
		{2, 4 * time.Second},
		{3, 16 * time.Second},
		{4, goalRetryBackoffCap}, // 64s would exceed the cap
		{5, goalRetryBackoffCap},
	}
	for _, c := range cases {
		if got := goalRetryDelay(c.attempt); got != c.want {
			t.Errorf("goalRetryDelay(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

// TestPursueGoalRetryBackoffCancellable proves the backoff wait itself is
// context-cancellable: a cancellation arriving mid-wait ends PursueGoal
// immediately rather than sleeping out the rest of the schedule, and — since
// this is the same context.Canceled path documented for a worker-turn
// failure — the goal is left exactly as it was (still active), not cleared,
// matching the "deliberate abort" semantics the rest of the package
// establishes for a cancelled context.
//
// Run inside a synctest bubble: the cancelling goroutine fires its own timer
// at a fake-time offset chosen to land inside the second (4s) backoff wait,
// so the assertion on elapsed time is exact and costs no wall-clock time.
func TestPursueGoalRetryBackoffCancellable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 1000, // never recovers on its own
			workerErr:  errors.New("fake transient provider error"),
		}
		s := goalSession(t, prov, t.TempDir())

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go func() {
			timer := time.NewTimer(1500 * time.Millisecond) // 0.5s into the 4s wait
			defer timer.Stop()
			<-timer.C
			cancel()
		}()

		start := time.Now()
		_, err := s.PursueGoal(ctx, "cond", GoalOptions{Evaluator: evalModel})
		elapsed := time.Since(start)

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if want := 1500 * time.Millisecond; elapsed != want {
			t.Errorf("elapsed = %v, want exactly %v (cancelled mid-backoff, not the full 1s+4s schedule)", elapsed, want)
		}
		if cond, ok := s.ActiveGoal(); !ok || cond != "cond" {
			t.Errorf("ActiveGoal = %q, %v; want the goal left untouched (still active) after a cancelled backoff wait", cond, ok)
		}
	})
}

// TestPursueGoalNoRetryAfterToolExecution is the red-first test for the
// non-idempotency review finding: a retry re-issues the WHOLE directive
// (Prompt has no sub-turn resume point), so once a worker-turn attempt has
// already executed a tool call before failing on a later model call, retrying
// risks re-running that tool. PursueGoal must detect this (via the
// toolExecCount snapshot) and stop retrying immediately rather than reissue
// the directive — the failed attempt still counts (one goal.stalled record),
// but no second attempt, and no second tool execution, ever happens.
func TestPursueGoalNoRetryAfterToolExecution(t *testing.T) {
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

	if toolRuns != 1 {
		t.Errorf("tool executions = %d, want exactly 1 (a retry must not re-run it)", toolRuns)
	}
	if prov.workerCall != 2 {
		t.Errorf("worker provider calls = %d, want exactly 2 (no third attempt after a post-tool-execution failure)", prov.workerCall)
	}

	var stalled int
	for _, ev := range evs {
		if ev.Type == EventGoalStalled {
			stalled++
		}
	}
	if stalled != 1 {
		t.Errorf("goal.stalled events = %d, want 1 (retries stop after the first, post-tool-execution failure)", stalled)
	}
	if cond, ok := s.ActiveGoal(); ok {
		t.Errorf("ActiveGoal = %q, still active; want cleared (retries exhausted, non-idempotency risk)", cond)
	}
}
