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

// contextOverflowProvider always fails the worker's model call with a
// classified context-overflow *provider.Error, and calls the tool-less
// evaluator model normally (never reached by these tests: PursueGoal must
// give up on the worker turn before it ever asks the evaluator).
type contextOverflowProvider struct {
	err        *provider.Error
	workerCall int
}

func (p *contextOverflowProvider) Name() string { return "test" }

func (p *contextOverflowProvider) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	if len(req.Tools) == 0 {
		return &scriptedStream{}, nil // evaluator; unused if the worker fails fast, as expected
	}
	p.workerCall++
	return nil, p.err
}

func contextOverflowErr() *provider.Error {
	return &provider.Error{
		Kind:         provider.ErrKindContextOverflow,
		Raw:          "anthropic: prompt is too long: 205102 tokens > 200000 maximum (invalid_request_error, HTTP 400)",
		PromptTokens: 205102,
		TokenLimit:   200000,
	}
}

// TestPursueGoalContextOverflowFailsFastAndPermanently is the red-first
// regression test for issue #62's suggested layer 1: a context/prompt
// overflow is deterministic (retrying the identical, now-too-long request
// fails identically), so unlike an ordinary transient worker-turn error the
// goal loop must not retry it at all — no backoff wait, exactly one worker
// call, immediate clear with a distinct, clearly-named reason.
//
// Run inside a synctest bubble so the assertion that NO backoff elapsed
// (contrast TestPursueGoalRetriesTransientWorkerError, which asserts the 1s+4s
// schedule DOES elapse for an ordinary transient error) is exact and free.
func TestPursueGoalContextOverflowFailsFastAndPermanently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &contextOverflowProvider{err: contextOverflowErr()}
		var evs []Event
		s := goalSession(t, prov, t.TempDir())
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		start := time.Now()
		_, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("PursueGoal succeeded, want a context-overflow error")
		}
		if elapsed != 0 {
			t.Errorf("elapsed = %v, want 0 (no retry backoff for a deterministic context overflow)", elapsed)
		}
		if prov.workerCall != 1 {
			t.Errorf("worker provider calls = %d, want exactly 1 (no retry)", prov.workerCall)
		}
		wantMsg := "context exhausted: prompt 205102 tokens > limit 200000"
		if err.Error() != wantMsg {
			t.Errorf("PursueGoal error = %q, want %q", err.Error(), wantMsg)
		}
		if !provider.IsContextOverflow(err) {
			t.Errorf("PursueGoal error not classified as context overflow: %v", err)
		}

		if cond, ok := s.ActiveGoal(); ok {
			t.Fatalf("ActiveGoal = %q, still active; want cleared (no zombie goal)", cond)
		}

		var stalled, cleared int
		var clearedReason string
		for _, ev := range evs {
			switch ev.Type {
			case EventGoalStalled:
				stalled++
			case EventGoalCleared:
				cleared++
				clearedReason = ev.GoalReason
			}
		}
		if stalled != 1 {
			t.Errorf("goal.stalled events = %d, want exactly 1 (single fail-fast attempt)", stalled)
		}
		if cleared != 1 {
			t.Fatalf("goal.cleared events = %d, want exactly 1", cleared)
		}
		if !strings.Contains(clearedReason, "context exhausted") {
			t.Errorf("goal.cleared reason = %q, want it to name the context exhaustion clearly", clearedReason)
		}

		loaded, err := LoadSession(s.cfg, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cond, ok := loaded.ActiveGoal(); ok {
			t.Errorf("resumed ActiveGoal = %q, active after context overflow — zombie goal survives reload", cond)
		}
	})
}

// TestPromptContextOverflowErrorNamesLimit exercises the plain (non-goal)
// Prompt path: a context-overflow provider error must surface with the same
// clearly-named message — the server's last_turn.error for an ordinary
// prompt_async reads directly off Session.Prompt's returned error.
func TestPromptContextOverflowErrorNamesLimit(t *testing.T) {
	prov := &contextOverflowProvider{err: contextOverflowErr()}
	s := NewSession(Config{
		Providers:    provider.Registry{"test": prov},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		Instructions: &InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
	})
	_, err := s.Prompt(context.Background(), "hello")
	if err == nil {
		t.Fatal("Prompt succeeded, want a context-overflow error")
	}
	want := "context exhausted: prompt 205102 tokens > limit 200000"
	if err.Error() != want {
		t.Errorf("Prompt error = %q, want %q", err.Error(), want)
	}
}
