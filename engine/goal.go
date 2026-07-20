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
//
// # Round 6: an evaluator failure must be advisory, not instantly fatal (NEP-4792)
//
// Round 3 (above) closed the "evaluator failure leaves a zombie goal" hole by
// clearing the goal on ANY evaluateGoal error — a provider error, or two
// unparseable replies in a row. That fix traded one incident for another:
// production data showed two fleet boxes die mid-HEALTHY-work because the
// tool-less evaluator call hit a transient provider hiccup (or, once, a
// stretch of oddly-worded replies neither attempt could parse) while the
// worker model itself was making fine progress. Unlike a worker-turn error —
// which is expensive to retry blindly (see promptTurnWithRetry's
// non-idempotency doc) — a failing evaluator call risks nothing by being
// retried or, failing that, simply skipped for one turn: the worker keeps
// working either way, and the ONLY thing a bad verdict can do wrong is delay
// noticing completion, not corrupt anything.
//
// So evaluateGoal itself first tries to ride out the failure in-boundary,
// mirroring promptTurnWithRetry's error classification exactly (see
// runEvaluatorWithRetry): a provider error classified provider.AsRetryable
// gets the SAME retryable schedule and budget the worker turn uses
// (goalRetryableBackoff, goalRetryableMaxAttempts) — the two paths share
// provider weather, so they share a budget's shape, just each keeping its
// own counter. A non-retryable provider error is not retried at all (the
// call is cheap; a permanently broken provider needs the boundary-failure
// path below, not a wasted second attempt). Separately, an unparseable reply
// still gets its original one extra attempt, but that attempt now uses a
// STRICTER system prompt (goalEvaluatorStrictSystem) instead of repeating the
// same instructions verbatim — repeating unchanged instructions to a model
// that already failed to follow them once is exactly why the doubled attempt
// used to buy so little.
//
// If evaluateGoal still returns an error after all that — the retryable
// budget exhausted, a non-retryable error, or two unparseable replies even
// with the stricter re-ask — the boundary "fails", but failing a boundary no
// longer clears the goal. PursueGoal journals a durable goal.eval_failed
// record (carrying the error and the CONSECUTIVE failure count — see
// recordGoalEvalFailed), substitutes a fixed evaluation-unavailable notice
// for the next turn's guidance (never the raw error text, and never a stale
// NOT-MET reason from turns ago — see goalEvalUnavailableNotice), waits a
// short backoff (goalRetryDelay, keyed on the consecutive count, the same
// short schedule the deterministic worker-retry path uses), and `continue`s
// — the worker gets another ordinary turn. A later boundary that DOES parse a
// verdict (MET or NOT MET) resets the consecutive count to zero: the horizon
// below is about a STREAK, not a lifetime total, so one good evaluation
// undoes any number of prior bad ones.
//
// The streak is also paired with the generation it accumulated against
// (evalFailuresGen alongside evalFailures — the same pairing pattern
// reason/reasonGen uses, see the "Round 5" section below), so an UpdateGoal
// mid-streak resets it too: a self-adjust changes what the evaluator is
// even checking, so failures counted against the OLD condition must not
// carry over and let the terminal fire after fewer than
// goalEvalFailureLimit failures against the NEW one. This mirrors the
// server's own fold (server/journal.go's EventGoalUpdated case resets
// GoalSummary.evalFailures to 0) — the engine's loop-local counter now
// agrees with what the server surface already reports. See
// TestEvalFailureStreakResetsOnConditionUpdate.
//
// The horizon has to exist somewhere, though — infinite advisory failures
// would just be Round 3's zombie-goal risk wearing a disguise (a goal that
// LOOKS active but whose evaluator has been dead for hours, silently). After
// goalEvalFailureLimit consecutive failed boundaries, PursueGoal clears the
// goal with a dedicated reason ("goal evaluator failed at N consecutive turn
// boundaries") and returns a distinct sentinel error type
// (*goalEvaluatorExhaustedError) instead of a bare error — a server or other
// caller can tell this terminal apart from an ordinary worker-turn
// exhaustion via errors.As, never by string-matching GoalReason — and,
// unlike every advisory boundary below the horizon, this terminal DOES emit
// session.error: it must be LOUD, since past this point nothing else will
// ever explain the goal's silence.
//
// See TestPursueGoalEvaluatorUnparseableTwiceIsAdvisory,
// TestPursueGoalEvaluatorRetryableErrorRecoversWithinBoundary, and
// TestPursueGoalEvaluatorTerminalAfterConsecutiveFailureLimit.
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

// goalEvaluatorStrictSystem replaces goalEvaluatorSystem for the second
// evaluator attempt within a boundary (see evaluateGoal and goal.go's "Round
// 6" doc section): repeating the exact same instructions to a model that
// already failed to follow them once buys little, so this attempt instead
// calls out the failure explicitly and narrows the instructions to nothing
// but the two-line contract.
const goalEvaluatorStrictSystem = `Your previous reply could not be parsed as a verdict.
Reply with EXACTLY ONE line, in EXACTLY one of these two forms, and nothing else — no other text, no headings, no markdown, no code fences:
MET: <one short sentence saying why>
NOT MET: <one short sentence saying what is still missing>`

