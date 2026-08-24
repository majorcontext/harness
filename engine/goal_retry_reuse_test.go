package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// This file tests the seven invariants of
// docs/design/goal-retry-directive-reuse.md §2: promptTurnWithRetry's retry
// dispatch must reuse an already-appended, still-unanswered directive
// instead of appending a duplicate, for the shape that dispatch approves
// (directiveReuseEligible, goal.go), while leaving every other shape to
// today's drop-and-reappend fallback (dropUnansweredDirective) unchanged.

// messagesEqualJSON reports whether a and b are equal message-for-message,
// comparing each message's canonical JSON encoding (role, parts, ID,
// CreatedAt) rather than Go struct equality: time.Time carries a monotonic
// reading in memory that a round-trip through the JSONL log never
// reproduces, so a raw reflect.DeepEqual would flag a difference that is
// not really there. JSON marshaling renders both sides through the exact
// same wire-time formatting the log itself already uses.
func messagesEqualJSON(t *testing.T, a, b []message.Message) bool {
	t.Helper()
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		aj, err := json.Marshal(a[i])
		if err != nil {
			t.Fatalf("marshal a[%d]: %v", i, err)
		}
		bj, err := json.Marshal(b[i])
		if err != nil {
			t.Fatalf("marshal b[%d]: %v", i, err)
		}
		if !bytes.Equal(aj, bj) {
			return false
		}
	}
	return true
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// rawRecordTypes reads s's own session log file directly (bypassing
// LoadSession's message replay entirely) and returns the "type" field of
// every line, in file order — used by invariant 6's test to prove no new
// record type was introduced.
func rawRecordTypes(t *testing.T, s *Session) []string {
	t.Helper()
	data, err := os.ReadFile(sessionPath(s.cfg.SessionDir, s.ID))
	if err != nil {
		t.Fatalf("read session log: %v", err)
	}
	var types []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &head); err != nil {
			t.Fatalf("unmarshal record line %q: %v", line, err)
		}
		types = append(types, head.Type)
	}
	return types
}

// TestPursueGoalRetryReuseLiveAndLogEachHaveExactlyOneDirective is the
// red-first regression test for invariants 1 and 2: after two failed worker
// attempts (a transient, unclassified error — the deterministic tier) and a
// third that succeeds, the directive text must appear EXACTLY ONCE both in
// live history AND, crucially, in the DURABLE LOG reached via LoadSession.
// Before the reuse fix, dropUnansweredDirective already held invariant 2
// (live never grows past one copy) — the defect this fix closes is that the
// LOG, which dropUnansweredDirective cannot touch, still grew to three
// copies. Reading through LoadSession (not the live session's own history)
// is what actually exercises the log.
//
// Red-verified: reverting promptTurnWithRetry's dispatch to always call
// s.Prompt (the pre-fix shape) makes the reloaded count 3, not 1 — see this
// test's own doc note in the PR description for the captured red output.
func TestPursueGoalRetryReuseLiveAndLogEachHaveExactlyOneDirective(t *testing.T) {
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

		if got := countUserMessagesWithText(s.History(), "cond"); got != 1 {
			t.Errorf("invariant 2: live directive count = %d, want exactly 1", got)
		}

		loaded, err := LoadSession(s.cfg, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := countUserMessagesWithText(loaded.History(), "cond"); got != 1 {
			t.Errorf("invariant 1: reloaded (durable log) directive count = %d, want exactly 1 (two failed attempts must not leave permanent duplicates in the LOG)", got)
		}
	})
}

