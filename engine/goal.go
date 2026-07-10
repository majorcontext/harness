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
//
// # Round 3: the same escape path, the other half of the loop
//
// The diagram above has two error edges into ACTIVE's exits — "worker turn
// errors" and "evaluator: MET" (the NOT MET edge loops back to ACTIVE) — but
// a third case sat on neither edge: an evaluator call that fails outright
// (a provider error, or two unparseable replies in a row, see
// errEvaluatorUnparseable). The worker-turn edge got its clear-on-exhaustion
// fix in round 2 (above); this one did not — PursueGoal's evaluateGoal error
// branch returned the bare error and left goalActive true, the exact same
// shape of zombie, just reached from the other model call. One production
// session (ses_01kx3ts0pjfap950bmr9b2js0b.jsonl) hit exactly this: the
// worker turn succeeded, the evaluator returned unparseable output twice in
// a row, session.error was emitted, and the goal stayed active in the log
// forever — turns=0, no goal.eval ever, nothing to explain the silence
// beyond that one error record. (Its log tail also carries a single
// anomalously large Anthropic thinking signature on the worker's last
// message — see message.ProviderData and provider/anthropic/transcode.go's
// replay-size cap — but that turn itself succeeded; it is a correlated
// hazard, not this incident's cause.) Fixed the same way as round 2: a
// failing evaluator call now clears the goal (unless the error is a
// cancelled context) before returning, closing the last "third way out" of
// ACTIVE. See TestPursueGoalUnparseableTwiceClearsGoal.
//
// # Round 4: retryable provider weather must not exhaust the same budget as
// a dead provider (GitHub issue #61)
//
// The STALLED->CLEARED edge above (goalWorkerRetries, ~5s of total backoff)
// treats every worker-turn error identically, but that conflates two very
// different failure shapes: a deterministic failure (bad request, auth)
// that will fail the same way forever, and provider-side overload/rate-limit
// weather that is self-healing but "routinely lasts several minutes" (see
// the issue). Field data: four goal loops died to ONE shared Anthropic
// overload wave across two days — every one resumed cleanly the instant a
// human manually re-armed it once the wave passed, the strongest possible
// evidence those stalls were premature, not genuine.
//
// The fix classifies each worker-turn error via provider.AsRetryable (a
// typed wrapper the provider adapter attaches — see provider/retryable.go —
// never string-matched) and gives the retryable class its OWN budget
// (goalRetryableMaxAttempts, a much longer jittered backoff — see
// promptTurnWithRetry) that never touches goalWorkerRetries. The updated
// diagram:
//
//	          RegisterGoal
//	               |
//	               v
//	+-------- ACTIVE (goal.set) ------------------------------+
//	|              |                                           |
//	|   worker turn errors                                     | evaluator: MET
//	|              |                                           v
//	|              v                                     ACHIEVED (goal.achieved)
//	|         STALLED (goal.stalled, carries the error + retryable class)
//	|              |
//	|      classified retryable?
//	|         /            \
//	|       no              yes
//	|        |               |
//	|  deterministic    retryable budget (goalRetryableMaxAttempts)
//	|  budget           exhausted (a truly long outage)?
//	|  (goalWorkerRetries)   /          \
//	|  exhausted AND       no            yes
//	|  no tool executed?    |             |
//	|   /        \     (wait, then    PARK: same turn's directive
//	|  no        yes    retry, back    retried on the NEXT ordinary
//	|  |          |     to ACTIVE)     turn (see below) — back to
//	|  (wait,     |                    ACTIVE, no clear
//	|   retry,    |
//	|   back to   |
//	|   ACTIVE)   v
//	+-----------CLEARED (goal.cleared, carries the error reason)
//	ClearGoal (caller/DELETE) --------------------------------> CLEARED (goal.cleared, no reason)
//
// Self-re-arm (deliverable 4 of issue #61): a retryable-class exhaustion
// does NOT clear the goal — it "parks" by retrying the exact same directive
// on the next ordinary turn (PursueGoal's for-loop `continue`s, so turn++
// runs exactly as it would after any other turn). This is a deliberate
// design choice over the alternative (a server-side cooldown timer that
// automatically re-POSTs /goal): parking reuses the state machine's
// EXISTING, already-durable, already-resumable "max turns exhausted"
// terminal state (goal left ACTIVE, turn.end outcome
// outcomeMaxTurnsExceeded — see server/journal.go) as the natural backstop
// once MaxTurns is set, and reuses the existing "MaxTurns==0 means no
// limit" contract as the backstop when it is not — both invariants the
// state machine already had to honor for an ordinary long-running goal, so
// this adds no new terminal state, no new server-side timer, and no new way
// for a goal to go silent: every parked cycle is bounded by real wall-clock
// time (goalRetryableBackoff's schedule can't hot-spin) and durably
// explained by a goal.stalled record naming the retryable class (see
// recordGoalStalled) the moment it happens, not after the fact.
//
// See TestPursueGoalRetryableErrorLongBackoffThenRecovers and
// TestPursueGoalRetryableBudgetExhaustedParksInsteadOfClearing, and
// docs/goal-loop.md for the operator-facing write-up.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
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
// One exception: a provider error classified provider.ErrKindContextOverflow
// (issue #62) IS distinguishable, structurally, from an ordinary opaque
// failure — the classification is a typed field the adapter sets, not text
// this package would have to parse — and it is deterministic: the same,
// now-too-long request will fail identically on every retry. See
// promptTurnWithRetry's context-overflow branch and PursueGoal's matching
// branch, both of which fail fast (no backoff wait, no further attempt,
// distinct goal.cleared reason) instead of taking the bounded-retry path
// described here.
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