// goalPartCap bounds each rendered transcript part so a long tool result cannot
// blow up the evaluator request.
const goalPartCap = 4096

// errEvaluatorUnparseable is returned when two consecutive evaluator replies
// (the second using goalEvaluatorStrictSystem) cannot be parsed. Unlike
// before Round 6, this no longer terminates the loop by itself — see
// evaluateGoal's callers — it just counts as one failed evaluator boundary.
var errEvaluatorUnparseable = errors.New("engine: goal evaluator returned unparseable output twice in a row")

// goalEvalFailureLimit is the number of CONSECUTIVE failed evaluator
// boundaries (see goal.go's "Round 6" doc section) PursueGoal tolerates
// before treating the evaluator as durably broken and clearing the goal. It
// is deliberately much smaller than goalRetryableMaxAttempts: that budget
// already rides out one boundary's worth of provider weather in-boundary,
// so by the time a boundary counts as "failed" at all, something more
// unusual than an ordinary transient hiccup is going on — five separate
// TURNS of it (each potentially minutes apart, each after its own full
// worker turn) is a much stronger signal of a truly broken evaluator than
// exhausting a single boundary's in-boundary retry budget ever is.
const goalEvalFailureLimit = 5

// goalEvalUnavailableNotice replaces the evaluator's feedback in the next
// turn's guidance directive after a failed boundary (see PursueGoal and
// recordGoalEvalFailed): the worker must never see the raw error text (an
// implementation detail, possibly carrying provider internals) nor a stale
// NOT-MET reason left over from a much earlier successful evaluation — both
// would be misleading about what actually happened.
const goalEvalUnavailableNotice = "the evaluator could not render a verdict for the last turn; continue working toward the goal and finish it"

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

// goalEvaluatorExhaustedError is returned by PursueGoal when the evaluator has
// failed at goalEvalFailureLimit consecutive turn boundaries (see goal.go's
// "Round 6" doc section) — a durable, probably-permanent evaluator outage,
// distinct from every failed boundary below that horizon (which is advisory
// only: no error returned, no clear, the loop just continues). A caller (the
// server, in particular) recognizes this type via errors.As and maps it to a
// dedicated turn.end outcome instead of string-matching GoalReason — see
// server/journal.go's outcomeEvaluatorExhausted (Task 2).
type goalEvaluatorExhaustedError struct {
	err      error
	failures int
}

func (e *goalEvaluatorExhaustedError) Error() string {
	return fmt.Sprintf("engine: goal evaluator failed at %d consecutive turn boundaries: %v", e.failures, e.err)
}
func (e *goalEvaluatorExhaustedError) Unwrap() error { return e.err }