// TestPursueGoalRetryReuseLiveHistoryEqualsReplayedHistory is the red-first
// regression test for invariant 3, the whole point of this fix: LoadSession
// on the durable log must produce a history equal, message-for-message, to
// the live session's own s.History() — never merely "no duplicates", but
// full agreement between what the live process holds and what a resumed
// process would see. Before the reuse fix, dropUnansweredDirective pruned
// the live copy of each failed attempt's directive but never touched the
// log, so a reload replayed messages live history no longer had: this
// exact scenario left live history with 1 directive and replayed history
// with 3.
//
// This test's provider never produces an orphaned tool_use (every failing
// worker call fails the Stream() call itself, before any tool_call is ever
// emitted), so message.ResolveOrphanToolCalls' load-time repair
// (engine/store.go) never has anything to inject and no
// position-dependent synthetic message ID is ever produced — the plain
// per-message JSON comparison below needs no exclusion for that case.
func TestPursueGoalRetryReuseLiveHistoryEqualsReplayedHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 2,
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
			t.Fatalf("PursueGoal error = %v, want nil", err)
		}
		if !res.Achieved {
			t.Fatalf("result = %+v, want achieved", res)
		}

		live := s.History()
		loaded, err := LoadSession(s.cfg, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		replayed := loaded.History()

		if !messagesEqualJSON(t, live, replayed) {
			t.Errorf("live history (%d messages) does not equal replayed history (%d messages):\nlive:\n%s\nreplayed:\n%s",
				len(live), len(replayed), mustJSON(t, live), mustJSON(t, replayed))
		}
	})
}

// TestPursueGoalRetryReuseNeverLosesEmbeddedOperatorMessage is the
// red-first regression test for invariant 4: a directive is not always
// just the goal condition on its own. PursueGoal's own turn-boundary queue
// drain bakes a drained "OPERATOR MESSAGES" block directly INTO the
// directive STRING before promptTurnWithRetry ever sees it, so that block
// rides inside the SAME message every retry reuses. This must survive two
// failed attempts and the eventual, successful reuse — appearing exactly
// once in BOTH live history and the durable log (never lost, and, since
// invariants 1/2 already forbid a duplicate directive, never doubled
// either).
//
// This is distinct from the existing park-focused tests in
// goal_retry_dedup_test.go (TestPursueGoalDeterministicParkKeepsEmbeddedOperatorMessage
// and its two siblings), which only prove survival across a PARK, never
// across a retry that goes on to REUSE the directive and succeed.
func TestPursueGoalRetryReuseNeverLosesEmbeddedOperatorMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &goalProvider{
			workerErrN: 2,
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "all done"}),
			},
			eval: [][]provider.Event{
				evalTurn("MET: looks complete"),
			},
		}
		s := goalSession(t, prov, t.TempDir())

		// Enqueued BEFORE PursueGoal starts, so turn 1's OWN turn-boundary
		// drain (before promptTurnWithRetry ever runs) bakes it into turn
		// 1's directive string — not a separately-appended message.
		if _, err := s.EnqueuePrompt("urgent operator directive"); err != nil {
			t.Fatalf("EnqueuePrompt = %v", err)
		}

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil", err)
		}
		if !res.Achieved {
			t.Fatalf("result = %+v, want achieved", res)
		}

		live := s.History()
		var liveCount int
		for _, m := range live {
			if strings.Contains(m.Parts.Text(), "urgent operator directive") {
				liveCount++
			}
		}
		if liveCount != 1 {
			t.Errorf("live history contains the embedded operator message %d times, want exactly 1: %s", liveCount, mustJSON(t, live))
		}

		loaded, err := LoadSession(s.cfg, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		var replayedCount int
		for _, m := range loaded.History() {
			if strings.Contains(m.Parts.Text(), "urgent operator directive") {
				replayedCount++
			}
		}
		if replayedCount != 1 {
			t.Errorf("reloaded (durable log) history contains the embedded operator message %d times, want exactly 1", replayedCount)
		}
	})
}

// TestPursueGoalParkedDirectiveNotDuplicatedInLog is the red-first
// regression test for invariant 5 combined with invariant 1: a parked
// attempt's directive must appear exactly once in the DURABLE LOG, not just
// live history. Every worker call fails identically (goalWorkerRetries+1 =
// 3 attempts total on the deterministic tier), so attempt 1 appends the
// directive via the ordinary Prompt path and attempts 2 and 3 must both
// reuse it — never re-append — right up to the exhausting attempt that
// parks.
//
// Before the reuse fix, attempts 2 and 3 each called s.Prompt again after
// dropUnansweredDirective pruned the LIVE copy, so the log accumulated
// THREE directive records even though live history (correctly) only ever
// showed one — this test's LoadSession-based count is what would have
// caught that.
func TestPursueGoalParkedDirectiveNotDuplicatedInLog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		workerErr := errors.New("worker provider exploded")
		prov := &goalProvider{failWorker: workerErr}
		s := goalSession(t, prov, t.TempDir())

		_, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if !IsGoalWorkerParked(err) {
			t.Fatalf("err = %v, want IsGoalWorkerParked", err)
		}

		if got := countUserMessagesWithText(s.History(), "cond"); got != 1 {
			t.Errorf("live directive count = %d, want exactly 1", got)
		}

		loaded, err := LoadSession(s.cfg, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := countUserMessagesWithText(loaded.History(), "cond"); got != 1 {
			t.Errorf("reloaded (durable log) directive count = %d, want exactly 1 (a parked attempt's directive is appended exactly once, by attempt 1's ordinary Prompt call — every later attempt before the park reuses it)", got)
		}
	})
}

