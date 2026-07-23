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

	// evalErrN/evalErrHit/evalErr mirror workerErrN/workerErr on the
	// evaluator side: the first evalErrN evaluator (tool-less) calls fail
	// with evalErr instead of consuming a scripted verdict — a fake
	// evaluator-side provider failure (see engine's evaluateGoal/
	// runEvaluatorWithRetry and GitHub issue #61's evaluator-path mirror,
	// Round 6). Each failing call still counts against ei's position: the
	// scripted eval turns are consumed only once the failures are
	// exhausted.
	evalErrN   int
	evalErrHit int
	evalErr    error

	// onEvalStream, if set, is called synchronously at the very start of
	// every evaluator (tool-less) Stream call, with the 1-based call
	// number, before any scripted response or failure is decided. Since
	// Stream runs on the SAME goroutine as the PursueGoal call driving it,
	// a test can use this hook to fire a same-goroutine mutation
	// (UpdateGoal, ClearGoal) deterministically from inside an in-flight
	// evaluator call — reproducing the exact generation/active-state race
	// PursueGoal must handle, without any real concurrency (channels,
	// goroutines) at all. See
	// TestPursueGoalEvaluatorFailureDiscardedWhenGoalUpdatedMidCall and
	// TestClearGoalMidFailingBoundariesStopsCleanly.
	onEvalStream func(call int)
	evalCall     int
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
		p.evalCall++
		if p.onEvalStream != nil {
			p.onEvalStream(p.evalCall)
		}
		if p.evalErrHit < p.evalErrN {
			p.evalErrHit++
			err := p.evalErr
			if err == nil {
				err = errors.New("fake transient evaluator provider error")
			}
			return nil, err
		}
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

// TestPursueGoalUnparseableTwice pins the NEW (Round 6) contract for
// two consecutive unparseable evaluator replies: REWRITTEN from its original
// assertion (a bare error plus exactly one session.error) now that a single
// failed evaluator boundary is advisory, not fatal — see the package doc's
// "Round 6" section and TestPursueGoalUnparseableTwiceDoesNotClearGoal below.
// MaxTurns:1 bounds the loop at exactly the failed boundary so the test can
// observe its outcome without a second worker turn ever running.
//
// Run inside a synctest bubble: the failed boundary waits the short
// goalRetryDelay before PursueGoal returns (see AGENTS.md on synctest for
// all backoff timing).
func TestPursueGoalUnparseableTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{MaxTurns: 1, Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil (a single failed evaluator boundary is advisory)", err)
		}
		if res.Achieved || res.Reason != "max turns" {
			t.Errorf("result = %+v, want not achieved, reason \"max turns\"", res)
		}
		// A failed boundary must never emit session.error on its own — only the
		// terminal (goalEvalFailureLimit consecutive failures) does that.
		if msgs := sessionErrorMessages(t, hooks); len(msgs) != 0 {
			t.Errorf("session.error messages = %v, want none for a single failed boundary", msgs)
		}
	})
}