// IsGoalEvaluatorExhausted reports whether err is (or wraps, via errors.As)
// the sentinel PursueGoal returns once the evaluator has failed at
// goalEvalFailureLimit consecutive turn boundaries (see
// goalEvaluatorExhaustedError above) — the one hook a caller (the server, in
// particular) needs to map this terminal onto its own outcome without
// reaching into the unexported type itself or string-matching GoalReason.
// Mirrors provider.IsContextOverflow's shape (provider/errors.go).
func IsGoalEvaluatorExhausted(err error) bool {
	var ee *goalEvaluatorExhaustedError
	return errors.As(err, &ee)
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
// error is returned — UNLESS the error is classified provider-retryable
// (see provider.AsRetryable and GitHub issue #61), in which case
// promptTurnWithRetry instead runs a much longer, separately-budgeted
// backoff (goalRetryableMaxAttempts), and exhausting THAT budget parks the
// turn (retries the same directive on the next ordinary turn) rather than
// clearing the goal — see the package doc's "Round 4" section for the full
// rationale and the state diagram. A cancelled context is never retried or
// treated as a worker failure — it is a deliberate abort (DELETE /goal,
// shutdown drain) and is returned immediately with the goal left exactly as
// it was, since a drain must be resumable.
//
// A failing evaluator call (a provider error, or two unparseable replies in a
// row even after the stricter re-ask) is advisory, not fatal — see the
// package doc's "Round 6" section. evaluateGoal already rides out a
// retryable-class provider error on its own in-boundary backoff
// (runEvaluatorWithRetry); if it still returns an error, PursueGoal journals
// a goal.eval_failed record carrying the CONSECUTIVE failure count, replaces
// the next turn's guidance reason with a fixed evaluation-unavailable notice
// (goalEvalUnavailableNotice — never the raw error text, never a stale
// NOT-MET reason), waits a short backoff, and continues: the goal stays
// active and the worker gets another ordinary turn. Only once
// goalEvalFailureLimit consecutive boundaries have failed does PursueGoal
// clear the goal (a dedicated reason distinct from a worker-turn failure's)
// and return a *goalEvaluatorExhaustedError instead of a bare error — that
// terminal, and only that terminal, also emits session.error. A cancelled
// context is never retried or counted as a failed boundary, same rule as the
// worker-turn path above. A concurrent ClearGoal or UpdateGoal racing an
// in-flight evaluator call is handled exactly like the same race on the
// worker-turn and ordinary-verdict paths (see goalStatus): a clean stop or a
// silently discarded stale outcome, respectively — never a failed-boundary
// record for a generation that is no longer current.
//
// # Self-adjust: the condition is re-read every turn boundary
//
// PursueGoal does not trust its own condition parameter once the loop is
// running — it is only the value used to (maybe) register the goal at the
// very start. Every turn boundary instead takes a fresh goalSnapshot
// (condition, goalGen, active) under s.mu, and that snapshot's condition —
// not the parameter — drives that turn's directive, the guidance template,
// and the evaluator call. A concurrent UpdateGoal (self-adjust: the goal
// tool's "adjust" action, or an operator's POST /goal on a running loop)
// therefore redirects the very next turn instead of being invisible to it
// or, worse, being conflated with a clear.
//
// This also closes a narrow race the old condition-equality check could not:
// an evaluator call or worker turn started against generation N can finish
// AFTER an UpdateGoal has already moved the goal to generation N+1. Its
// verdict is stale — computed against a condition that is no longer current
// — and must never be journaled or acted on, but the goal is still very much
// active, so treating this as "goal cleared" would be wrong too. goalStatus
// reports this third case explicitly (active-but-stale), and every point
// that used to check "was this cleared while I was working" now also checks
// "is this stale": a stale outcome is silently discarded (no goal.eval, no
// goal.stalled, no achieve, no clear) and the loop simply continues to the
// next turn, which re-snapshots and picks up the new condition. See
// goalSnapshot and goalStatus's doc comments, and
// TestPursueGoalPicksUpUpdatedConditionNextTurn /
// TestStaleMetVerdictDiscarded / TestClearGoalStillStopsUpdatedLoop.
//
// # Round 5: a discarded turn must not leak its stale reason into the next directive
//
// Silently discarding a stale outcome (the section above) closes the
// journaling half of the problem, but a live end-to-end run surfaced a
// second half it didn't: `reason` — the last NOT MET evaluator feedback,
// carried into goalGuidance for the next turn's directive — was declared
// once outside the loop and only ever reassigned on the ordinary, non-stale
// NOT MET path. Every stale-discard `continue` (worker-turn failure,
// evaluator failure, or a discarded evaluator verdict) skipped that
// reassignment, so the turn AFTER a discard built its directive from
// whatever `reason` happened to hold from the last turn that completed
// normally — which can be describing state that is no longer true. The
// repro: turn 1's evaluator said "the file PROOF_A.txt does not exist";
// turn 2 created the file and self-adjusted the goal (via the goal tool),
// making turn 2's own evaluator verdict stale and discarded; turn 3's
// directive nonetheless repeated turn 1's now-false "does not exist"
// feedback verbatim, costing an extra turn re-litigating something already
// done.
//
// The fix: `reason` is only ever valid paired with the generation it was
// produced for. A second variable, reasonGen, is set alongside `reason`
// every time (only) the ordinary NOT MET path assigns it, to that turn's
// snap.gen. Building the next turn's directive compares reasonGen against
// that turn's OWN fresh snapshot: a match reuses `reason` as before: a
// mismatch — which covers every stale-discard site by construction (each
// leaves reasonGen unchanged while the discard that caused it bumped
// goalGen) AND the narrower case of a generation change that happens
// between two turns with no discard at all (e.g. an UpdateGoal landing in
// the gap after turn N ends and before turn N+1 snapshots) — substitutes
// goalAdjustedNotice, an explicit "the goal changed, prior feedback no
// longer applies" directive, instead of ever reusing a reason paired with a
// different generation. See goalAdjustedNotice and
// TestStaleDiscardReplacesReasonWithAdjustmentNotice.
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
		if !s.goalActiveNow() {
			return &GoalResult{Achieved: false, Turns: 0, Reason: "goal cleared"}, nil
		}
	} else if err := s.RegisterGoal(condition); err != nil {
		s.emitSessionError(err)
		return nil, err
	}

	var (
		reason    string // last NOT MET reason, carried into the next turn's guidance
		reasonGen uint64 // generation `reason` was produced at; see the pairing rule below
		// evalFailures counts CONSECUTIVE failed evaluator boundaries (see
		// the package doc's "Round 6" section and recordGoalEvalFailed):
		// reset to zero the moment a later boundary parses a verdict (MET or
		// NOT MET), and left untouched across a stale-discard (the failure
		// was against a generation that is no longer current, so it never
		// happened for THIS streak's purposes — see the evaluator-failure
		// branch below). Reaching goalEvalFailureLimit is the terminal
		// horizon.
		//
		// evalFailuresGen is only ever valid paired with evalFailures — the
		// generation the streak was accumulated against — exactly the
		// reason/reasonGen pairing rule below, applied to the OTHER piece of
		// state a stale-discard can leave dangling. Every failed-boundary
		// site compares it against that turn's OWN fresh snap.gen before
		// adding to the count: a match continues the streak, a mismatch
		// (an UpdateGoal moved the goal to a new generation since
		// evalFailures was last set) starts a fresh streak at 1 instead of
		// silently carrying a count accumulated against a condition the
		// evaluator is no longer even checking — see the evaluator-failure
		// branch below and TestEvalFailureStreakResetsOnConditionUpdate.
		evalFailures    int
		evalFailuresGen uint64
	)
	for turn := 1; opts.MaxTurns == 0 || turn <= opts.MaxTurns; turn++ {
		// Per-turn-boundary snapshot (see goalSnapshot's doc comment): this
		// is the single source of truth for the rest of this iteration,
		// deliberately NOT the condition parameter or a value carried over
		// from a previous iteration.
		snap := s.snapshotGoal()
		if !snap.active {
			// Cleared between registration and this turn (or mid-loop by a
			// concurrent DELETE): clean stop, no turn runs.
			return &GoalResult{Achieved: false, Turns: turn - 1, Reason: "goal cleared"}, nil
		}
		// Drain the ENTIRE prompt queue, FIFO, in one locked operation
		// (dequeueAllLocked via DequeueAllPrompts) — right here, at the turn
		// boundary, and only now that a turn is actually about to run (a
		// drain above the !snap.active return would dequeue-and-discard a
		// prompt for a turn that never happens; better to leave it queued
		// for the next natural drain trigger, e.g. Task 3's idle dispatch).
		// Every drained prompt journals its own prompt.dequeued(injected)
		// record before this turn's directive is built, let alone sent — see
		// the plan's locked decision "Dequeue journals BEFORE the text
		// enters any turn": replay can never double-deliver, and a prompt
		// injected here is considered DELIVERED the moment it is folded into
		// directive below, even if this turn's outcome later turns out to be
		// stale and gets discarded (the worker-turn/evaluator stale-discard
		// `continue` sites below). A discarded turn's directive was still
		// really sent to (and seen by) the worker model — injected prompts
		// are never restored to the queue on a stale discard, only ever
		// delivered once. See TestInjectedPromptsNotRedeliveredAfterStaleDiscard.
		queued := s.DequeueAllPrompts("injected")
		// `reason` is only ever valid paired with the generation it was
		// produced for (reasonGen, set alongside it below). Every one of
		// this loop's stale-discard `continue` sites — a worker-turn
		// failure, an evaluator failure, or a discarded evaluator verdict —
		// leaves `reason` untouched, so without this check the NEXT turn's
		// directive would silently repeat a reason that describes a
		// condition or transcript state that is no longer current (the live
		// incident this guards: turn 3 repeated turn 1's "the file does not
		// exist" feedback verbatim, one turn after turn 2 had created the
		// file and self-adjusted the goal). The same rule also covers a
		// generation change that happens WITHOUT any discard — e.g. an
		// UpdateGoal landing in the gap between turn N ending and turn N+1's
		// snapshot — since the check is purely "does this turn's generation
		// match the one `reason` was produced for", not "was there a
		// discard". See TestStaleDiscardReplacesReasonWithAdjustmentNotice.
		directive := snap.condition
		if turn > 1 {
			if reasonGen == snap.gen {
				directive = goalGuidance(snap.condition, reason)
			} else {
				directive = goalGuidance(snap.condition, goalAdjustedNotice)
			}
		}
		if len(queued) > 0 {
			// Prepend, never replace: the goal directive/guidance below is
			// still exactly what it would have been with no queue activity
			// at all — this only adds a clearly labeled block ahead of it.
			// The evaluator's CONDITION field is built from snap.condition
			// alone (see evaluateGoal/runEvaluator) and never includes this
			// block or `directive` itself, so "goal injection judges only
			// the goal" holds for that field structurally, not by
			// convention. This block is NOT hidden from the evaluator
			// overall, though: runEvaluator's CONVERSATION TRANSCRIPT field
			// renders the full history (renderConversation(s.History())),
			// which includes this turn's directive — and therefore this
			// block — once the worker turn that received it has run. Only
			// the condition string itself stays clean.
			directive = operatorMessagesBlock(queued, operatorContextGoal) + directive
		}
		if attempts, err := s.promptTurnWithRetry(ctx, directive, turn, snap.gen); err != nil {
			if errors.Is(err, context.Canceled) {
				// Deliberate abort: leave the goal exactly as it is (a
				// drain must be resumable), no goal.stalled, no clear.
				return nil, err
			}
			active, stale := s.goalStatus(snap.gen)
			if !active {
				// Cleared concurrently (DELETE /goal) while a retry was in
				// flight: clean stop, same as the checks above/below.
				return &GoalResult{Achieved: false, Turns: turn - 1, Reason: "goal cleared"}, nil
			}
			if stale {
				// UpdateGoal moved the goal to a new generation while this
				// turn's retries were in flight: this turn's failure was
				// attributed to a condition that is no longer current, so it
				// must not clear the (still active, just redirected) goal —
				// discard silently and let the next iteration's fresh
				// snapshot pick up the new condition.
				continue
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
			failReason := fmt.Sprintf("worker turn failed after %d attempt(s): %v", attempts, err)
			s.clearGoal(failReason)
			return nil, fmt.Errorf("engine: goal loop stalled: %w", err)
		}
		met, evalReason, err := s.evaluateGoal(ctx, snap.condition, opts.Evaluator)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				// Deliberate abort (or a cancelled ctx surfacing through the
				// evaluator's own stream): same rule as the worker-turn
				// path above — leave the goal exactly as it is, since a
				// drain must be resumable.
				return nil, err
			}
			// Round 6 (NEP-4792): a failing evaluator call is advisory, not
			// fatal — see the package doc. evaluateGoal already spent its own
			// in-boundary retry budget before returning here (a
			// retryable-class provider error rode out
			// runEvaluatorWithRetry's backoff; an unparseable reply got its
			// one stricter re-ask), so reaching this point means this
			// BOUNDARY has failed — not that the goal has.
			//
			// recordGoalEvalFailed follows recordGoalEval's own convention
			// exactly (see below): attempt the write with the CANDIDATE
			// count first, and only consult goalStatus if the write is
			// refused, to tell a concurrent ClearGoal (clean stop) apart
			// from a concurrent UpdateGoal (the failure was against a
			// generation that is no longer current — discard silently,
			// evalFailures left untouched, exactly like every other
			// stale-discard site in this loop).
			//
			// The candidate is built off evalFailures ONLY when
			// evalFailuresGen still matches THIS turn's snap.gen (see the
			// pairing rule on the var block above): a mismatch means the
			// running count was accumulated against a condition an
			// UpdateGoal has since moved past, so it must not carry into a
			// streak against the new one — start fresh at 1 instead of
			// letting the terminal fire early against a horizon it never
			// actually earned.
			candidateBase := 0
			if evalFailuresGen == snap.gen {
				candidateBase = evalFailures
			}
			candidateFailures := candidateBase + 1
			if !s.recordGoalEvalFailed(err, turn, candidateFailures, snap.gen) {
				if _, stale := s.goalStatus(snap.gen); stale {
					continue
				}
				return &GoalResult{Achieved: false, Turns: turn, Reason: "goal cleared"}, nil
			}
			evalFailures, evalFailuresGen = candidateFailures, snap.gen
			if evalFailures >= goalEvalFailureLimit {
				// The terminal horizon: a durable, probably-permanent
				// evaluator outage, not the ordinary provider weather
				// evaluateGoal's own in-boundary retry already absorbs.
				// Unlike every advisory boundary below this, the terminal
				// DOES clear the goal (a reason distinct from a worker-turn
				// failure's, so a reader never has to guess which half of
				// the loop gave up) and DOES emit session.error — it must be
				// LOUD, since past this point nothing else will ever explain
				// the goal's silence, the exact failure mode Round 3 closed
				// for a single failure and this horizon exists to close
				// again at N consecutive ones.
				clearReason := fmt.Sprintf("goal evaluator failed at %d consecutive turn boundaries", evalFailures)
				s.clearGoal(clearReason)
				exhaustedErr := &goalEvaluatorExhaustedError{err: err, failures: evalFailures}
				s.emitSessionError(exhaustedErr)
				return nil, exhaustedErr
			}
			// Below the horizon: wait the same short backoff the
			// deterministic worker-retry path uses (goalRetryDelay), keyed
			// on the consecutive-failure count, then continue — the goal
			// stays active and the worker gets another ordinary turn.
			if werr := waitGoalRetryBackoff(ctx, evalFailures); werr != nil {
				return nil, werr
			}
			// Never leak the raw error into the worker's directive, and
			// never repeat a reason paired with an earlier generation (see
			// the reasonGen pairing rule above) — substitute the fixed
			// evaluation-unavailable notice instead.
			reason, reasonGen = goalEvalUnavailableNotice, snap.gen
			continue
		}
		evalFailures, evalFailuresGen = 0, snap.gen
		if !s.recordGoalEval(met, evalReason, turn, snap.gen) {
			// Either ClearGoal fired while this evaluation was in flight (the
			// goal is no longer active, so its verdict must not land in the
			// journal — clean stop, never an achievement), or an UpdateGoal
			// moved the goal to a new generation while it was in flight (the
			// verdict is stale — computed against a condition that is no
			// longer current — so it is discarded silently and the loop
			// continues against the new condition instead of stopping).
			if _, stale := s.goalStatus(snap.gen); stale {
				continue
			}
			return &GoalResult{Achieved: false, Turns: turn, Reason: "goal cleared"}, nil
		}
		if met {
			if !s.achieveGoal(evalReason, turn, snap.gen) {
				// Same two possibilities as recordGoalEval above, in the
				// narrow window between it and this call: a concurrent clear
				// (clean stop, not an achievement) or a concurrent update
				// (stale MET verdict, discarded, loop continues).
				if _, stale := s.goalStatus(snap.gen); stale {
					continue
				}
				return &GoalResult{Achieved: false, Turns: turn, Reason: "goal cleared"}, nil
			}
			return &GoalResult{Achieved: true, Turns: turn, Reason: evalReason}, nil
		}
		reason = evalReason
		reasonGen = snap.gen
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
//
// gen is the calling turn's goalSnapshot generation, threaded straight
// through to recordGoalStalled so a stall record for an attempt is never
// journaled once an UpdateGoal has moved the goal past this turn's
// generation — see recordGoalStalled and PursueGoal's stale-discard handling.
func (s *Session) promptTurnWithRetry(ctx context.Context, directive string, turn int, gen uint64) (attempts int, err error) {
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
		if !s.recordGoalStalled(err, turn, attempts, retryable, class, waiting, gen) {
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
// is no longer active (a concurrent ClearGoal) OR when it is active but at a
// different generation than gen (a concurrent UpdateGoal moved past this
// turn's snapshot — see goalSnapshot/goalStatus), so a stalled record can
// never land in the log after goal.cleared, and a stale attempt's failure is
// never attributed to a condition that is no longer current. Reports whether
// the record was written, i.e. whether the goal is still active, at the same
// generation, and retrying is worthwhile.
//
// retryable/class/waiting carry the retryable-class classification (see
// promptTurnWithRetry and GitHub issue #61): retryable and class are zero
// on a deterministic-path stall; waiting is true for a retryable stall
// still within its budget ("waiting out provider weather") and false for
// the final retryable stall that reports the budget exhausted (the turn is
// about to park — see PursueGoal's doc comment).
func (s *Session) recordGoalStalled(err error, turn, attempt int, retryable bool, class provider.RetryableClass, waiting bool, gen uint64) bool {
	s.mu.Lock()
	if !s.goalActive || s.goalGen != gen {
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
// structurally impossible. Errors if a goal is already active. The condition
// is stored trimmed (matching UpdateGoal, so its same-condition no-op check
// compares like with like).
//
// Bumps goalGen so a PursueGoal loop that snapshotted the PREVIOUS goal (now
// cleared or achieved) never mistakes this freshly-registered one for a
// continuation of it — see goalSnapshot.
func (s *Session) RegisterGoal(condition string) error {
	trimmed := strings.TrimSpace(condition)
	if trimmed == "" {
		return errors.New("engine: RegisterGoal requires a non-empty condition")
	}
	s.mu.Lock()
	if s.goalActive {
		cur := s.goalCondition
		s.mu.Unlock()
		return fmt.Errorf("engine: a goal is already active: %q", cur)
	}
	s.goalActive = true
	s.goalCondition = trimmed
	s.goalGen++
	s.persistGoalLocked(recGoalSet, goalRecord{Condition: trimmed})
	// Emit while holding s.mu (see ClearGoal): event order matches log
	// order. OnEvent must not call back into this Session.
	s.emit(Event{Type: EventGoalSet, GoalCondition: trimmed})
	s.mu.Unlock()
	return nil
}

// UpdateGoal rewrites the condition of an already-active goal: it journals a
// goal.updated record and emits EventGoalUpdated, following RegisterGoal's
// persist-and-emit-while-holding-s.mu shape exactly. It errors if no goal is
// currently active — use RegisterGoal to start one. Updating to the exact
// same condition (after trimming) is a silent no-op: nil error, no record, no
// event, since nothing actually changed.
//
// Bumps goalGen (skipped on the same-condition no-op, since nothing to
// re-snapshot against changed). A running PursueGoal loop picks up the new
// condition at its next turn boundary (see goalSnapshot), and any evaluator
// verdict or worker-turn outcome still in flight against the OLD generation
// is discarded rather than journaled — see PursueGoal's doc comment and
// goalStatus.
func (s *Session) UpdateGoal(condition string) error {
	trimmed := strings.TrimSpace(condition)
	if trimmed == "" {
		return errors.New("engine: UpdateGoal requires a non-empty condition")
	}
	s.mu.Lock()
	if !s.goalActive {
		s.mu.Unlock()
		return errors.New("engine: no active goal to update")
	}
	if s.goalCondition == trimmed {
		s.mu.Unlock()
		return nil
	}
	s.goalCondition = trimmed
	s.goalGen++
	s.persistGoalLocked(recGoalUpdated, goalRecord{Condition: trimmed})
	// Emit while holding s.mu (see ClearGoal): event order matches log
	// order. OnEvent must not call back into this Session.
	s.emit(Event{Type: EventGoalUpdated, GoalCondition: trimmed})
	s.mu.Unlock()
	return nil
}

// goalSnapshot is PursueGoal's per-turn-boundary read of goal state — the
// condition, the generation that condition was established at (see goalGen's
// field comment), and whether the goal is still active — taken together
// under a single s.mu critical section (see snapshotGoal) so a turn's
// directive, evaluator call, and post-evaluation bookkeeping all agree on
// exactly one version of the goal, never a torn mix of an old condition with
// a new gen or vice versa. A concurrent UpdateGoal or ClearGoal is always
// observed at the NEXT snapshot (the top of the next turn), never mid-turn.
type goalSnapshot struct {
	condition string
	gen       uint64
	active    bool
}

// snapshotGoal takes a goalSnapshot under s.mu.
func (s *Session) snapshotGoal() goalSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return goalSnapshot{condition: s.goalCondition, gen: s.goalGen, active: s.goalActive}
}

// goalActiveNow reports whether a goal is currently active, with no
// generation check — used only where no snapshot/generation is yet in play
// (the pre-loop registered-vs-cleared race check in PursueGoal). Everywhere
// a turn is already underway, goalStatus is the right call instead: it also
// reports staleness against that turn's snapshot.
func (s *Session) goalActiveNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goalActive
}

// goalStatus reports whether the goal is currently active, and — only
// meaningful when active is true — whether it is "stale" relative to gen:
// active but at a DIFFERENT generation, meaning a concurrent UpdateGoal
// rewrote the condition after gen was snapshotted. PursueGoal uses this to
// tell apart the three ways an in-flight worker-turn or evaluator outcome
// can no longer be trusted:
//
//   - !active: the goal was cleared — clean stop (existing "goal cleared"
//     exit).
//   - active && stale: the goal was updated, not cleared — the in-flight
//     outcome is discarded silently (no journal write) and the loop
//     continues, picking up the new condition at its next snapshot.
//   - active && !stale: nothing changed — proceed normally.
func (s *Session) goalStatus(gen uint64) (active, stale bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goalActive, s.goalActive && s.goalGen != gen
}

// recordGoalEval records one evaluator verdict for a turn. It is a no-op —
// no journal write, no event — when the goal is no longer active (a
// concurrent ClearGoal may have raced this evaluation to completion, and its
// verdict must never land in the log after goal.cleared) OR when it is active
// but at a different generation than gen (a concurrent UpdateGoal moved the
// goal past this turn's snapshot — see goalSnapshot/goalStatus — so the
// verdict is stale and must be discarded, not journaled, even though the
// goal is still active). Reports whether the record was written.
func (s *Session) recordGoalEval(met bool, reason string, turn int, gen uint64) bool {
	s.mu.Lock()
	if !s.goalActive || s.goalGen != gen {
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

// recordGoalEvalFailed records one failed evaluator boundary (see goal.go's
// "Round 6" doc section and evaluateGoal/runEvaluatorWithRetry): a provider
// error the in-boundary retryable retry couldn't ride out, or two
// consecutive unparseable replies even with the stricter re-ask.
// consecutiveFailures is the CANDIDATE count for this boundary (the caller's
// running count plus one) — passed in rather than read from Session state
// because the count is PursueGoal's own loop-local bookkeeping, not
// something this method could reconstruct on its own.
//
// Like recordGoalEval and recordGoalStalled, it is a no-op — no journal
// write, no event — when the goal is no longer active (a concurrent
// ClearGoal) OR when it is active but at a different generation than gen (a
// concurrent UpdateGoal moved the goal past this turn's snapshot — see
// goalSnapshot/goalStatus), so a failed boundary is never attributed to a
// condition that is no longer current and never lands in the log after
// goal.cleared. Reports whether the record was written — PursueGoal only
// commits consecutiveFailures to its own running counter when this reports
// true (see its caller), leaving the counter untouched on a stale discard.
func (s *Session) recordGoalEvalFailed(err error, turn, consecutiveFailures int, gen uint64) bool {
	s.mu.Lock()
	if !s.goalActive || s.goalGen != gen {
		s.mu.Unlock()
		return false
	}
	reason := err.Error()
	s.persistGoalLocked(recGoalEvalFailed, goalRecord{Reason: reason, Turn: turn, EvalFailures: consecutiveFailures})
	// Emit while still holding s.mu (see ClearGoal): keeps event order
	// matching log order under a concurrent clear. OnEvent must not call
	// back into this Session — that would deadlock on s.mu, held here.
	s.emit(Event{Type: EventGoalEvalFailed, GoalReason: reason, GoalTurn: turn, GoalEvalFailures: consecutiveFailures})
	s.mu.Unlock()
	return true
}

// achieveGoal records goal.achieved and clears the active goal. It is a
// no-op when the goal is no longer active (already cleared concurrently, so
// a cleared-then-achieved sequence can never reach the log) OR when it is
// active but at a different generation than gen (a concurrent UpdateGoal
// moved the goal past this turn's snapshot — the MET verdict is stale and
// must never achieve a goal the caller has since redirected). Reports
// whether the goal was achieved.
func (s *Session) achieveGoal(reason string, turns int, gen uint64) bool {
	s.mu.Lock()
	if !s.goalActive || s.goalGen != gen {
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

// evaluateGoal runs a single boundary's evaluator check and parses its
// verdict, retrying once on an unparseable reply — the second attempt uses
// goalEvaluatorStrictSystem instead of repeating goalEvaluatorSystem verbatim
// (see the package doc's "Round 6" section: a model that already failed to
// follow the instructions once is unlikely to follow them again unchanged).
// Two unparseable replies in a row return errEvaluatorUnparseable. Each
// attempt is itself run through runEvaluatorWithRetry, which rides out a
// provider error classified provider.AsRetryable on its own in-boundary
// backoff before this loop ever sees it; what surfaces here (a non-retryable
// provider error, a retryable one whose budget is exhausted, or
// errEvaluatorUnparseable) is, by construction, this BOUNDARY's failure —
// see PursueGoal's caller for what that means (advisory, not fatal, below
// goalEvalFailureLimit consecutive occurrences). A context.Canceled error is
// never retried at any layer and surfaces immediately.
func (s *Session) evaluateGoal(ctx context.Context, condition string, evaluator message.ModelRef) (met bool, reason string, err error) {
	systemPrompts := [2]string{goalEvaluatorSystem, goalEvaluatorStrictSystem}
	for attempt := 0; attempt < 2; attempt++ {
		out, err := s.runEvaluatorWithRetry(ctx, condition, evaluator, systemPrompts[attempt])
		if err != nil {
			return false, "", err
		}
		if m, r, ok := parseEvaluation(out); ok {
			return m, r, nil
		}
	}
	return false, "", errEvaluatorUnparseable
}

// runEvaluatorWithRetry issues one evaluator request, retrying a provider
// error classified provider.AsRetryable on the exact same budget and backoff
// schedule promptTurnWithRetry uses for the worker turn
// (goalRetryableMaxAttempts, goalRetryableBackoff/waitGoalRetryableBackoff —
// see GitHub issue #61 and the package doc's "Round 4" section) — the two
// paths ride out the same shared provider weather, so they share a budget's
// shape, each keeping its own counter. A non-retryable provider error is
// returned immediately, unretried: unlike a worker turn, the evaluator call
// is cheap and tool-less, so this budget exists purely to survive transient
// weather, not to paper over a provider that is permanently broken — see
// evaluateGoal's caller, PursueGoal, for what happens when this ultimately
// returns an error (the boundary counts as failed; it is never immediately
// fatal on its own). context.Canceled is never retried, matching every other
// backoff wait in this package.
func (s *Session) runEvaluatorWithRetry(ctx context.Context, condition string, evaluator message.ModelRef, systemPrompt string) (string, error) {
	var retryableAttempt int
	for {
		out, err := s.runEvaluator(ctx, condition, evaluator, systemPrompt)
		if err == nil {
			return out, nil
		}
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		if _, retryable := provider.AsRetryable(err); !retryable {
			return "", err
		}
		retryableAttempt++
		if retryableAttempt >= goalRetryableMaxAttempts {
			return "", err
		}
		if werr := waitGoalRetryableBackoff(ctx, retryableAttempt); werr != nil {
			return "", werr
		}
	}
}

// runEvaluator issues one tool-less completion check on the evaluator model and
// returns its raw text. systemPrompt is goalEvaluatorSystem on the first
// attempt within a boundary and goalEvaluatorStrictSystem on the second (see
// evaluateGoal).
func (s *Session) runEvaluator(ctx context.Context, condition string, evaluator message.ModelRef, systemPrompt string) (string, error) {
	prov, err := s.cfg.Providers.For(evaluator)
	if err != nil {
		return "", err
	}
	content := "GOAL CONDITION:\n" + condition + "\n\nCONVERSATION TRANSCRIPT:\n" + renderConversation(s.History())
	req := &provider.Request{
		Model:  evaluator,
		System: []string{systemPrompt},
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

// goalAdjustedNotice replaces a carried-over evaluator reason in
// goalGuidance whenever the goal's generation changed since that reason was
// produced (see PursueGoal's reasonGen bookkeeping). A stale reason
// describes state as of an earlier, possibly now-obsolete condition — the
// live incident this guards had turn 3's directive repeat turn 1's "the file
// does not exist" feedback verbatim after turn 2 had already created the
// file and self-adjusted the goal, costing an extra turn re-litigating
// something already true. Reusing goalGuidance's own fixed-template tone
// rather than inventing a new shape.
const goalAdjustedNotice = "the goal condition changed since the last evaluation; disregard the previous evaluator feedback and re-assess against the current goal below"

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
