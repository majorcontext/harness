// Goal loop: pursue a completion condition with an independent evaluator.
//
// PursueGoal drives the ordinary Prompt loop toward a natural-language
// condition. After every turn it asks a second, TOOL-LESS model — the
// evaluator, resolved through the same provider registry — whether the
// condition is met, feeding the evaluator's reason back as guidance for the
// next turn until the condition is met or the turn budget runs out.
//
// This is a plan-artifact-free, gate-free loop: it introduces no plan mode and
// no permission gate (see AGENTS.md, "Deliberately absent"). It is a control
// loop over Prompt plus a read-only evaluator call, nothing more.
//
// Durable goal.* records land in the session log so a resumed session can tell
// whether a goal is still active (see store.go, ActiveGoal). The loop also
// emits goal.* engine events so a server can journal them.
//
// # State machine
//
// A goal is a single boolean, goalActive, plus its condition string, both
// guarded by Session.mu (see the Session struct). There are exactly two
// terminal transitions out of "active" — achieved and cleared — and one
// transient sub-state, "stalled", that a worker-turn failure passes through
// on its way to either a retry (back to "active", same turn) or a permanent
// clear:
//
//	          RegisterGoal
//	               |
//	               v
//	+-------- ACTIVE (goal.set) ----------+
//	|              |                       |
//	|   worker turn errors                 | evaluator: MET
//	|              |                       v
//	|              v                  ACHIEVED (goal.achieved)
//	|         STALLED (goal.stalled, carries the error)
//	|              |
//	|    retries < goalWorkerRetries AND
//	|    no tool executed this attempt?
//	|         /              \
//	|       yes               no
//	|        |                 |
//	|   (wait goalRetryDelay)  |
//	+--------+                 v
//	                      CLEARED (goal.cleared, carries the error reason)
//	ClearGoal (caller/DELETE) ----------> CLEARED (goal.cleared, no reason)
//
// The retry branch waits a capped exponential backoff (goalRetryDelay; see
// goalWorkerRetries) before the next attempt, and is gated off the moment a
// tool call has executed during the failing attempt — see
// promptTurnWithRetry's doc comment for why a retry (which re-issues the
// whole directive, not a resume) is unsafe to do blindly once a tool has
// already run.
//
// The critical invariant this enforces — the one a real incident violated
// (see the forensic note below) — is that ACTIVE has no third way out. Every
// path from ACTIVE terminates in ACHIEVED or CLEARED, both of which are
// durable, journaled records; there is no path that leaves goalActive true
// with nothing further ever happening. Before this fix, a worker-turn error
// (s.Prompt returning non-nil) took a third, undocumented way out: PursueGoal
// returned the bare error immediately, goalActive stayed true, and nothing
// was ever recorded to explain why the loop had stopped — a zombie goal,
// active in the log forever, that only a human noticing the silence could
// clear. Two production sessions hit exactly this
// (ses_41813d5a411c2ba5.jsonl, ses_55e4ae35d8344540.jsonl): each died right
// after a "goal not met" guidance message was appended, mid-turn, with no
// goal.eval and no error record — the log simply stopped, for hours, until a
// human forced a goal.cleared. See docs/goal-loop.md for the write-up.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// GoalOptions configures a PursueGoal run.
type GoalOptions struct {
	// Registered indicates the caller already called RegisterGoal
	// synchronously; PursueGoal then treats an inactive goal at loop start
	// as cleared-before-start rather than registering a fresh one.
	Registered bool

	// MaxTurns caps the number of worker turns; 0 means unlimited.
	MaxTurns int
	// Evaluator is the model ref used for the completion check. It is required
	// — the engine hardcodes no default — and is resolved through the same
	// provider registry as the worker model.
	Evaluator message.ModelRef
}

// GoalResult is the outcome of a PursueGoal run.
type GoalResult struct {
	Achieved bool
	Turns    int
	Reason   string
}