// TestPursueGoalRetryReuseWritesNoNewRecordType is the regression test for
// invariant 6: the fix must introduce no new session-log record type and no
// new field an older binary must understand (see the design doc §4's
// rejection of a durable "retract" record). This reads the session's raw
// JSONL log directly — bypassing LoadSession's own record-type switch
// entirely — and asserts every record's "type" field is one this package
// already knew about before this fix, across a scenario that exercises
// every dispatch branch promptTurnWithRetry's retry logic has: a failed
// attempt, a successful reuse, and a goal achieved.
func TestPursueGoalRetryReuseWritesNoNewRecordType(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		knownTypes := map[string]bool{
			recSession: true, recMessage: true, recModel: true,
			recGoalSet: true, recGoalUpdated: true, recGoalEval: true,
			recGoalStalled: true, recGoalAchieved: true, recGoalCleared: true,
			recGoalEvalFailed: true, recGoalParked: true,
			recPromptQueued: true, recPromptDequeued: true,
			recCompact: true,
		}

		prov := &goalProvider{
			workerErrN: 2,
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
			t.Fatalf("PursueGoal error = %v, want nil", err)
		}
		if !res.Achieved {
			t.Fatalf("result = %+v, want achieved", res)
		}

		for i, typ := range rawRecordTypes(t, s) {
			if !knownTypes[typ] {
				t.Errorf("log record %d has type %q, want one of the record types this package already defined before this fix (no new journal format)", i, typ)
			}
		}
	})
}

// TestPursueGoalInterruptedTurnTailSurvivesInLogAfterLiveDrop is the
// regression test for invariant 7 — the log stays append-only in the
// strict sense; this fix prevents a WRITE, it never RETRACTS one — exercised
// against the one shape §5 of the design doc deliberately keeps out of
// scope: the three-message interrupted-turn tail (directive, partial
// assistant reply with an orphaned ToolCall, synthetic tool result).
// dropUnansweredDirective still prunes that trio from LIVE history only
// (unchanged behavior, see goal.go), so the log ends up STRICTLY LONGER
// than live history for this one turn — proving the log itself never lost
// what live memory dropped, which is exactly invariant 7's claim, not a
// regression this fix introduces.
func TestPursueGoalInterruptedTurnTailSurvivesInLogAfterLiveDrop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		orphaned := toolCall("orphan1", "bash", `{"command":"echo hi"}`)
		prov := &goalProvider{
			workerDieN:      1,
			workerDieEvents: []provider.Event{{Type: provider.EventToolCall, ToolCall: orphaned}},
			workerDieErr:    errTransportDropped,
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
			t.Fatalf("PursueGoal error = %v, want nil (a deterministic failure with no tool execution must retry)", err)
		}
		if !res.Achieved {
			t.Fatalf("result = %+v, want achieved", res)
		}

		live := s.History()
		loaded, err := LoadSession(s.cfg, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		replayed := loaded.History()

		if len(replayed) <= len(live) {
			t.Fatalf("replayed history = %d messages, live = %d; want the log strictly LONGER (the dropped interrupted-turn trio must remain journaled, never retracted)", len(replayed), len(live))
		}

		var gotOrphanInLog bool
		for _, m := range replayed {
			for _, p := range m.Parts {
				if tc, ok := p.(*message.ToolCall); ok && tc.CallID == "orphan1" {
					gotOrphanInLog = true
				}
			}
		}
		if !gotOrphanInLog {
			t.Errorf("replayed (durable log) history does not carry the dropped interrupted ToolCall %q — the log must never lose an already-written record", "orphan1")
		}

		for _, m := range live {
			for _, p := range m.Parts {
				if tc, ok := p.(*message.ToolCall); ok && tc.CallID == "orphan1" {
					t.Errorf("live history still carries the interrupted ToolCall %q — want it pruned from LIVE memory (§5, unchanged behavior)", "orphan1")
				}
			}
		}
	})
}