// TestPursueGoalUnparseableTwiceDoesNotClearGoal is the REWRITE of the
// former TestPursueGoalUnparseableTwiceClearsGoal (Round 3), which pinned the
// exact opposite of today's contract: it asserted that two consecutive
// unparseable evaluator replies cleared the goal, carrying the error as the
// reason. That was Round 3's fix for the ses_01kx3ts0pjfap950bmr9b2js0b
// zombie-goal forensic finding (worker turn succeeded, evaluator failed
// twice, goal stayed active forever) — but clearing on the FIRST failed
// boundary traded that incident for a new one: production fleet boxes died
// mid-healthy-work on a transient evaluator hiccup. Round 6 keeps
// the no-zombie guarantee (something durable always explains the state —
// see the goal.eval_failed record asserted below) without treating a single
// failed boundary as fatal: the goal stays ACTIVE, not cleared, and the loop
// keeps working. See TestPursueGoalEvaluatorTerminalAfterConsecutiveFailureLimit
// for where a real, sustained evaluator outage still does eventually clear.
//
// Run inside a synctest bubble: the failed boundary waits the short
// goalRetryDelay before PursueGoal returns (see AGENTS.md on synctest for
// all backoff timing).
func TestPursueGoalUnparseableTwiceDoesNotClearGoal(t *testing.T) {
	dir := t.TempDir()
	var s *Session
	var evs []Event
	var res *GoalResult
	var err error
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "work"}),
			},
			eval: [][]provider.Event{
				evalTurn("I am not sure about this"),
				evalTurn("still rambling with no verdict"),
			},
		}
		s = goalSession(t, prov, dir)
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		res, err = s.PursueGoal(context.Background(), "cond", GoalOptions{MaxTurns: 1, Evaluator: evalModel})
	})
	if err != nil {
		t.Fatalf("PursueGoal error = %v, want nil (a single failed evaluator boundary must not error)", err)
	}
	if res.Achieved || res.Reason != "max turns" {
		t.Errorf("result = %+v, want not achieved, reason \"max turns\"", res)
	}

	// No zombie, but ALSO no clear: the goal must still be active in memory.
	if cond, ok := s.ActiveGoal(); !ok || cond != "cond" {
		t.Fatalf("ActiveGoal = %q, %v; want still active after one failed evaluator boundary (advisory, not fatal)", cond, ok)
	}

	var sawCleared, sawEvalFailed bool
	for _, ev := range evs {
		switch ev.Type {
		case EventGoalCleared:
			sawCleared = true
		case EventGoalEvalFailed:
			sawEvalFailed = true
			if !strings.Contains(ev.GoalReason, "unparseable") {
				t.Errorf("goal.eval_failed GoalReason = %q, want it to carry the evaluator error", ev.GoalReason)
			}
			if ev.GoalEvalFailures != 1 {
				t.Errorf("goal.eval_failed GoalEvalFailures = %d, want 1", ev.GoalEvalFailures)
			}
		case EventGoalAchieved:
			t.Error("goal.achieved emitted after an evaluator failure, want none")
		}
	}
	if sawCleared {
		t.Error("goal.cleared emitted for a single failed evaluator boundary, want none — advisory, not fatal")
	}
	if !sawEvalFailed {
		t.Fatal("no goal.eval_failed event emitted — the boundary's failure must still be durably explained, just not fatally")
	}

	// The failed boundary is durably explained on disk too, and the goal is
	// still active there — the exact resumability check that would have
	// caught ses_01kx3ts0pjfap950bmr9b2js0b staying silently active forever,
	// now applied to the case where the goal SHOULD still be active.
	loaded, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cond, ok := loaded.ActiveGoal(); !ok || cond != "cond" {
		t.Errorf("resumed ActiveGoal = %q, %v; want active", cond, ok)
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
// TestPursueGoalWorkerFailureEmitsOnce is a Round 7 rewrite: the
// original concern — a permanently failing worker turn must call
// emitSessionError exactly once per attempt, never an extra time for
// PursueGoal's own exhaustion handling — is unchanged, since only the
// non-emitting tail of that handling (clear vs. park) changed. What
// changed: PursueGoal no longer clears the goal on exhaustion. It now
// wraps the underlying error in the *goalWorkerParkedError sentinel
// (IsGoalWorkerParked) and leaves the goal active — see
// TestPursueGoalWorkerFailsPermanentlyParksGoal for the full park-shape
// assertions (the journaled record, ActiveGoal after LoadSession, etc.);
// this test stays narrowly focused on the emit-count concern its name
// promises.
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
	if !IsGoalWorkerParked(err) {
		t.Fatalf("err = %v, want IsGoalWorkerParked", err)
	}

	// No zombie AND no permanent loss: every attempt failed, so the loop
	// exit-parked — the goal must still be active, ready to resume, not
	// cleared.
	if cond, ok := s.ActiveGoal(); !ok || cond != "cond" {
		t.Fatalf("ActiveGoal = %q, %v; want still active after a worker-turn park", cond, ok)
	}

	// promptTurnWithRetry makes goalWorkerRetries+1 attempts total (the
	// initial try plus goalWorkerRetries retries) before giving up — nothing
	// here executes a tool call (failWorker fails Stream itself, before any
	// tool runs), so the non-idempotency early-stop never triggers and all
	// goalWorkerRetries+1 attempts run. Each attempt is a full s.Prompt call,
	// and Prompt's own streamTurn-error path (engine.go) calls
	// emitSessionError once per failing call; PursueGoal's retry/park code
	// does not additionally call emitSessionError itself for this path (see
	// the loop in PursueGoal and promptTurnWithRetry) — the park only
	// journals goal.parked, a distinct event, same as the clear it replaced
	// did. So the total session.error count for a permanent failure is
	// exactly one per attempt: goalWorkerRetries+1, all carrying the same
	// underlying error text.
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

// TestPursueGoalWorkerFailsPermanentlyParksGoal is a Round 7
// rewrite of TestPursueGoalWorkerFailsPermanentlyClearsGoal. The original
// concern — a worker turn that keeps failing past the retry budget must
// never just return a bare error and leave the goal a silent zombie (the
// bug that left ses_41813d5a411c2ba5's goal active for nearly 7 hours until
// a human manually cleared it) — is unchanged; what changed is HOW
// PursueGoal now closes that hole. A production incident showed the
// original fix (clearing) traded one failure mode for another: OpenRouter
// 404s (a genuinely non-retryable, deterministic-tier failure) exhausted
// goalWorkerRetries in seconds, cleared the goal, and the box then sat idle
// for HOURS with nothing further ever resuming it — a human had to notice
// and manually re-POST /goal, the exact same "silently abandoned, only a
// human's attention fixes it" shape Round 2 was meant to close, just
// reached from the other direction. So PursueGoal must now wrap the error
// in the *goalWorkerParkedError sentinel (IsGoalWorkerParked), journal a
// durable, CLASSIFIED goal.parked record (never the raw provider error
// text — see classifyGoalWorkerError) instead of goal.cleared, and leave
// the goal fully active — both in memory and, after a reload, on disk —
// so an external caller (the server's activity-driven auto-arm) can resume
// it automatically the next time anything happens on the session.
func TestPursueGoalWorkerFailsPermanentlyParksGoal(t *testing.T) {
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
	if !IsGoalWorkerParked(err) {
		t.Fatalf("err = %v, want IsGoalWorkerParked", err)
	}

	// No zombie AND no permanent loss: the goal must still be active in
	// memory, ready to resume — not cleared.
	if cond, ok := s.ActiveGoal(); !ok || cond != "cond" {
		t.Fatalf("ActiveGoal = %q, %v; want still active after a worker-turn park", cond, ok)
	}

	var sawCleared bool
	var stalled int
	var parked int
	for _, ev := range evs {
		switch ev.Type {
		case EventGoalStalled:
			stalled++
		case EventGoalCleared:
			sawCleared = true
		case EventGoalParked:
			parked++
			// The goal.parked Reason is deliberately CLASSIFIED — see
			// classifyGoalWorkerError — never the raw provider error text
			// goal.stalled/goal.eval_failed carry: this record can outlive
			// the request that produced it (an operator-facing pause
			// presentation), so it must never echo provider detail that
			// has no fixed shape across vendors.
			if strings.Contains(ev.GoalReason, "connection reset by peer") {
				t.Errorf("goal.parked GoalReason = %q, must NOT carry the raw provider error text", ev.GoalReason)
			}
			if ev.GoalReason == "" {
				t.Error("goal.parked GoalReason is empty, want a classified reason")
			}
			if ev.GoalRetryable {
				t.Error("goal.parked GoalRetryable = true, want false (this failure is not provider-retryable)")
			}
			if ev.GoalAttempts != goalWorkerRetries+1 {
				t.Errorf("goal.parked GoalAttempts = %d, want %d", ev.GoalAttempts, goalWorkerRetries+1)
			}
		}
	}
	if sawCleared {
		t.Error("goal.cleared emitted — a worker-turn exhaustion must park, never clear")
	}
	if parked != 1 {
		t.Fatalf("goal.parked events = %d, want exactly 1", parked)
	}
	if stalled != goalWorkerRetries+1 {
		t.Errorf("goal.stalled events = %d, want %d (one per attempt)", stalled, goalWorkerRetries+1)
	}

	// Still active on disk too: a resumed session must see the same active
	// goal, not a zombie-free-but-abandoned clear.
	loaded, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cond, ok := loaded.ActiveGoal(); !ok || cond != "cond" {
		t.Errorf("resumed ActiveGoal = %q, %v; want still active after a worker-turn park", cond, ok)
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
	// race a concurrent ClearGoal (or UpdateGoal, see
	// TestStaleEvaluatorFailureDiscarded) against an evaluator call that then
	// fails with a genuine (non-cancellation) provider error.
	evalErr error

	// evalAfter, when set, scripts every evaluator call AFTER the first one —
	// the first call is always the one that blocks on entered/release and
	// then returns evalErr/evalOut as above; a test that needs the loop to
	// continue into a later turn (e.g. a stale-discard test, where the first
	// evaluator call's failure must be discarded and a second, later turn
	// must run for real) supplies the later turns' verdicts here instead of
	// hitting the same blocked/scripted behavior again. Existing callers that
	// never see a second evaluator call (they stop the loop from within the
	// first call, e.g. via a concurrent ClearGoal) are unaffected.
	evalAfter [][]provider.Event
	evalCall  int

	requests []*provider.Request

	once sync.Once
}

func (p *blockingEvalProvider) Name() string { return "test" }

func (p *blockingEvalProvider) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	p.requests = append(p.requests, req)
	if len(req.Tools) == 0 {
		p.evalCall++
		if p.evalCall > 1 && p.evalAfter != nil {
			ev := p.evalAfter[p.evalCall-2]
			return &scriptedStream{events: ev}, nil
		}
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

// TestClearGoalDuringPendingEvaluatorFailureIsCleanStop reproduces a
// ClearGoal (DELETE /goal) racing an in-flight evaluator call that then fails
// with a genuine (non-cancellation, non-retryable) provider error. REWRITTEN
// (Round 6) doc comment: this test's OLD framing ("must be treated
// exactly like the same race on the worker-turn path... a deliberately-
// cleared goal is not an error condition regardless of which half of the
// loop the clear raced with") described an era where an evaluator failure
// was otherwise just as fatal as a permanently-failing worker turn, and this
// test's only point was that the race with ClearGoal pre-empted that
// fatality. That symmetry no longer holds in general — an evaluator failure
// is now advisory below goalEvalFailureLimit consecutive boundaries, not
// fatal — but the race THIS test exercises is unaffected: recordGoalEvalFailed
// follows recordGoalEval's own no-op-when-inactive convention (see
// PursueGoal's evaluator-error branch), so a ClearGoal that wins the race
// still produces exactly the same clean stop it always did — nothing is
// journaled as a failed boundary for a goal that is already gone, and
// nothing is left to clear a second time.
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
	if strings.Contains(log, `"type":"goal.eval_failed"`) {
		t.Errorf("session log contains a goal.eval_failed record for a boundary that resolved after ClearGoal:\n%s", log)
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
				s.recordGoalEval(true, "reason", 1, s.snapshotGoal().gen)
			},
		},
		{
			name: "achieveGoal",
			want: EventGoalAchieved,
			run: func(s *Session) {
				if err := s.RegisterGoal("cond"); err != nil {
					t.Fatal(err)
				}
				s.achieveGoal("reason", 1, s.snapshotGoal().gen)
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
		{
			name: "recordGoalEvalFailed",
			want: EventGoalEvalFailed,
			run: func(s *Session) {
				if err := s.RegisterGoal("cond"); err != nil {
					t.Fatal(err)
				}
				s.recordGoalEvalFailed(errors.New("boom"), 1, 1, s.snapshotGoal().gen)
			},
		},
		{
			// recordGoalParked (Round 7) follows the exact same
			// emit-under-lock discipline as every other goal record — see
			// its doc comment.
			name: "recordGoalParked",
			want: EventGoalParked,
			run: func(s *Session) {
				if err := s.RegisterGoal("cond"); err != nil {
					t.Fatal(err)
				}
				s.recordGoalParked(1, 1, false, "", s.snapshotGoal().gen)
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
//
// Updated for Round 7: the gate's own behavior (stop retrying
// immediately) is unchanged — only what happens to the goal once retrying
// stops changed, from a clear to a park (see
// TestPursueGoalWorkerFailsPermanentlyParksGoal for that rewrite's full
// rationale); this test's tail now asserts the park shape instead.
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
	if !IsGoalWorkerParked(err) {
		t.Fatalf("err = %v, want IsGoalWorkerParked", err)
	}

	if toolRuns != 1 {
		t.Errorf("tool executions = %d, want exactly 1 (a retry must not re-run it)", toolRuns)
	}
	if prov.workerCall != 2 {
		t.Errorf("worker provider calls = %d, want exactly 2 (no third attempt after a post-tool-execution failure)", prov.workerCall)
	}

	var stalled, parked int
	for _, ev := range evs {
		switch ev.Type {
		case EventGoalStalled:
			stalled++
		case EventGoalParked:
			parked++
		}
	}
	if stalled != 1 {
		t.Errorf("goal.stalled events = %d, want 1 (retries stop after the first, post-tool-execution failure)", stalled)
	}
	if parked != 1 {
		t.Errorf("goal.parked events = %d, want 1", parked)
	}
	if cond, ok := s.ActiveGoal(); !ok || cond != "cond" {
		t.Errorf("ActiveGoal = %q, %v; want still active (parked, not cleared, after the non-idempotency gate stops retrying)", cond, ok)
	}
}

// TestGoalRetryableDelaySchedule pins the retryable-class backoff schedule
// (goalRetryableDelay) as a pure function, independent of the loop — the
// same style as TestGoalRetryDelaySchedule for the deterministic path.
func TestGoalRetryableDelaySchedule(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 40 * time.Second},
		{5, 80 * time.Second},
		{6, 160 * time.Second},
		{7, goalRetryableBackoffCap}, // 320s would exceed the 5-minute cap
		{8, goalRetryableBackoffCap},
	}
	for _, c := range cases {
		if got := goalRetryableDelay(c.attempt); got != c.want {
			t.Errorf("goalRetryableDelay(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

// TestGoalRetryableBackoffJitter proves goalRetryableBackoff applies "equal
// jitter" (half the base delay fixed, half randomized in [0, half)) using
// the goalJitterFunc test seam, so the schedule is exactly assertable at
// both ends of the random range instead of merely bounded.
func TestGoalRetryableBackoffJitter(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })

	goalJitterFunc = func(max time.Duration) time.Duration { return 0 }
	if got, want := goalRetryableBackoff(1), 2500*time.Millisecond; got != want {
		t.Errorf("goalRetryableBackoff(1) with zero jitter = %v, want %v (half of the 5s base)", got, want)
	}

	goalJitterFunc = func(max time.Duration) time.Duration { return max - 1 }
	if got, want := goalRetryableBackoff(1), 5*time.Second-1; got != want {
		t.Errorf("goalRetryableBackoff(1) with max jitter = %v, want %v (just under the full 5s base)", got, want)
	}
}

// retryableProviderErr builds a fake provider error marked retryable, as if
// an adapter had classified an Anthropic 529/overloaded_error (see
// provider/anthropic and GitHub issue #61).
func retryableProviderErr(class provider.RetryableClass) error {
	return provider.MarkRetryable(errors.New("anthropic: Overloaded (overloaded_error, HTTP 529)"), class)
}

// TestPursueGoalRetryableErrorLongBackoffThenRecovers is the red-first test
// for the goal-loop half of GitHub issue #61: a worker-turn error the
// provider adapter classified retryable (see provider.RetryableError) must
// get its OWN long-backoff budget (goalRetryableMaxAttempts), separate from
// — and not consuming — the ordinary deterministic goalWorkerRetries
// budget. Here the fake provider fails 5 times (more than
// goalWorkerRetries+1 == 3 would ever tolerate on the deterministic path)
// before succeeding, and the goal must still achieve normally.
func TestPursueGoalRetryableErrorLongBackoffThenRecovers(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })
	goalJitterFunc = func(max time.Duration) time.Duration { return 0 } // deterministic: exactly half the base delay

	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 5,
			workerErr:  retryableProviderErr(provider.RetryableOverloaded),
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
			t.Fatalf("PursueGoal error = %v, want nil (retryable errors get a long budget)", err)
		}
		if !res.Achieved || res.Turns != 1 {
			t.Fatalf("result = %+v, want achieved in 1 turn after retrying past 5 retryable errors", res)
		}

		var want time.Duration
		for attempt := 1; attempt <= 5; attempt++ {
			want += goalRetryableDelay(attempt) / 2 // zero jitter above halves each delay
		}
		if elapsed != want {
			t.Errorf("elapsed = %v, want exactly %v (the retryable backoff schedule for 5 failed attempts)", elapsed, want)
		}

		var stalled int
		for _, ev := range evs {
			if ev.Type == EventGoalStalled {
				stalled++
				if !ev.GoalRetryable {
					t.Errorf("goal.stalled event %d: GoalRetryable = false, want true", stalled)
				}
				if ev.GoalRetryableClass != string(provider.RetryableOverloaded) {
					t.Errorf("goal.stalled event %d: GoalRetryableClass = %q, want %q", stalled, ev.GoalRetryableClass, provider.RetryableOverloaded)
				}
				if !ev.GoalWaiting {
					t.Errorf("goal.stalled event %d: GoalWaiting = false, want true (budget not exhausted)", stalled)
				}
			}
		}
		if stalled != 5 {
			t.Errorf("goal.stalled events = %d, want 5 (one per failed attempt)", stalled)
		}
		if cond, ok := s.ActiveGoal(); ok {
			t.Errorf("ActiveGoal = %q, active after achievement, want inactive", cond)
		}
	})
}