// # Retryable-class backoff (GitHub issue #61)
//
// A worker-turn error the provider adapter classified retryable (see
// provider.AsRetryable — Anthropic 529/overloaded_error, 429, 5xx; see
// provider/anthropic and provider/openaicompat) gets an entirely separate
// budget and schedule from the deterministic goalWorkerRetries path above,
// because the two failure modes have opposite shapes: a deterministic
// failure (bad request, auth) will fail identically forever, so a short,
// fast-fail budget is correct; provider-side overload/rate-limit weather is
// self-healing but "routinely lasts several minutes" (see the issue's field
// data — four goal loops died to ONE shared Anthropic overload wave across
// two days, every one resuming cleanly the moment a human manually re-armed
// it once the wave passed), so a short budget kills a loop the provider was
// always going to let succeed. goalRetryableMaxAttempts is deliberately
// generous (12 attempts) and the backoff (goalRetryableDelay) grows from 5s
// to a 5-minute cap — worst case, about 30 minutes of waiting before this
// budget is exhausted, versus the deterministic path's ~5 seconds total.
//
// These retryable-class attempts NEVER consume goalWorkerRetries: a
// provider overload wave does not spend down the same fast-fail allowance a
// bad request would (see promptTurnWithRetry).
const (
	goalRetryableBackoffBase       = 5 * time.Second
	goalRetryableBackoffMultiplier = 2
	goalRetryableBackoffCap        = 5 * time.Minute
	// goalRetryableMaxAttempts bounds a single turn's retryable-class
	// attempts before promptTurnWithRetry gives up and PursueGoal parks the
	// turn (see goalRetryableExhaustedError) rather than clearing the goal.
	goalRetryableMaxAttempts = 12
)

// goalRetryableDelay returns the base (pre-jitter) backoff for the given
// 1-indexed retryable-class attempt that just failed, doubling each time up
// to goalRetryableBackoffCap — the same shape as goalRetryDelay, just a
// much longer schedule (see the doc comment above).
func goalRetryableDelay(attempt int) time.Duration {
	d := goalRetryableBackoffBase
	for i := 1; i < attempt; i++ {
		if d >= goalRetryableBackoffCap {
			return goalRetryableBackoffCap
		}
		d *= goalRetryableBackoffMultiplier
	}
	if d > goalRetryableBackoffCap {
		d = goalRetryableBackoffCap
	}
	return d
}

// goalJitterFunc returns a pseudo-random duration in [0, max) — the random
// half of goalRetryableBackoff's "equal jitter" (half the base delay fixed,
// half randomized). Jitter matters here specifically because a shared
// provider overload wave hits every affected goal loop at once (see the
// GitHub issue #61 field data: four losses to ONE wave); without it, every
// surviving loop would retry in lockstep and re-hit the still-recovering
// provider at the exact same instants. Real math/rand in production;
// overridable by tests (see TestGoalRetryableDelaySchedule and
// TestPursueGoalRetryableErrorLongBackoffThenRecovers) so the schedule
// stays exactly assertable instead of merely bounded — the same test-seam
// convention as server.goalDeleteRace.
var goalJitterFunc = func(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
}