// TestPursueGoalRetryFallsBackWhenAnchorFoldedAwayMidTurn is the production
// entry point test for the design doc's §6 risk: maybeAutoCompact runs
// inside Prompt BEFORE the directive is appended, so a compaction landing
// between one turn's anchor capture and a later retry's dispatch decision
// can fold the anchor message away entirely. directiveReuseEligible must
// recognize the missing anchor and report NOT eligible (see its own doc
// comment); promptTurnWithRetry's dispatch must then fall back to an
// ordinary Prompt call rather than reuse against a tail it can no longer
// safely identify.
//
// This drives two goal turns for real through PursueGoal (not a hand-built
// history): turn 1 succeeds and becomes turn 2's anchor. Turn 2's first
// attempt fails; onWorkerStream fires exactly as that failing call starts
// and collapses history down to the tail this attempt itself is
// responsible for — simulating a compaction fold whose boundary lands
// exactly at the anchor, the same fold shape
// TestDropUnansweredDirectiveByIdentityIgnoresLaterUnrelatedGrowth already
// covers for dropUnansweredDirective directly. Turn 2's second attempt can
// then no longer find the anchor, so it must fall back to appending a
// fresh directive (accepting a duplicate in this one rare race, per
// directiveReuseEligible's doc comment) rather than crash, panic, or
// silently drop unrelated history.
func TestPursueGoalRetryFallsBackWhenAnchorFoldedAwayMidTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var s *Session
		prov := &goalProvider{
			// Call 1 (turn 1) succeeds. Call 2 (turn 2, attempt 1) fails.
			// Call 3 (turn 2, attempt 2 — the fallback) succeeds.
			failWorkerCall: 2,
			workerErr:      errors.New("fake transient provider error"),
			worker: [][]provider.Event{
				asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 1 done"}),
				asstTurn(provider.StopEndTurn, &message.Text{Text: "turn 2 done"}),
			},
			eval: [][]provider.Event{
				evalTurn("NOT MET: keep going"),
				evalTurn("MET: done"),
			},
		}
		prov.onWorkerStream = func(call int) {
			if call != 2 {
				return
			}
			// Fires at the START of turn 2's failing attempt — AFTER that
			// attempt's own Prompt call has already appended turn 2's
			// directive, but before the provider call fails. Collapsing
			// history to its own last message keeps that just-appended
			// directive but removes everything before it, including turn
			// 2's anchor (turn 1's assistant reply) — exactly the §6 race.
			s.mu.Lock()
			if n := len(s.history); n > 0 {
				s.history = s.history[n-1:]
			}
			s.mu.Unlock()
		}
		s = goalSession(t, prov, t.TempDir())

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil (the fallback path must still let turn 2 succeed)", err)
		}
		if !res.Achieved {
			t.Fatalf("result = %+v, want achieved", res)
		}
		if prov.workerCall != 3 {
			t.Errorf("worker provider calls = %d, want exactly 3 (turn 1, turn 2's failing attempt, turn 2's fallback attempt)", prov.workerCall)
		}
	})
}

// fallbackAnchorRegressionProvider gives exact, call-number-precise control
// over the worker stream that TestPursueGoalRetryFallbackBoundsDuplicates
// AfterUndroppableResidue needs — goalProvider's workerErrN/failWorkerCall
// combination cannot express it, because workerErrN consumes its failure
// budget starting at call 1 regardless of failWorkerCall's own exact-match
// target (the two do not compose the way their doc comments alone suggest).
// Call 1 always returns the tool-call turn (the hook denies it); calls
// 2 through failN all fail with a retryable-classified error; every call
// after that succeeds with the final turn.
type fallbackAnchorRegressionProvider struct {
	calls int
	failN int
}

