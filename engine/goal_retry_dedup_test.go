package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
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

// denyAndEnqueueHooks denies every tool call, like fakeHooks{deny: ...} in
// engine_test.go, but additionally enqueues a prompt at the MOMENT the deny
// decision is made — still inside the SAME s.Prompt call, strictly before
// engine.go's tool-call-boundary drain (DequeueAllPrompts) runs a few lines
// later in that call, right after the denied ToolResult is appended. Denying
// a tool call is already synchronous (no tool.Run ever executes), so
// landing the enqueue inside that exact window needs no goroutine or
// channel — only a hook callback that fires before the drain point does. See
// TestPursueGoalRetryNeverDropsDeliveredOperatorMessageAfterDeniedTool's own
// doc comment for why this timing matters.
type denyAndEnqueueHooks struct {
	s        *Session
	deny     string
	text     string
	enqueued bool
}

func (h *denyAndEnqueueHooks) ChatParams(_ context.Context, req *plugin.ChatParamsRequest) plugin.ChatParams {
	return req.Params
}

func (h *denyAndEnqueueHooks) SystemTransform(_ context.Context, _ *plugin.SystemTransformRequest) []string {
	return nil
}

func (h *denyAndEnqueueHooks) ShellEnv(_ context.Context, _ *plugin.ShellEnvRequest) map[string]string {
	return nil
}

func (h *denyAndEnqueueHooks) ToolExecuteBefore(_ context.Context, _ *plugin.ToolExecuteBeforeRequest) (json.RawMessage, string) {
	if !h.enqueued {
		h.enqueued = true
		if _, _, err := h.s.EnqueuePrompt(h.text, ""); err != nil {
			panic("denyAndEnqueueHooks: EnqueuePrompt failed: " + err.Error())
		}
	}
	return nil, h.deny
}

func (h *denyAndEnqueueHooks) ToolExecuteAfter(_ context.Context, req *plugin.ToolExecuteAfterRequest) message.Parts {
	return req.Output
}

func (h *denyAndEnqueueHooks) ExecuteTool(_ context.Context, req *plugin.ToolExecuteRequest) (*plugin.ToolExecuteResponse, error) {
	return nil, fmt.Errorf("plugin: no plugin provides tool %q", req.Tool)
}

func (h *denyAndEnqueueHooks) Emit(_ []plugin.Event) {}

func (h *denyAndEnqueueHooks) Tools() []plugin.ToolDef { return nil }

func (h *denyAndEnqueueHooks) Plugins() []plugin.Info { return nil }

// TestPursueGoalRetryNeverDropsDeliveredOperatorMessageAfterDeniedTool is the
// red-first regression test for the MAJOR finding on dropUnansweredDirective:
// its old enumeration of "what a failed, no-tool-executed attempt could have
// appended" was wrong. A plugin's tool.execute.before hook can DENY a tool
// call — appending a ToolResult message without incrementing toolExecCount
// (engine.go's runToolCall), so toolGateStops stays false — and the model can
// then make a further request in the SAME Prompt call, during which a
// prompt that arrived mid-tool-call gets delivered at the tool-call boundary
// (engine.go's own DequeueAllPrompts drain) as its own standalone "OPERATOR
// MESSAGES" message, already journaled prompt.dequeued("injected") before
// that further request ever runs. If THAT further request then fails,
// toolGateStops is still false (no tool ever executed this attempt), so
// dropUnansweredDirective ran — and the old version truncated history back
// to the length snapshotted before the whole attempt, discarding the
// already-delivered operator message along with the unanswered directive.
//
// denyAndEnqueueHooks (above) enqueues the prompt from INSIDE the deny
// decision itself, deliberately NOT before PursueGoal starts: enqueuing
// early would let PursueGoal's own turn-boundary drain (goal.go) bake the
// text straight into the turn's directive STRING before promptTurnWithRetry
// ever runs — and every retry re-appends that string verbatim regardless of
// whether the drop logic is correct, so a pre-enqueued prompt can never
// drive this test red for the mid-turn-delivery case it names.
//
// This test proves the fix: the operator message — a message of its own,
// distinct from the directive — and the denied tool's own result, both
// survive every retry and the eventual success.
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
	hooks := &denyAndEnqueueHooks{deny: "denied by policy", text: "urgent operator update"}
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
	hooks.s = s

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

// historyContainsText reports whether any message in history renders text
// containing want as a substring. Used below to confirm that operator mail
// baked INTO a directive string (PursueGoal's own turn-boundary queue drain
// — see operatorMessagesBlock and its embedding at PursueGoal's directive
// construction) survives a call that PARKS the goal, as opposed to a call
// that retries.
func historyContainsText(history []message.Message, want string) bool {
	for _, m := range history {
		if strings.Contains(m.Parts.Text(), want) {
			return true
		}
	}
	return false
}

