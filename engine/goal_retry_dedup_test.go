package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// countUserMessagesWithText counts how many messages in history are a plain
// user message whose rendered text equals want exactly — the shape a goal
// turn's directive/guidance takes (see PursueGoal's directive construction).
func countUserMessagesWithText(history []message.Message, want string) int {
	n := 0
	for _, m := range history {
		if m.Role == message.RoleUser && m.Parts.Text() == want {
			n++
		}
	}
	return n
}

// TestPursueGoalRetryDoesNotDuplicateDirectiveInHistory is the red-first
// regression test for NEP-5272's defect 2 (operator finding on box
// hyper-lemon): promptTurnWithRetry's non-idempotency doc already
// acknowledges that a retry re-issues the whole directive through Prompt,
// which "has no partial-turn resume point to retry from below itself" — but
// Prompt ALSO unconditionally appends that directive as a brand new,
// durable user message every single call, so two failed attempts followed
// by a third that succeeds left THREE copies of the identical goal
// condition permanently in history, each one inflating every later
// request's input cost forever after.
//
// This test proves the mitigation goal.go CAN make without touching
// Prompt itself (out of scope — see promptTurnWithRetry's doc comment for
// the residual gap this does NOT close): once a failed attempt's directive
// goes unanswered (no assistant reply, no tool call — the only shape a
// retryable attempt's failure can leave behind, since the non-idempotency
// gate already stops retrying the moment a tool call executes), the retry
// path prunes that dangling copy from the LIVE in-memory history before
// re-appending the same text for the next attempt, so at most one copy is
// ever outstanding at a time — never a permanently growing pile.
//
// Run inside a synctest bubble purely to match
// TestPursueGoalRetriesTransientWorkerError's shape (this test does not
// itself assert on elapsed time).
func TestPursueGoalRetryDoesNotDuplicateDirectiveInHistory(t *testing.T) {
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
		s := goalSession(t, prov, t.TempDir())

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil (transient errors must be retried)", err)
		}
		if !res.Achieved {
			t.Fatalf("result = %+v, want achieved", res)
		}

		history := s.History()
		if got := countUserMessagesWithText(history, "cond"); got != 1 {
			t.Errorf("user messages carrying the directive text %q = %d, want exactly 1 (two failed attempts must not leave permanent duplicates)", "cond", got)
		}
	})
}

// TestPursueGoalRetryNeverDropsDeliveredOperatorMessageAfterDeniedTool is the
// red-first regression test for the MAJOR finding on dropUnansweredDirective:
// its old enumeration of "what a failed, no-tool-executed attempt could have
// appended" was wrong. A plugin's tool.execute.before hook can DENY a tool
// call — appending a ToolResult message without incrementing toolExecCount
// (engine.go's runToolCall), so toolGateStops stays false — and the model can
// then make a further request in the SAME Prompt call, during which a
// prompt already sitting in the queue gets delivered at the tool-call
// boundary (Prompt's own DequeueAllPrompts drain) as an "OPERATOR MESSAGES"
// block, already journaled prompt.dequeued("injected") before that further
// request ever runs. If THAT further request then fails, toolGateStops is
// still false (no tool ever executed this attempt), so
// dropUnansweredDirective ran — and the old version truncated history back
// to the length snapshotted before the whole attempt, discarding the
// already-delivered operator message along with the unanswered directive.
// This test proves the fix: the operator message, and the denied tool's own
// result, both survive every retry and the eventual success.
func TestPursueGoalRetryNeverDropsDeliveredOperatorMessageAfterDeniedTool(t *testing.T) {
	testTool := Tool{
		Def: provider.ToolDef{Name: "test_tool", Description: "test", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			t.Fatal("test_tool.Run must never be called — the hook denies every call")
			return nil, nil
		},
	}
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopToolUse, toolCall("tc1", "test_tool", `{}`)),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "all done"}),
		},
		// The SECOND worker call is this same attempt's post-tool-round
		// request (the model asked to continue after the denied call) — it
		// fails, forcing a retry.
		failWorkerCall: 2,
		workerErr:      errors.New("fake transient provider error"),
		eval: [][]provider.Event{
			evalTurn("MET: looks complete"),
		},
	}
	hooks := &fakeHooks{deny: "denied by policy"}
	s := NewSession(Config{
		Providers:    provider.Registry{prov.Name(): prov},
		Model:        message.ModelRef{Provider: prov.Name(), Model: "m1"},
		System:       []string{"base"},
		SessionDir:   t.TempDir(),
		Instructions: &InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
		Tools:        []Tool{testTool},
		Hooks:        hooks,
	})

	if _, err := s.EnqueuePrompt("urgent operator update"); err != nil {
		t.Fatalf("EnqueuePrompt = %v", err)
	}

	res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatalf("PursueGoal error = %v, want nil (a deterministic failure with no tool execution must retry)", err)
	}
	if !res.Achieved {
		t.Fatalf("result = %+v, want achieved", res)
	}

	history := s.History()
	var sawOperatorMessage, sawDeniedResult bool
	for _, m := range history {
		if strings.Contains(m.Parts.Text(), "urgent operator update") {
			sawOperatorMessage = true
		}
		for _, p := range m.Parts {
			if tr, ok := p.(*message.ToolResult); ok && tr.CallID == "tc1" && strings.Contains(tr.Content.Text(), "denied by policy") {
				sawDeniedResult = true
			}
		}
	}
	if !sawOperatorMessage {
		t.Errorf("history = %+v, want the delivered operator message to survive the retry", history)
	}
	if !sawDeniedResult {
		t.Errorf("history = %+v, want the denied tool's own result to survive the retry", history)
	}
}