// goalEvaluatorSystem instructs the evaluator to answer in a strict two-form
// vocabulary. Parsing is lenient (prefix, case-insensitive) so a stray period
// or lowercase reply still resolves.
const goalEvaluatorSystem = `You are a strict goal-completion evaluator for an autonomous agent.
You are given a GOAL CONDITION and a transcript of the agent's work so far.
Decide whether the condition has been FULLY satisfied by the work shown.

Reply with EXACTLY ONE line, in one of these two forms and nothing else:
MET: <one short sentence saying why>
NOT MET: <one short sentence saying what is still missing>

Do not add any other text, headings, markdown, or code fences.`

// goalPartCap bounds each rendered transcript part so a long tool result cannot
// blow up the evaluator request.
const goalPartCap = 4096

// errEvaluatorUnparseable is returned when two consecutive evaluator replies
// cannot be parsed — the loop errors rather than spinning.
var errEvaluatorUnparseable = errors.New("engine: goal evaluator returned unparseable output twice in a row")

// goalWorkerRetries is how many additional attempts PursueGoal makes on a
// worker-turn error (s.Prompt failing) before giving up on that turn. A
// transient provider failure (a rate limit, a momentary 5xx, a hiccup while
// the model handles a large tool result) is indistinguishable from a
// permanent one from here, so every worker-turn error gets the same bounded
// retry — 2 extra attempts, 3 total — rather than deciding "transient" from
// error text (fragile, provider-specific, and the false negative is a
// zombie goal, whereas the false positive is at worst two wasted requests).
//
// Retries are spaced out on a capped exponential backoff (see goalRetryDelay:
// 1s after the first failure, 4s after the second, capped thereafter) so a
// rate limit or a momentary 5xx — the two named transient causes above — has
// time to clear before the next attempt; back-to-back retries with no delay
// are close to useless against exactly those causes. The wait is
// context-cancellable: a cancelled ctx ends it immediately (see
// waitGoalRetryBackoff), same as any other worker-turn cancellation.
//
// # Non-idempotency: a retry can re-run tool calls
//
// A retry is not a resume — see promptTurnWithRetry's doc comment for the
// full risk and the (partial, best-effort) mitigation this package applies.
const goalWorkerRetries = 2

// goalRetryBackoffBase and goalRetryBackoffMultiplier define the backoff
// schedule: goalRetryDelay(1) == goalRetryBackoffBase (1s), and each
// subsequent attempt multiplies the previous delay by
// goalRetryBackoffMultiplier (4x: 1s, 4s, 16s, ...), capped at
// goalRetryBackoffCap so a hypothetical future increase in goalWorkerRetries
// can never make a single wait unboundedly long. With today's
// goalWorkerRetries (2), only the first two terms of the schedule — 1s, 4s —
// are ever used; the cap and the terms beyond it exist for that future case
// and are covered by TestGoalRetryDelaySchedule.
const (
	goalRetryBackoffBase       = 1 * time.Second
	goalRetryBackoffMultiplier = 4
	goalRetryBackoffCap        = 30 * time.Second
)

// goalRetryDelay returns the backoff delay to wait after the given 1-indexed
// attempt has failed, before the next attempt runs.
func goalRetryDelay(attempt int) time.Duration {
	d := goalRetryBackoffBase
	for i := 1; i < attempt; i++ {
		if d >= goalRetryBackoffCap {
			return goalRetryBackoffCap
		}
		d *= goalRetryBackoffMultiplier
	}
	if d > goalRetryBackoffCap {
		d = goalRetryBackoffCap
	}
	return d
}

