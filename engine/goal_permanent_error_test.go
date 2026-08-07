package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// permanentProviderErr builds a fake provider error marked permanent, as if
// an adapter (provider/anthropic's apiError) had classified it — mirrors
// retryableProviderErr's shape exactly. The message reproduces the
// NEP-5272 incident fingerprint verbatim, since these tests also assert
// that the classified goal.parked reason (see classifyGoalWorkerError)
// never leaks it.
func permanentProviderErr() error {
	return provider.MarkPermanent(errors.New("anthropic: messages.85: `tool_use` ids were found without `tool_result` blocks immediately after (invalid_request_error, HTTP 400)"))
}

// TestPursueGoalPermanentWorkerErrorParksAfterOneAttempt is the red-first
// regression test for NEP-5272's defect 1: a worker-turn error the adapter
// classifies provider.AsPermanent (an HTTP 400 invalid_request_error naming
// a structurally malformed request — the orphaned tool_use, in the
// production incident) must fail fast after exactly ONE attempt, no backoff
// wait, instead of burning the full goalWorkerRetries deterministic budget
// (3 identical, guaranteed-to-fail model calls, as production observed:
// "engine: goal worker turn parked after 3 deterministic-tier attempt(s)").
// Mirrors TestPursueGoalContextOverflowFailsFastAndPermanently's shape
// (elapsed == 0 inside a synctest bubble, exactly one worker call) but PARKS
// instead of clearing: unlike context overflow, a malformed-request shape
// might be fixed by something else entirely (e.g. NEP-5272's own
// orphan-tool-call repair) between now and a later resume, so the goal must
// stay resumable rather than being asserted permanently dead.
func TestPursueGoalPermanentWorkerErrorParksAfterOneAttempt(t *testing.T) {
	dir := t.TempDir()
	var s *Session
	var evs []Event
	var err error
	var elapsed time.Duration
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 1000, // never recovers
			workerErr:  permanentProviderErr(),
		}
		s = goalSession(t, prov, dir)
		s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

		start := time.Now()
		_, err = s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		elapsed = time.Since(start)

		if prov.workerCall != 1 {
			t.Errorf("worker provider calls = %d, want exactly 1 (no retry for a permanent error)", prov.workerCall)
		}
	})
	if err == nil {
		t.Fatal("PursueGoal with a permanent worker error succeeded, want error")
	}
	if elapsed != 0 {
		t.Errorf("elapsed = %v, want 0 (no retry backoff for a permanent error)", elapsed)
	}
	if !strings.Contains(err.Error(), "tool_use") {
		t.Errorf("error = %v, want it to carry the underlying provider error", err)
	}
	if !IsGoalWorkerParked(err) {
		t.Fatalf("err = %v, want IsGoalWorkerParked", err)
	}

	// Never a zombie AND never permanently lost: the goal must stay active,
	// ready to resume — parking, not clearing, since something else (the
	// orphan-tool-call repair, an operator edit) might fix the malformed
	// request shape before the next resume.
	if cond, ok := s.ActiveGoal(); !ok || cond != "cond" {
		t.Fatalf("ActiveGoal = %q, %v; want still active after a permanent-error park", cond, ok)
	}

	var sawCleared bool
	var stalled, parked int
	for _, ev := range evs {
		switch ev.Type {
		case EventGoalStalled:
			stalled++
		case EventGoalCleared:
			sawCleared = true
		case EventGoalParked:
			parked++
			if strings.Contains(ev.GoalReason, "tool_use") {
				t.Errorf("goal.parked GoalReason = %q, must NOT carry the raw provider error text", ev.GoalReason)
			}
			if ev.GoalReason == "" {
				t.Error("goal.parked GoalReason is empty, want a classified reason")
			}
			if ev.GoalRetryable {
				t.Error("goal.parked GoalRetryable = true, want false (a permanent error is not provider-retryable weather)")
			}
			if ev.GoalAttempts != 1 {
				t.Errorf("goal.parked GoalAttempts = %d, want 1 (fail-fast, no retry)", ev.GoalAttempts)
			}
		}
	}
	if sawCleared {
		t.Error("goal.cleared emitted — a permanent worker-turn error must park, never clear (see context overflow for the one deliberate clearing exception)")
	}
	if parked != 1 {
		t.Fatalf("goal.parked events = %d, want exactly 1", parked)
	}
	if stalled != 1 {
		t.Errorf("goal.stalled events = %d, want exactly 1 (single fail-fast attempt)", stalled)
	}

	loaded, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cond, ok := loaded.ActiveGoal(); !ok || cond != "cond" {
		t.Errorf("resumed ActiveGoal = %q, %v; want still active after a permanent-error park", cond, ok)
	}
}

// TestPursueGoalPermanentErrorSingleAttemptRegardlessOfPriorToolExecution
// proves the permanent-error fail-fast path (no retry, ever) behaves
// identically whether or not a tool already executed earlier in the same
// attempt. It does NOT exercise the non-idempotency gate itself — the
// permanent branch in promptTurnWithRetry returns before that gate is ever
// reached, so a tool running first changes nothing about which branch
// fires. What this test actually pins: the attempt count and single stall
// record still reflect exactly one attempt, matching
// TestPursueGoalPermanentWorkerErrorParksAfterOneAttempt's shape, even when
// a tool ran on the way to the permanent failure.
func TestPursueGoalPermanentErrorSingleAttemptRegardlessOfPriorToolExecution(t *testing.T) {
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
		workerErr:      permanentProviderErr(),
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
		t.Errorf("tool executions = %d, want exactly 1 (a retry must not re-run it)", toolRuns)
	}
	if prov.workerCall != 2 {
		t.Errorf("worker provider calls = %d, want exactly 2 (no third attempt)", prov.workerCall)
	}

	var stalled, parked int
	for _, ev := range evs {
		switch ev.Type {
		case EventGoalStalled:
			stalled++
		case EventGoalParked:
			parked++
			// promptTurnWithRetry's attempts counts calls to s.Prompt, not
			// underlying provider.Stream calls — the tool call and its
			// failing follow-up model call both happen inside ONE s.Prompt
			// invocation (see TestPursueGoalNoRetryAfterToolExecution, which
			// asserts prov.workerCall == 2 for the same shape), so this is 1.
			if ev.GoalAttempts != 1 {
				t.Errorf("goal.parked GoalAttempts = %d, want 1", ev.GoalAttempts)
			}
		}
	}
	if stalled != 1 {
		t.Errorf("goal.stalled events = %d, want 1", stalled)
	}
	if parked != 1 {
		t.Errorf("goal.parked events = %d, want 1", parked)
	}
}