// TestPursueGoalRetryableBudgetExhaustedParksInsteadOfClearing is a Round 7
// rewrite of GitHub issue #61's original deliverable-4 test. The
// original concern — once the retryable-class budget
// (goalRetryableMaxAttempts) is exhausted for a turn (a truly long outage),
// the goal must NOT be cleared into a permanently-dead stall requiring an
// operator re-POST — is unchanged. What changed is HOW it avoids that dead
// stall: issue #61's original fix parked by looping IN PLACE (an in-loop
// `continue` that retried the same directive on the next ordinary turn,
// never leaving PursueGoal — see goalRetryableExhaustedError's doc
// comment), so with MaxTurns set the loop only reached the ordinary "max
// turns" terminal after enough parked cycles. The worker-park rework supersedes that: the
// FIRST retryable-budget exhaustion now exit-parks immediately — the same
// terminal a deterministic-tier exhaustion reaches (see
// TestPursueGoalWorkerFailsPermanentlyParksGoal) — freeing the run slot
// instead of holding it for the rest of the outage (see the package doc's
// "Round 7" section for why: a queued prompt can now dispatch as a normal
// turn instead of only ever being injected into a doomed retry). So this
// test now asserts exactly ONE turn's worth of retryable attempts before
// PursueGoal returns the *goalWorkerParkedError sentinel — MaxTurns is no
// longer even reachable via repeated parking.
func TestPursueGoalRetryableBudgetExhaustedParksInsteadOfClearing(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })
	goalJitterFunc = func(max time.Duration) time.Duration { return 0 }

	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 1000, // never recovers within the test
			workerErr:  retryableProviderErr(provider.RetryableOverloaded),
		}
		var evs []Event
		s := goalSession(t, prov, t.TempDir())
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel, MaxTurns: 2})
		if res != nil {
			t.Fatalf("result = %+v, want nil (an exit-park returns an error, not a GoalResult)", res)
		}
		if !IsGoalWorkerParked(err) {
			t.Fatalf("err = %v, want IsGoalWorkerParked", err)
		}

		// The critical assertion: no zombie AND no dead stall. The goal
		// stays active — ready for an external caller to resume it — not
		// cleared the way a permanent deterministic failure used to be
		// (see TestPursueGoalWorkerFailsPermanentlyParksGoal, which now
		// parks too).
		if cond, ok := s.ActiveGoal(); !ok || cond != "cond" {
			t.Errorf("ActiveGoal = %q, %v; want the goal left ACTIVE for resume, not cleared", cond, ok)
		}

		var sawCleared bool
		var exhausted int
		var waiting int
		var parked int
		for _, ev := range evs {
			switch ev.Type {
			case EventGoalCleared:
				sawCleared = true
			case EventGoalStalled:
				if !ev.GoalRetryable {
					t.Errorf("goal.stalled event: GoalRetryable = false, want true")
				}
				if ev.GoalWaiting {
					waiting++
				} else {
					exhausted++
					if ev.GoalRetryableClass != string(provider.RetryableOverloaded) {
						t.Errorf("exhausted goal.stalled: GoalRetryableClass = %q, want %q", ev.GoalRetryableClass, provider.RetryableOverloaded)
					}
				}
			case EventGoalParked:
				parked++
				if !ev.GoalRetryable {
					t.Errorf("goal.parked event: GoalRetryable = false, want true")
				}
				if ev.GoalRetryableClass != string(provider.RetryableOverloaded) {
					t.Errorf("goal.parked event: GoalRetryableClass = %q, want %q", ev.GoalRetryableClass, provider.RetryableOverloaded)
				}
				if ev.GoalAttempts != goalRetryableMaxAttempts {
					t.Errorf("goal.parked event: GoalAttempts = %d, want %d", ev.GoalAttempts, goalRetryableMaxAttempts)
				}
				if ev.GoalReason == "" {
					t.Error("goal.parked GoalReason is empty, want a classified reason")
				}
			}
		}
		if sawCleared {
			t.Error("goal.cleared emitted — a retryable-budget exhaustion must park, never clear")
		}
		// Exactly one turn's worth: PursueGoal exits the instant the FIRST
		// turn's retryable budget exhausts, not after MaxTurns worth of
		// repeated in-loop parking (the pre-Round-7 shape this supersedes).
		if parked != 1 {
			t.Errorf("goal.parked events = %d, want exactly 1 (exit-park stops the loop on the first exhaustion)", parked)
		}
		if exhausted != 1 {
			t.Errorf("exhausted (non-waiting) goal.stalled events = %d, want 1 (one parked turn)", exhausted)
		}
		if waiting != goalRetryableMaxAttempts-1 {
			t.Errorf("waiting goal.stalled events = %d, want %d", waiting, goalRetryableMaxAttempts-1)
		}
		if prov.workerCall != goalRetryableMaxAttempts {
			t.Errorf("worker provider calls = %d, want %d (goalRetryableMaxAttempts for the one parked turn)", prov.workerCall, goalRetryableMaxAttempts)
		}
	})
}

