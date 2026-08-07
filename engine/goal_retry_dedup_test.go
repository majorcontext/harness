package engine

import (
	"context"
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