// goalRetryableBackoff applies equal jitter to goalRetryableDelay(attempt):
// half the base delay is fixed, the other half is randomized within
// [0, half) via goalJitterFunc, so the actual wait for attempt N falls in
// [half, base).
func goalRetryableBackoff(attempt int) time.Duration {
	base := goalRetryableDelay(attempt)
	half := base / 2
	return half + goalJitterFunc(half)
}

// waitGoalRetryableBackoff is waitGoalRetryBackoff's counterpart for the
// retryable-class schedule: it blocks for goalRetryableBackoff(attempt), or
// until ctx is done, whichever comes first.
func waitGoalRetryableBackoff(ctx context.Context, attempt int) error {
	t := time.NewTimer(goalRetryableBackoff(attempt))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// goalRetryableExhaustedError is returned by promptTurnWithRetry when a
// worker turn's retryable-class backoff budget (goalRetryableMaxAttempts)
// is exhausted while every failure was classified provider-retryable — a
// truly long outage, not the "several minutes" the schedule is tuned for.
//
// PursueGoal recognizes this type (via errors.As) and treats it completely
// differently from an ordinary exhausted-retries error: see PursueGoal's
// "park, don't die" handling, the self-re-arm half of GitHub issue #61's
// fix. It is never returned for a deterministic failure — those still
// return the bare underlying error, exhausting goalWorkerRetries and
// clearing the goal exactly as before this fix.
type goalRetryableExhaustedError struct {
	err   error
	class provider.RetryableClass
}

func (e *goalRetryableExhaustedError) Error() string { return e.err.Error() }
func (e *goalRetryableExhaustedError) Unwrap() error { return e.err }

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
// error is returned — UNLESS the error is classified provider-retryable
// (see provider.AsRetryable and GitHub issue #61), in which case
// promptTurnWithRetry instead runs a much longer, separately-budgeted
// backoff (goalRetryableMaxAttempts), and exhausting THAT budget parks the
// turn (retries the same directive on the next ordinary turn) rather than
// clearing the goal — see the package doc's "Round 4" section for the full
// rationale and the state diagram. A cancelled context is never retried or
// treated as a worker failure — it is a deliberate abort (DELETE /goal,
// shutdown drain) and is returned immediately with the goal left exactly as
// it was, since a drain must be resumable. A failing evaluator call — a
// provider error, or two unparseable replies in a row — is not retried (it
// is the worker turn that is expensive and worth protecting from a
// transient hiccup, not the tool-less evaluator check), but it is held to
// the exact same no-zombie
// guarantee as a permanently failing worker turn: the goal is cleared
// (goal.cleared carrying the error as its reason) before the error is
// returned, unless the error is itself a cancelled context, which leaves the
// goal untouched for the same resume reason as above.
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
			var exhausted *goalRetryableExhaustedError
			if errors.As(err, &exhausted) {
				// Self-re-arm (GitHub issue #61, deliverable 4): the
				// retryable-class backoff budget ran out for this turn — a
				// truly long provider outage, not the "several minutes" the
				// schedule is tuned for — but the goal must NOT become a
				// permanently-dead stall requiring an operator re-POST.
				// promptTurnWithRetry already journaled the exhaustion as a
				// non-waiting goal.stalled record naming the retryable
				// class (see recordGoalStalled), so the pause is durably
				// explained; there is nothing left to clear.
				//
				// Instead of clearing, PARK: retry the exact same directive
				// on the next loop iteration, which — because it consumes
				// an ordinary turn (turn++ via the for-loop's post
				// statement below) — is bounded exactly the way every
				// other turn is. With MaxTurns set, repeated parking
				// eventually reaches the ordinary, already-resumable "max
				// turns" terminal state (goal left ACTIVE, see below)
				// instead of "cleared". With MaxTurns unlimited (0), it
				// parks indefinitely, each cycle bounded by real wall-clock
				// time (goalRetryableBackoff's schedule) rather than
				// hot-spinning — the same opt-in "no turn limit" contract
				// MaxTurns==0 already carries for an ordinary long-running
				// goal. No evaluator call runs for a turn that never had a
				// successful worker attempt.
				continue
			}
			if provider.IsContextOverflow(err) {
				// Issue #62, layer 1: a deterministic context/prompt
				// overflow gets its own distinct stall/clear reason
				// instead of the generic "worker turn failed after N
				// attempt(s)" wording, and the error is returned AS-IS
				// (not wrapped in "engine: goal loop stalled") so
				// last_turn.error (server/journal.go's recordTurnEnd)
				// surfaces exactly err.Error()'s clear, deterministic
				// message — see provider.Error.Error(). Checked after the
				// exhausted-park branch above by construction: overflow is
				// never classified retryable, so the two are disjoint.
				s.clearGoal(err.Error())
				return nil, err
			}
			// Every attempt failed — either the deterministic retry budget
			// ran out, or retrying stopped early because a tool already
			// executed this attempt (see promptTurnWithRetry's
			// non-idempotency doc). This is the fix for the zombie-goal
			// incident (see the package doc's state machine) — the goal
			// must never stay active with nothing further recorded. Clear
			// it, carrying the error as the reason, then return the error
			// so the caller (e.g. the server) still journals the failure.
			reason := fmt.Sprintf("worker turn failed after %d attempt(s): %v", attempts, err)
			s.clearGoal(reason)
			return nil, fmt.Errorf("engine: goal loop stalled: %w", err)
		}
		met, reason, err := s.evaluateGoal(ctx, condition, opts.Evaluator)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				// Deliberate abort (or a cancelled ctx surfacing through the
				// evaluator's own stream): same rule as the worker-turn
				// path above — leave the goal exactly as it is, since a
				// drain must be resumable.
				return nil, err
			}
			// Unlike the Prompt call above, evaluateGoal (the evaluator
			// model call, its provider errors, and its unparseable-twice
			// error) never goes through Prompt, so this loop-terminating
			// error needs its own session.error emission.
			//
			// It also needs its own clear. This was the round-3 zombie-goal
			// escape path: a permanent worker-turn failure clears the goal
			// (see promptTurnWithRetry's caller above), but a permanent
			// evaluator failure returned bare, leaving goalActive true with
			// nothing further ever recorded — a session (see the forensic
			// note ses_01kx3ts0pjfap950bmr9b2js0b.jsonl) whose transcript
			// just stops after the worker's last message, goal still active
			// in the log forever. The state machine promises no third way
			// out of ACTIVE (see the package doc); that promise cannot be
			// conditional on which half of the loop failed. Clear it,
			// carrying the error as the reason, exactly as the worker-turn
			// path does, then return the error so the caller still
			// journals the failure.
			//
			// But if a concurrent DELETE /goal already cleared the goal
			// while this call was in flight, there is nothing left to
			// clear and nothing to journal as a failure: the goal-loop
			// state machine already reached a clean stop, symmetric with
			// the same check on the worker-turn path above. A
			// deliberately-cleared goal is not an error condition
			// regardless of which half of the loop the clear raced with.
			if !s.goalActiveWith(condition) {
				return &GoalResult{Achieved: false, Turns: turn, Reason: "goal cleared"}, nil
			}
			s.clearGoal(fmt.Sprintf("goal evaluator failed: %v", err))
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
//
// # Two independent budgets, chosen by error classification
//
// Every failed attempt is first classified via provider.AsRetryable (never
// by matching error text — see provider/retryable.go). A DETERMINISTIC
// failure (not classified retryable) runs the fast path exactly as
// described above: goalWorkerRetries additional attempts, goalRetryDelay's
// short backoff. A RETRYABLE failure instead runs its own loop, up to
// goalRetryableMaxAttempts attempts, spaced by goalRetryableBackoff's much
// longer jittered schedule (see the doc comment on that function) — and
// these attempts never increment the deterministic counter, so surviving a
// long provider outage costs a turn nothing against goalWorkerRetries.
//
// If the retryable budget is exhausted, this function returns a
// *goalRetryableExhaustedError wrapping the last error, which PursueGoal
// recognizes and parks the turn instead of clearing the goal (see
// PursueGoal's doc comment and goalRetryableExhaustedError's).
//
// The non-idempotency gate below (stop retrying once a tool has executed
// this attempt) applies identically to BOTH budgets: retrying after a tool
// call ran is unsafe regardless of why the subsequent call failed.
func (s *Session) promptTurnWithRetry(ctx context.Context, directive string, turn int) (attempts int, err error) {
	var deterministicAttempt, retryableAttempt int
	for {
		attempts++
		toolsBefore := s.toolExecutions()
		_, perr := s.Prompt(ctx, directive)
		if perr == nil {
			return attempts, nil
		}
		err = perr
		if errors.Is(err, context.Canceled) {
			return attempts, err
		}
		class, retryable := provider.AsRetryable(err)
		// exhausted decides, for a retryable failure, whether THIS attempt
		// is the one that exhausts goalRetryableMaxAttempts — computed
		// before retryableAttempt is incremented below, so the comparison
		// reads as "one more than the retries already spent, including this
		// one, would meet or exceed the ceiling".
		exhausted := retryable && retryableAttempt+1 >= goalRetryableMaxAttempts
		// The tool-execution gate is evaluated BEFORE the stall is
		// journaled so the record's waiting flag tells the truth: an
		// attempt that ran a tool and then failed is about to stop
		// retrying entirely (the non-idempotency doc above) — journaling
		// it as waiting=true, immediately followed by goal.cleared, would
		// read as a park on any timeline keyed to the flag.
		toolGateStops := s.toolExecutions() > toolsBefore
		waiting := retryable && !exhausted && !toolGateStops
		if !s.recordGoalStalled(err, turn, attempts, retryable, class, waiting) {
			// Concurrently cleared: stop retrying, nothing left to retry for.
			return attempts, err
		}
		if provider.IsContextOverflow(err) {
			// Deterministic failure (issue #62): the request as built
			// cannot fit the model's context window, and every later
			// attempt reissues the exact same directive against the exact
			// same (now-too-long) history — retrying is pure waste, not
			// resilience. Fail fast after the single stall record above
			// (overflow is never classified retryable, so its waiting flag
			// is false): no backoff wait, no further attempt. PursueGoal's
			// caller clears the goal with a distinct reason (see its doc
			// comment).
			return attempts, err
		}
		if toolGateStops {
			// This attempt executed a tool call before failing: see the
			// non-idempotency doc above. Retrying would reissue the
			// directive and risk re-running that tool, so stop now instead
			// of waiting and trying again — regardless of classification.
			return attempts, err
		}
		if !retryable {
			deterministicAttempt++
			if deterministicAttempt > goalWorkerRetries {
				return attempts, err
			}
			if werr := waitGoalRetryBackoff(ctx, deterministicAttempt); werr != nil {
				return attempts, werr
			}
			continue
		}
		retryableAttempt++
		if exhausted {
			return attempts, &goalRetryableExhaustedError{err: err, class: class}
		}
		if werr := waitGoalRetryableBackoff(ctx, retryableAttempt); werr != nil {
			return attempts, werr
		}
	}
}