// TestPursueGoalRetryableErrorAfterToolExecutionStillGated proves the
// non-idempotency gate (see promptTurnWithRetry's doc comment) applies
// regardless of error classification: a retryable-class error on a later
// model call, after an earlier call in the same attempt already executed a
// tool, must still stop retrying immediately rather than enter the long
// backoff — retrying would risk re-running the tool no matter how
// sympathetic the failure looks.
//
// Updated for Round 7: the gate itself is unchanged; the tail now
// asserts a park (not a clear) — and, since PursueGoal derives
// retryable/class straight from the returned error via provider.AsRetryable
// rather than from whether promptTurnWithRetry happened to wrap it in
// *goalRetryableExhaustedError (see PursueGoal's worker-turn error
// handling), the goal.parked record/event for THIS gated case still
// correctly reports GoalRetryable=true — the underlying failure really was
// provider-retryable-classified weather, even though the gate stopped
// retrying before the long retryable budget ever got a chance to exhaust on
// its own.
func TestPursueGoalRetryableErrorAfterToolExecutionStillGated(t *testing.T) {
	var toolRuns int
	testTool := Tool{
		Def: provider.ToolDef{Name: "test_tool", Description: "test", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			toolRuns++
			return message.Parts{&message.Text{Text: "ok"}}, nil
		},
	}
	prov := &goalProvider{
		failWorkerCall: 2,
		workerErr:      retryableProviderErr(provider.RetryableOverloaded),
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
		t.Fatal("PursueGoal succeeded, want error")
	}
	if !IsGoalWorkerParked(err) {
		t.Fatalf("err = %v, want IsGoalWorkerParked", err)
	}
	if toolRuns != 1 {
		t.Errorf("tool executions = %d, want exactly 1", toolRuns)
	}
	if prov.workerCall != 2 {
		t.Errorf("worker provider calls = %d, want exactly 2 (no retry after tool execution, even for a retryable error)", prov.workerCall)
	}
	var stalled int
	var parked int
	for _, ev := range evs {
		switch ev.Type {
		case EventGoalStalled:
			stalled++
		case EventGoalParked:
			parked++
			if !ev.GoalRetryable {
				t.Errorf("goal.parked event: GoalRetryable = false, want true (the underlying error was provider-retryable-classified)")
			}
			if ev.GoalRetryableClass != string(provider.RetryableOverloaded) {
				t.Errorf("goal.parked event: GoalRetryableClass = %q, want %q", ev.GoalRetryableClass, provider.RetryableOverloaded)
			}
			if ev.GoalAttempts != 1 {
				t.Errorf("goal.parked event: GoalAttempts = %d, want 1 (the gate stops after the first attempt)", ev.GoalAttempts)
			}
		}
	}
	if stalled != 1 {
		t.Errorf("goal.stalled events = %d, want 1", stalled)
	}
	if parked != 1 {
		t.Errorf("goal.parked events = %d, want 1", parked)
	}
	if cond, ok := s.ActiveGoal(); !ok || cond != "cond" {
		t.Errorf("ActiveGoal = %q, %v; want still active (parked, not cleared, after the non-idempotency gate stops retrying)", cond, ok)
	}
}