// TestPursueGoalDeterministicParkKeepsEmbeddedOperatorMessage is the
// red-first regression test for the MAJOR finding on promptTurnWithRetry's
// call to dropUnansweredDirective: the call ran unconditionally, guarded
// only by a now-corrected comment claiming "every branch below this point is
// about to retry." Three branches do not retry — they PARK — and the
// deterministic-budget exhaustion below is one of them.
//
// PursueGoal bakes any queued prompt into the DIRECTIVE STRING itself at its
// own turn boundary (operatorMessagesBlock + snap.condition), and
// DequeueAllPrompts has already journaled that prompt delivered
// (prompt.dequeued("injected")) — nothing will ever re-queue it.
// isSafeToDropDirectiveTail's case 1 (a single RoleUser message) approves
// dropping that message because it cannot see the operator block embedded
// inside its text. On the LAST, parking attempt, dropping that message loses
// the operator mail from live history entirely, with no later attempt to
// re-append it — violating dropUnansweredDirective's own invariant that
// already-delivered operator mail must never be discarded.
//
// This test proves the fix: after every goalWorkerRetries+1 deterministic
// attempt fails and the loop exit-parks, the operator text baked into the
// final attempt's directive still appears in s.History().
func TestPursueGoalDeterministicParkKeepsEmbeddedOperatorMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		workerErr := errors.New("worker provider exploded")
		prov := &goalProvider{failWorker: workerErr}
		s := goalSession(t, prov, t.TempDir())

		if _, _, err := s.EnqueuePrompt("urgent operator directive", ""); err != nil {
			t.Fatalf("EnqueuePrompt = %v", err)
		}

		_, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if !IsGoalWorkerParked(err) {
			t.Fatalf("err = %v, want IsGoalWorkerParked", err)
		}

		history := s.History()
		if !historyContainsText(history, "urgent operator directive") {
			t.Errorf("history = %+v, want the operator block embedded in the parked directive to survive", history)
		}
	})
}

// TestPursueGoalRetryableBudgetExhaustedParkKeepsEmbeddedOperatorMessage
// covers the same MAJOR finding as
// TestPursueGoalDeterministicParkKeepsEmbeddedOperatorMessage above, but for
// the WEATHER tier: promptTurnWithRetry's goalRetryableMaxAttempts
// exhaustion, which returns a *goalRetryableExhaustedError instead of a bare
// err. See TestPursueGoalRetryableBudgetExhaustedParksInsteadOfClearing for
// the tier's ordinary park-shape assertions; this test adds the
// embedded-operator-mail survival check that test does not cover.
func TestPursueGoalRetryableBudgetExhaustedParkKeepsEmbeddedOperatorMessage(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })
	goalJitterFunc = func(max time.Duration) time.Duration { return 0 }

	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 1000, // never recovers within the test
			workerErr:  retryableProviderErr(provider.RetryableOverloaded),
		}
		s := goalSession(t, prov, t.TempDir())

		if _, _, err := s.EnqueuePrompt("urgent operator directive", ""); err != nil {
			t.Fatalf("EnqueuePrompt = %v", err)
		}

		_, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel, MaxTurns: 2})
		if !IsGoalWorkerParked(err) {
			t.Fatalf("err = %v, want IsGoalWorkerParked", err)
		}

		history := s.History()
		if !historyContainsText(history, "urgent operator directive") {
			t.Errorf("history = %+v, want the operator block embedded in the parked directive to survive", history)
		}
	})
}

// TestPursueGoalStreamTruncatedParkKeepsEmbeddedOperatorMessage covers the
// third parking branch: promptTurnWithRetry's goalStreamTruncatedMaxAttempts
// exhaustion (the stream-truncation tier, which shares the deterministic
// tier's short backoff schedule but runs its own, separately budgeted
// counter — see promptTurnWithRetry's doc comment). Like the other two
// tiers, exhaustion here returns a *goalRetryableExhaustedError, and must
// leave the exhausting attempt's directive — and any operator mail embedded
// in it — in live history rather than dropping it.
func TestPursueGoalStreamTruncatedParkKeepsEmbeddedOperatorMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 1000, // never recovers within the test
			workerErr:  retryableProviderErr(provider.RetryableStreamTruncated),
		}
		s := goalSession(t, prov, t.TempDir())

		if _, _, err := s.EnqueuePrompt("urgent operator directive", ""); err != nil {
			t.Fatalf("EnqueuePrompt = %v", err)
		}

		_, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if !IsGoalWorkerParked(err) {
			t.Fatalf("err = %v, want IsGoalWorkerParked", err)
		}

		history := s.History()
		if !historyContainsText(history, "urgent operator directive") {
			t.Errorf("history = %+v, want the operator block embedded in the parked directive to survive", history)
		}
	})
}