// waitGoalRetryBackoff blocks for goalRetryDelay(attempt), or until ctx is
// done, whichever comes first — the backoff is context-cancellable so a
// deliberate abort (DELETE /goal, shutdown drain) ends the loop immediately
// instead of waiting out the rest of the schedule. Uses time.NewTimer (not
// time.After) with an explicit Stop so the timer is released promptly when
// ctx fires first.
func waitGoalRetryBackoff(ctx context.Context, attempt int) error {
	t := time.NewTimer(goalRetryDelay(attempt))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PursueGoal runs the goal loop: prompt the condition, then after every turn
// ask the evaluator whether it is met, feeding the evaluator's reason back as
// guidance until the condition is met or MaxTurns is exhausted.
//
// Turn 1 prompts the raw condition as the directive. A NOT MET verdict makes
// the next directive a fixed-template guidance message carrying the evaluator's
// reason. Returns Achieved=true on the first MET verdict; Achieved=false with
// reason "max turns" when the budget runs out.
//
// A worker-turn error (s.Prompt failing) is retried up to goalWorkerRetries
// times — see promptTurnWithRetry — recording a goal.stalled record for every
// failed attempt so the session log always explains a pause instead of going
// silent. If every attempt fails, the goal is cleared (goal.cleared carrying
// the error as its reason, so no zombie active-goal state survives) and the
// error is returned. A cancelled context is never retried or treated as a
// worker failure — it is a deliberate abort (DELETE /goal, shutdown drain)
// and is returned immediately with the goal left exactly as it was, since a
// drain must be resumable. Two unparseable evaluator replies in a row also
// return an error without clearing the goal, since that failure is in the
// evaluator, not the worker, and the existing goal state is still accurate.
//
// Must not be called concurrently with itself or Prompt (it drives Prompt).
func (s *Session) PursueGoal(ctx context.Context, condition string, opts GoalOptions) (*GoalResult, error) {
	if opts.Evaluator.IsZero() {
		err := errors.New("engine: PursueGoal requires GoalOptions.Evaluator")
		s.emitSessionError(err)
		return nil, err
	}
	if strings.TrimSpace(condition) == "" {
		err := errors.New("engine: PursueGoal requires a non-empty condition")
		s.emitSessionError(err)
		return nil, err
	}

	if opts.Registered {
		// The accepting caller registered synchronously (the server handler
		// does, closing the accept-vs-clear race). If the goal is no longer
		// active, a clear won the race before the loop started: clean stop.
		if !s.goalActiveWith(condition) {
			return &GoalResult{Achieved: false, Turns: 0, Reason: "goal cleared"}, nil
		}
	} else if err := s.RegisterGoal(condition); err != nil {
		s.emitSessionError(err)
		return nil, err
	}

	directive := condition
	for turn := 1; opts.MaxTurns == 0 || turn <= opts.MaxTurns; turn++ {
		if !s.goalActiveWith(condition) {
			// Cleared between registration and this turn (or mid-loop by a
			// concurrent DELETE): clean stop, no turn runs.
			return &GoalResult{Achieved: false, Turns: turn - 1, Reason: "goal cleared"}, nil
		}
		if attempts, err := s.promptTurnWithRetry(ctx, directive, turn); err != nil {
			if errors.Is(err, context.Canceled) {
				// Deliberate abort: leave the goal exactly as it is (a
				// drain must be resumable), no goal.stalled, no clear.
				return nil, err
			}
			if !s.goalActiveWith(condition) {
				// Cleared concurrently (DELETE /goal) while a retry was in
				// flight: clean stop, same as the checks above/below.
				return &GoalResult{Achieved: false, Turns: turn - 1, Reason: "goal cleared"}, nil
			}
			// Every attempt failed — either the retry budget ran out, or
			// retrying stopped early because a tool already executed this
			// attempt (see promptTurnWithRetry's non-idempotency doc). This
			// is the fix for the zombie-goal incident (see the package
			// doc's state machine) — the goal must never stay active with
			// nothing further recorded. Clear it, carrying the error as the
			// reason, then return the error so the caller (e.g. the server)
			// still journals the failure.
			reason := fmt.Sprintf("worker turn failed after %d attempt(s): %v", attempts, err)
			s.clearGoal(reason)
			return nil, fmt.Errorf("engine: goal loop stalled: %w", err)
		}
		met, reason, err := s.evaluateGoal(ctx, condition, opts.Evaluator)
		if err != nil {
			// Unlike the Prompt call above, evaluateGoal (the evaluator
			// model call and its unparseable-twice error) never goes
			// through Prompt, so this loop-terminating error needs its own
			// session.error emission.
			s.emitSessionError(err)
			return nil, err
		}
		if !s.recordGoalEval(met, reason, turn) {
			// ClearGoal fired while this evaluation was in flight: the goal is
			// no longer active, so its verdict must not land in the journal.
			// Treat this as a clean stop, never an achievement.
			return &GoalResult{Achieved: false, Turns: turn, Reason: "goal cleared"}, nil
		}
		if met {
			if !s.achieveGoal(reason, turn) {
				// Cleared in the narrow window between recordGoalEval and
				// achieveGoal — still a clean stop, not an achievement.
				return &GoalResult{Achieved: false, Turns: turn, Reason: "goal cleared"}, nil
			}
			return &GoalResult{Achieved: true, Turns: turn, Reason: reason}, nil
		}
		directive = goalGuidance(condition, reason)
	}
	return &GoalResult{Achieved: false, Turns: opts.MaxTurns, Reason: "max turns"}, nil
}

// promptTurnWithRetry runs one worker turn (s.Prompt), retrying on error up
// to goalWorkerRetries additional times (goalWorkerRetries+1 attempts total),
// waiting goalRetryDelay(attempt) between attempts (see the constants above)
// so a rate limit or a momentary 5xx has time to clear. It returns the number
// of attempts actually made, so the caller's error message reflects reality
// even when retrying stopped early (see below).
//
// context.Canceled is never retried — it is a deliberate abort, not a
// transient failure — and is returned immediately, whether it came from
// Prompt itself or from a cancelled backoff wait. Every failed attempt is
// recorded via recordGoalStalled; if that reports the goal was concurrently
// cleared, retrying stops immediately (nothing left to retry for) and the
// triggering error is returned as-is.
//
// # Non-idempotency: a retry can re-run tool calls
//
// Each retry re-issues the same directive through Prompt, which re-appends it
// as a fresh user message — Prompt has no partial-turn resume point to retry
// from below itself, so this is the same "ask again" a human operator would
// do by hand, just automatic and bounded. That is harmless when the failed
// attempt never got as far as executing a tool call: nothing happened yet to
// redo. It is NOT harmless when the failure happened on a LATER model call
// within that same attempt — Prompt's loop is model call -> tool calls ->
// model call -> ... until end-of-turn, so an attempt can execute one or more
// tool calls, append their results, and only then hit a provider error on the
// next model call. A retry in that case re-prompts a model that still has the
// original directive to satisfy, and nothing stops it from re-issuing the
// same tool call(s) — re-running a shell command, re-writing a file — a
// second time. Whether that is actually safe is entirely tool-specific
// (idempotent tools like read_file are fine; bash running `git push` or a
// write_file generally are not), and this package has no way to know that.
//
// This IS detectable, though only partially preventable: Session tracks a
// monotonic tool-execution counter (toolExecCount, see runToolCall in
// engine.go). promptTurnWithRetry snapshots it before each attempt via
// toolExecutions() and, when an attempt fails after the count moved, treats
// that as non-retryable — it records the stall and returns the error
// immediately, without waiting or trying again, rather than reissuing a
// directive that could re-run whatever the attempt already executed. This
// closes the case this package can see. It does NOT make retries idempotent
// in general: a failure before an attempt's first tool call is still
// retried, and if that retry attempt later executes a tool and then fails
// again on a still-later call, the identical risk resurfaces one attempt
// later — there is no bound on how many times this can recur short of
// Prompt gaining a resumable, sub-turn checkpoint, which it does not have.
func (s *Session) promptTurnWithRetry(ctx context.Context, directive string, turn int) (attempts int, err error) {
	for attempt := 1; attempt <= goalWorkerRetries+1; attempt++ {
		attempts = attempt
		toolsBefore := s.toolExecutions()
		_, perr := s.Prompt(ctx, directive)
		if perr == nil {
			return attempts, nil
		}
		err = perr
		if errors.Is(err, context.Canceled) {
			return attempts, err
		}
		if !s.recordGoalStalled(err, turn, attempt) {
			// Concurrently cleared: stop retrying, nothing left to retry for.
			return attempts, err
		}
		if s.toolExecutions() > toolsBefore {
			// This attempt executed a tool call before failing: see the
			// non-idempotency doc above. Retrying would reissue the
			// directive and risk re-running that tool, so stop now instead
			// of waiting and trying again.
			return attempts, err
		}
		if attempt <= goalWorkerRetries {
			if werr := waitGoalRetryBackoff(ctx, attempt); werr != nil {
				return attempts, werr
			}
		}
	}
	return attempts, err
}

// recordGoalStalled records one failed worker-turn attempt for a turn. Like
// recordGoalEval, it is a no-op — no journal write, no event — when the goal
// is no longer active (a concurrent ClearGoal), so a stalled record can never
// land in the log after goal.cleared. Reports whether the record was
// written, i.e. whether the goal is still active and retrying is worthwhile.
func (s *Session) recordGoalStalled(err error, turn, attempt int) bool {
	s.mu.Lock()
	if !s.goalActive {
		s.mu.Unlock()
		return false
	}
	reason := err.Error()
	s.persistGoalLocked(recGoalStalled, goalRecord{Reason: reason, Turn: turn, Attempt: attempt})
	// Emit while still holding s.mu (see ClearGoal): keeps event order
	// matching log order under a concurrent clear. OnEvent must not call
	// back into this Session — that would deadlock on s.mu, held here.
	s.emit(Event{Type: EventGoalStalled, GoalReason: reason, GoalTurn: turn, GoalAttempt: attempt})
	s.mu.Unlock()
	return true
}

// ActiveGoal reports the current goal's condition when one is set but not yet
// achieved or cleared. On a resumed session it reflects the session log's
// goal.* records (condition only; run counters reset per Claude Code semantics).
// It never auto-runs a goal — the caller decides.
func (s *Session) ActiveGoal() (condition string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goalCondition, s.goalActive
}

// ClearGoal cancels an active goal: it writes a durable goal.cleared record,
// resets the in-memory goal state, and emits a goal.cleared event. It reports
// whether a goal was active (false is a no-op, so a repeated clear is
// idempotent). This is the caller-initiated clear (DELETE /goal, CLI abort);
// it carries no reason. See clearGoal for the reason-carrying variant
// PursueGoal uses when a worker turn fails permanently.
//
// Ordering guarantee: ClearGoal journals and emits goal.cleared synchronously,
// under s.mu, before it returns. A caller that also needs to cancel the loop's
// context (e.g. the server's DELETE /goal handler) MUST call ClearGoal first
// and cancel second: cancelling first lets the goal-loop worker's
// context-cancellation unwind — which ends in a terminal status-idle record —
// race this call to the journal, so goal.cleared could be journaled after the
// idle record it is supposed to precede. Clear-then-cancel makes that
// structurally impossible: by the time cancellation can wake the worker,
// goal.cleared is already durable.
func (s *Session) ClearGoal() bool {
	return s.clearGoal("")
}

// clearGoal is ClearGoal's implementation, parameterized on the reason
// recorded with goal.cleared. An empty reason (ClearGoal's case) matches the
// pre-existing on-disk shape exactly (goalRecord.Reason omitempty); a
// non-empty reason (PursueGoal's permanent-failure case) is what lets a
// resumed session's log — and a live event subscriber — tell "a caller
// cancelled this" apart from "the worker kept failing and the loop gave up".
func (s *Session) clearGoal(reason string) bool {
	s.mu.Lock()
	if !s.goalActive {
		s.mu.Unlock()
		return false
	}
	s.goalActive = false
	s.goalCondition = ""
	s.persistGoalLocked(recGoalCleared, goalRecord{Reason: reason})
	// Emit while still holding s.mu: this keeps the event stream (-> server
	// journal/SSE seqs) ordered the same as the log write above under a
	// concurrent recordGoalEval/achieveGoal race (see those functions).
	// OnEvent must not call back into this Session — doing so would
	// deadlock on s.mu, which is still held here.
	s.emit(Event{Type: EventGoalCleared, GoalReason: reason})
	s.mu.Unlock()
	return true
}

// RegisterGoal records goal.set and marks the goal active. It is called
// synchronously by whoever accepts the goal (the HTTP handler, the CLI)
// BEFORE any loop goroutine spawns, so a ClearGoal arriving after acceptance
// always observes an active goal — the round-3 registration race is
// structurally impossible. Errors if a goal is already active.
func (s *Session) RegisterGoal(condition string) error {
	if strings.TrimSpace(condition) == "" {
		return errors.New("engine: RegisterGoal requires a non-empty condition")
	}
	s.mu.Lock()
	if s.goalActive {
		cur := s.goalCondition
		s.mu.Unlock()
		return fmt.Errorf("engine: a goal is already active: %q", cur)
	}
	s.goalActive = true
	s.goalCondition = condition
	s.persistGoalLocked(recGoalSet, goalRecord{Condition: condition})
	// Emit while holding s.mu (see ClearGoal): event order matches log
	// order. OnEvent must not call back into this Session.
	s.emit(Event{Type: EventGoalSet, GoalCondition: condition})
	s.mu.Unlock()
	return nil
}

// goalActiveWith reports whether the given condition is the currently
// active goal.
func (s *Session) goalActiveWith(condition string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goalActive && s.goalCondition == condition
}

// recordGoalEval records one evaluator verdict for a turn. It is a no-op —
// no journal write, no event — when the goal is no longer active: a
// concurrent ClearGoal may have raced this evaluation to completion, and its
// verdict must never land in the log after goal.cleared. Reports whether the
// record was written.
func (s *Session) recordGoalEval(met bool, reason string, turn int) bool {
	s.mu.Lock()
	if !s.goalActive {
		s.mu.Unlock()
		return false
	}
	s.persistGoalLocked(recGoalEval, goalRecord{Met: met, Reason: reason, Turn: turn})
	// Emit while still holding s.mu (see ClearGoal): keeps event order
	// matching log order under a concurrent clear. OnEvent must not call
	// back into this Session — that would deadlock on s.mu, held here.
	s.emit(Event{Type: EventGoalEval, GoalMet: met, GoalReason: reason, GoalTurn: turn})
	s.mu.Unlock()
	return true
}

// achieveGoal records goal.achieved and clears the active goal. It is a
// no-op when the goal is no longer active (already cleared concurrently),
// so a cleared-then-achieved sequence can never reach the log. Reports
// whether the goal was achieved.
func (s *Session) achieveGoal(reason string, turns int) bool {
	s.mu.Lock()
	if !s.goalActive {
		s.mu.Unlock()
		return false
	}
	s.goalActive = false
	s.goalCondition = ""
	s.persistGoalLocked(recGoalAchieved, goalRecord{Reason: reason, Turns: turns})
	// Emit while still holding s.mu (see ClearGoal): keeps event order
	// matching log order under a concurrent clear. OnEvent must not call
	// back into this Session — that would deadlock on s.mu, held here.
	s.emit(Event{Type: EventGoalAchieved, GoalReason: reason, GoalTurns: turns})
	s.mu.Unlock()
	return true
}

// evaluateGoal runs a single tool-less evaluator request and parses its
// verdict, retrying once on an unparseable reply — two unparseable replies in a
// row are an error (never silently spin). A provider or context error surfaces
// immediately.
func (s *Session) evaluateGoal(ctx context.Context, condition string, evaluator message.ModelRef) (met bool, reason string, err error) {
	for attempt := 0; attempt < 2; attempt++ {
		out, err := s.runEvaluator(ctx, condition, evaluator)
		if err != nil {
			return false, "", err
		}
		if m, r, ok := parseEvaluation(out); ok {
			return m, r, nil
		}
	}
	return false, "", errEvaluatorUnparseable
}

// runEvaluator issues one tool-less completion check on the evaluator model and
// returns its raw text.
func (s *Session) runEvaluator(ctx context.Context, condition string, evaluator message.ModelRef) (string, error) {
	prov, err := s.cfg.Providers.For(evaluator)
	if err != nil {
		return "", err
	}
	content := "GOAL CONDITION:\n" + condition + "\n\nCONVERSATION TRANSCRIPT:\n" + renderConversation(s.History())
	req := &provider.Request{
		Model:  evaluator,
		System: []string{goalEvaluatorSystem},
		Messages: []message.Message{{
			ID:    newID("msg"),
			Role:  message.RoleUser,
			Parts: message.Parts{&message.Text{Text: content}},
		}},
		MaxTokens: 256,
	}
	stream, err := prov.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var deltas strings.Builder
	var doneText string
	for {
		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		switch ev.Type {
		case provider.EventTextDelta:
			deltas.WriteString(ev.Text)
		case provider.EventDone:
			if ev.Message != nil {
				doneText = ev.Message.Parts.Text()
			}
		}
	}
	if doneText != "" {
		return doneText, nil
	}
	return deltas.String(), nil
}

// parseEvaluation leniently reads a verdict: a case-insensitive "NOT MET" or
// "MET" prefix (checked NOT MET first, since it is not a MET prefix), with the
// remainder after an optional colon taken as the reason.
func parseEvaluation(out string) (met bool, reason string, ok bool) {
	t := strings.TrimSpace(out)
	up := strings.ToUpper(t)
	switch {
	case strings.HasPrefix(up, "NOT MET"):
		return false, trimReason(t[len("NOT MET"):]), true
	case strings.HasPrefix(up, "MET"):
		return true, trimReason(t[len("MET"):]), true
	default:
		return false, "", false
	}
}

func trimReason(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, ":")
	return strings.TrimSpace(s)
}