func (p *fallbackAnchorRegressionProvider) Name() string { return "test" }

func (p *fallbackAnchorRegressionProvider) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	if len(req.Tools) == 0 {
		return &scriptedStream{events: evalTurn("MET: looks complete")}, nil
	}
	p.calls++
	switch {
	case p.calls == 1:
		return &scriptedStream{events: asstTurn(provider.StopToolUse, toolCall("tc1", "test_tool", `{}`))}, nil
	case p.calls <= p.failN:
		return nil, retryableProviderErr(provider.RetryableOverloaded)
	default:
		return &scriptedStream{events: asstTurn(provider.StopEndTurn, &message.Text{Text: "all done"})}, nil
	}
}

// TestPursueGoalRetryFallbackBoundsDuplicatesAfterUndroppableResidue is the
// red-first regression test for the review finding on PR #107 at
// engine/goal.go:1428: moving anchorID capture from per-attempt to
// once-per-turn is correct for the reuse path above, but it breaks the
// FALLBACK path (dropUnansweredDirective, invoked when the tail is some
// OTHER shape than the bare directive — see directiveReuseEligible's doc
// comment).
//
// A plugin tool.execute.before DENY appends [assistant(tool call),
// tool(denied)] without incrementing toolExecCount (engine.go), so a
// retry still runs. That residue does not match either shape
// isSafeToDropDirectiveTail approves (the directive alone, or the
// three-message interrupted-turn trio), so it is — correctly — never
// dropped. With a FIXED anchor pinned to the start of the turn, the tail
// after that anchor never again shrinks to a droppable shape for the rest
// of the turn: every later attempt's fallback re-appends a fresh
// directive and drops none. Across this test's ten retryable-tier
// failures (well under goalRetryableMaxAttempts = 12) that means ten
// extra duplicate directives, live AND in the durable log — the exact
// NEP-5272 growth this package exists to eliminate, reopened on this one
// path.
//
// Call 1 makes a tool call the hook denies. Calls 2-11 (ten retryable-tier
// failures — attempt 1's own post-deny continuation, then nine more
// single-call attempts) each fail; call 12 (attempt 11) succeeds, well
// under goalRetryableMaxAttempts (12).
//
// The fix re-anchors after a fallback append, to the point right before
// the fresh directive it just appended: attempt 3 onward then finds THAT
// directive alone in its own tail and reuses it (directiveReuseEligible)
// instead of appending yet another copy. The final duplicate count is
// bounded at 2 — the original directive, permanently stuck behind its own
// undroppable denied-tool residue, plus the one fallback directive that
// goes on to be reused for every remaining attempt and is finally
// answered — never 11.
//
// Red-verified: reverting the re-anchor (restoring the single anchorID
// captured once before the loop, used unmodified by every fallback call)
// makes both counts 11, not 2 — see this test's PR description note for
// the captured red output.
func TestPursueGoalRetryFallbackBoundsDuplicatesAfterUndroppableResidue(t *testing.T) {
	orig := goalJitterFunc
	t.Cleanup(func() { goalJitterFunc = orig })
	goalJitterFunc = func(max time.Duration) time.Duration { return 0 }

	synctest.Test(t, func(t *testing.T) {
		testTool := Tool{
			Def: provider.ToolDef{Name: "test_tool", Description: "test", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
				t.Fatal("test_tool.Run must never be called — the hook denies every call")
				return nil, nil
			},
		}
		prov := &fallbackAnchorRegressionProvider{failN: 11}
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

		res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
		if err != nil {
			t.Fatalf("PursueGoal error = %v, want nil (a retryable outage well under budget must eventually succeed)", err)
		}
		if !res.Achieved {
			t.Fatalf("result = %+v, want achieved", res)
		}

		if got := countUserMessagesWithText(s.History(), "cond"); got != 2 {
			t.Errorf("live directive count = %d, want exactly 2 (the original, stuck behind undroppable residue, plus one reused fallback copy)", got)
		}

		loaded, err := LoadSession(s.cfg, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := countUserMessagesWithText(loaded.History(), "cond"); got != 2 {
			t.Errorf("reloaded (durable log) directive count = %d, want exactly 2", got)
		}
	})
}