// recordGoalStalled records one failed worker-turn attempt for a turn. Like
// recordGoalEval, it is a no-op — no journal write, no event — when the goal
// is no longer active (a concurrent ClearGoal), so a stalled record can never
// land in the log after goal.cleared. Reports whether the record was
// written, i.e. whether the goal is still active and retrying is worthwhile.
//
// retryable/class/waiting carry the retryable-class classification (see
// promptTurnWithRetry and GitHub issue #61): retryable and class are zero
// on a deterministic-path stall; waiting is true for a retryable stall
// still within its budget ("waiting out provider weather") and false for
// the final retryable stall that reports the budget exhausted (the turn is
// about to park — see PursueGoal's doc comment).
func (s *Session) recordGoalStalled(err error, turn, attempt int, retryable bool, class provider.RetryableClass, waiting bool) bool {
	s.mu.Lock()
	if !s.goalActive {
		s.mu.Unlock()
		return false
	}
	reason := err.Error()
	s.persistGoalLocked(recGoalStalled, goalRecord{
		Reason:         reason,
		Turn:           turn,
		Attempt:        attempt,
		Retryable:      retryable,
		RetryableClass: string(class),
		Waiting:        waiting,
	})
	// Emit while still holding s.mu (see ClearGoal): keeps event order
	// matching log order under a concurrent clear. OnEvent must not call
	// back into this Session — that would deadlock on s.mu, held here.
	s.emit(Event{
		Type: EventGoalStalled, GoalReason: reason, GoalTurn: turn, GoalAttempt: attempt,
		GoalRetryable: retryable, GoalRetryableClass: string(class), GoalWaiting: waiting,
	})
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