// goalGuidance is the fixed-template directive sent after a NOT MET verdict.
func goalGuidance(condition, reason string) string {
	return "The goal has not been met yet.\n\nGOAL: " + condition +
		"\n\nEVALUATOR FEEDBACK: " + reason +
		"\n\nKeep working until the goal is fully satisfied, then stop."
}

// renderConversation renders history compactly for the evaluator: each message
// role-labeled, each part rendered as text and capped at goalPartCap.
func renderConversation(history []message.Message) string {
	var b strings.Builder
	for _, m := range history {
		b.WriteString(strings.ToUpper(string(m.Role)))
		b.WriteString(":\n")
		for _, p := range m.Parts {
			b.WriteString(truncateForGoal(renderPart(p)))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func renderPart(p message.Part) string {
	switch v := p.(type) {
	case *message.Text:
		return v.Text
	case *message.Reasoning:
		return "[reasoning] " + v.Text
	case *message.ToolCall:
		return fmt.Sprintf("[tool call %s] %s", v.Name, string(v.Arguments))
	case *message.ToolResult:
		s := "[tool result] " + v.Content.Text()
		if v.IsError {
			s = "[tool result (error)] " + v.Content.Text()
		}
		return s
	case *message.Blob:
		return "[blob " + v.MediaType + "]"
	default:
		return ""
	}
}

func truncateForGoal(s string) string {
	if len(s) <= goalPartCap {
		return s
	}
	return s[:goalPartCap] + "…[truncated]"
}
