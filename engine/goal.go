// Package engine runs headless agent sessions.
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
// before advisory evaluator handling, this no longer terminates the loop by itself — see
// evaluateGoal's callers — it just counts as one failed evaluator boundary.
var errEvaluatorUnparseable = errors.New("engine: goal evaluator returned unparseable output twice in a row")

// goalStreamTruncatedMaxAttempts bounds worker-turn attempts whose failure
// is classified provider.RetryableStreamTruncated — a response stream that
// died before its terminal event. Truncation is retryable (the 2026-08-06
// incident's truncated turns were followed by clean successes minutes
// later on the same model — the cut was a gateway's per-response ceiling,
// not a dead provider) but it is NOT weather: waiting longer does not
// raise a stream ceiling, and every retry re-prompts a full turn at full
// input cost, so it must never ride goalRetryableMaxAttempts' 12-attempt/
// ~30-minute schedule. Three attempts on the short goalRetryDelay
// schedule (~5s of waiting total) is enough to survive an isolated cut;
// exhaustion PARKS, same as every other tier, so a persistent ceiling
// still never kills the goal.
const goalStreamTruncatedMaxAttempts = 3

// goalEvalFailureLimit is the number of CONSECUTIVE failed evaluator
// boundaries (see evaluator failure handling) PursueGoal tolerates
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

// goalProviderExhaustedMaxAttempts bounds a single turn's provider-exhausted
// attempts (see provider.AsProviderExhausted, provider/errors.go) before
// promptTurnWithRetry gives up and PursueGoal parks the turn — the same
// weather-shaped budget goalRetryableMaxAttempts uses for ordinary
// overloaded/rate_limited/server_error weather, kept as its own named
// constant (and its own attempt counter — see promptTurnWithRetry) so a
// concurrent overload spell and an account wall in the same turn never
// share, and silently steal from, one another's budget.
//
// Before this constant existed, a provider-exhausted error was wrapped
// provider.MarkPermanent by the adapter (see provider/anthropic/anthropic.go:
// an account-level usage-limit rejection has no distinct HTTP status or wire
// error type, so the adapter can only classify it after regex-matching the
// message) and promptTurnWithRetry's fail-fast permanent branch treated it
// exactly like a structurally malformed request: ONE attempt, no backoff,
// immediate park. That is correct for a malformed request (retrying an
// identical request fails identically forever) but wrong here — an account
// wall lifts on its own, unchanged, the moment the provider's own clock
// rolls over (see provider.ErrKindProviderExhausted's doc comment) — so
// fail-fast silently killed goal supervision on the very first usage-limit
// rejection instead of giving the wall any chance to clear. Live evidence:
// box bx-01m0x8996 parked after "1 permanent-tier attempt(s)" on "You have
// reached your specified API usage limits" and never resumed without an
// operator DELETE + re-register.
//
// RecoverHint (the provider's own "you regain access on <date>" statement)
// is deliberately NEVER parsed into a wait duration — see
// provider.Error.RecoverHint's doc comment: the format varies by provider
// and by plan, so computing an exact wake time from it would be guessing
// dressed as precision. This tier instead rides the exact same jittered
// backoff schedule (goalRetryableBackoff/waitGoalRetryableBackoff) ordinary
// weather uses. goalRetryableMaxAttempts' own doc comment already argues
// this shape correctly: it trades a bounded, generous wait (~30 minutes
// worst case) against the alternative of an unbounded, unattended hold on
// the run slot for however long a quota happens to be spent — a wall that
// clears in seconds (a burst rate limit that reached this classification)
// or minutes resumes automatically within this budget; a wall measured in
// hours or days still exhausts it and parks, honestly classified (see
// goalClassProviderExhausted/classifyGoalWorkerError) rather than either
// pinning the run slot for the outage's full, unknown duration (the exact
// GitHub issue #61 shape current worker-failure handling rejected — see
// goalRetryableExhaustedError's doc comment) or silently dying on attempt
// one as it did before this fix.
const goalProviderExhaustedMaxAttempts = goalRetryableMaxAttempts

// classifyProviderExhausted re-derives retryable/class for a
// provider-exhausted error into the goal loop's local classification
// bookkeeping (see goalClassProviderExhausted's doc comment below), shared
// by promptTurnWithRetry and PursueGoal's worker-turn error handling so the
// two sites can never independently drift on what counts as
// provider-exhausted or which class value marks it — a review finding on
// the fix that introduced this tier: the override was duplicated verbatim
// at both call sites. retryable/class are the caller's own
// provider.AsRetryable(err) result, passed through unchanged when err is
// not provider-exhausted; providerExhausted reports which branch fired, for
// callers (promptTurnWithRetry) that need it for their own dispatch beyond
// just retryable/class.
func classifyProviderExhausted(err error, retryable bool, class provider.RetryableClass) (newRetryable bool, newClass provider.RetryableClass, providerExhausted bool) {
	if _, ok := provider.AsProviderExhausted(err); ok {
		return true, goalClassProviderExhausted, true
	}
	return retryable, class, false
}

// goalClassProviderExhausted is the local provider.RetryableClass marker
// used to record a provider-exhausted worker-turn failure through the
// EXISTING retryable/class bookkeeping (goalWorkerParkedError,
// recordGoalParked, classifyGoalWorkerError, the goal.stalled/goal.parked
// records and their matching events) instead of adding a fourth boolean or
// a new record field throughout this file. provider.AsRetryable(err) itself
// never returns this — a provider-exhausted error is wrapped
// provider.MarkPermanent, not provider.MarkRetryable (see
// provider.ErrKindProviderExhausted's doc comment: adapters mark it
// permanent for ordinary HTTP-retry purposes, since no short backoff
// schedule outlives a monthly quota) — but for THIS package's purposes it
// behaves like weather, not a doomed request, so promptTurnWithRetry and
// PursueGoal's worker-turn handling both fold it into their local
// retryable/class variables explicitly (see the "provider-exhausted" branch
// in each). It is not one of provider/retryable.go's real RetryableClass
// values, so a reader of a goal.stalled/goal.parked record's
// RetryableClass field sees it clearly labeled apart from
// overloaded/rate_limited/server_error/stream_truncated.
const goalClassProviderExhausted provider.RetryableClass = "provider_exhausted"

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
// PursueGoal classifies this error through Unwrap and parks the active goal.
// The type identifies retryable-budget exhaustion for internal callers and tests.
type goalRetryableExhaustedError struct {
	err   error
	class provider.RetryableClass
}

func (e *goalRetryableExhaustedError) Error() string { return e.err.Error() }
func (e *goalRetryableExhaustedError) Unwrap() error { return e.err }

// goalEvaluatorExhaustedError is returned by PursueGoal when the evaluator has
// failed at goalEvalFailureLimit consecutive turn boundaries — a durable evaluator outage,
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

// goalWorkerParkedError is returned by PursueGoal when a worker turn
// exhausts any exhaustion tier — deterministic (goalWorkerRetries),
// retryable-class (goalRetryableMaxAttempts), stream-truncated
// (goalStreamTruncatedMaxAttempts), or provider-exhausted
// (goalProviderExhaustedMaxAttempts) — and the loop exit-parks
// instead of clearing the goal. See PursueGoal's doc comment and the
// worker failure handling: unlike goalEvaluatorExhaustedError
// above, reaching this sentinel is NOT a durable "give up" terminal — the
// goal stays fully active, ready to resume the instant a caller starts a
// new PursueGoal call for it (the server's activity-driven auto-arm,
// Task 2 upstream of this package).
//
// It wraps the underlying worker-turn error (via Unwrap, so errors.Is/
// errors.As see through it) for an operator-facing caller that needs the
// real detail — a server log line, last_turn.error. The durable
// goal.parked record this sentinel is always journaled alongside (see
// recordGoalParked, called by every PursueGoal branch that constructs one
// of these) deliberately carries a different, CLASSIFIED level of detail
// instead: this sentinel and that record serve different audiences and are
// not required to match text.
type goalWorkerParkedError struct {
	err       error
	attempts  int
	retryable bool
	// permanent is true when err was classified provider.AsPermanent (NEP-
	// 5272 defect 1) — a fail-fast, single-attempt park, distinct from an
	// ordinary deterministic exhaustion (goalWorkerRetries+1 attempts). Only
	// ever true when retryable is false (the two classifications are
	// mutually exclusive — see provider.AsPermanent's doc comment); named
	// separately from retryable/class rather than folded into a fourth
	// RetryableClass value because a permanent error is never provider
	// weather and must never be confused with one.
	permanent bool
	class     provider.RetryableClass
}

func (e *goalWorkerParkedError) Error() string {
	tier := "deterministic"
	switch {
	case e.class == goalClassProviderExhausted:
		tier = "provider-exhausted"
	case e.permanent:
		tier = "permanent"
	case e.retryable:
		tier = "retryable"
	}
	return fmt.Sprintf("engine: goal worker turn parked after %d %s-tier attempt(s): %v", e.attempts, tier, e.err)
}
func (e *goalWorkerParkedError) Unwrap() error { return e.err }

// IsGoalWorkerParked reports whether err is (or wraps, via errors.As) the
// sentinel PursueGoal returns when a worker turn exit-parks the goal — see
// goalWorkerParkedError above. Mirrors IsGoalEvaluatorExhausted's shape
// exactly (the #81 wiring pattern this package established): a caller (the
// server, in particular) tells this terminal apart from an ordinary error,
// or from IsGoalEvaluatorExhausted's terminal, via errors.As — never by
// string-matching GoalReason.
func IsGoalWorkerParked(err error) bool {
	var pe *goalWorkerParkedError
	return errors.As(err, &pe)
}

// classifyGoalWorkerError renders a short, provider-detail-free reason for
// goal.parked's Reason field (see recordGoalParked) — extending the
// classifyMCPConnectError family (engine/mcp.go) to this package's own
// durable, potentially long-lived terminal. Unlike goal.stalled/
// goal.eval_failed's Reason, which carry the raw err.Error() verbatim (see
// recordGoalStalled/recordGoalEvalFailed), a goal.parked record can be read
// by an operator-facing surface well after the triggering request is gone
// (GET /session's pause presentation, a dashboard — Task 2 upstream), so it
// must never echo a provider's raw error text: request IDs, endpoint URLs,
// or other vendor-specific detail with no fixed shape across providers.
// RetryableClass (recorded alongside this string on both the record and the
// event — see recordGoalParked) already names the provider's own
// classification precisely for a retryable-tier park (overloaded/
// rate_limited/server_error, see provider.RetryableClass), so this string
// only needs to say which TIER parked the turn, not repeat that detail.
//
// permanent (NEP-5272 defect 1) distinguishes a fail-fast, single-attempt
// park (a malformed-request-shape error provider.AsPermanent classified) —
// which never spent the deterministic budget at all — from an ordinary
// exhausted-retries park, so an operator reading this reason is never
// misled into thinking goalWorkerRetries+1 identical attempts happened when
// only one ever did. Only ever true when retryable is false.
func classifyGoalWorkerError(retryable, permanent bool, class provider.RetryableClass) string {
	switch {
	case class == goalClassProviderExhausted:
		// Named explicitly, ahead of the generic retryable case below, so
		// an operator reading goal.parked never confuses this with ordinary
		// overload/rate-limit weather: this is an account-level usage/quota
		// wall (see goalProviderExhaustedMaxAttempts' doc comment), still
		// resumable, just parked longer than this turn's budget could ride
		// out.
		return "provider account usage limit exhausted the retry budget"
	case retryable:
		return fmt.Sprintf("provider %s errors exhausted the retry budget", class)
	case permanent:
		return "worker turn failed with a permanent provider error and cannot succeed on retry"
	default:
		return "worker turn failed repeatedly and did not recover"
	}
}

// recordGoalParked records goal.parked: the terminal PursueGoal reaches
// when a worker turn exhausts any exhaustion tier without clearing the
// goal (see PursueGoal's exit-park branches and classifyGoalWorkerError for
// why this record's Reason is classified rather than the raw error text
// goal.stalled/goal.eval_failed carry). Deliberately does NOT touch
// s.goalActive/s.goalCondition — the goal stays armed; that is the entire
// point of a park versus a clear.
//
// Like every other per-turn goal record, it is a no-op — no journal write,
// no event, reports false — when the goal is no longer active (a
// concurrent ClearGoal) OR when it is active but at a different generation
// than gen (a concurrent UpdateGoal moved past this turn's snapshot — see
// goalSnapshot/goalStatus): a park is never attributed to a condition that
// is no longer current. PursueGoal's caller treats a false return exactly
// like recordGoalEvalFailed's — goalStatus decides stale-discard
// (`continue`, no error, the loop picks up the new condition) versus clean
// stop ("goal cleared" result, a concurrent DELETE won the race) — never
// journaling or returning the sentinel for a generation that is no longer
// current.
func (s *Session) recordGoalParked(turn, attempts int, retryable, permanent bool, class provider.RetryableClass, gen uint64) bool {
	s.mu.Lock()
	if !s.goalActive || s.goalGen != gen {
		s.mu.Unlock()
		return false
	}
	reason := classifyGoalWorkerError(retryable, permanent, class)
	s.persistGoalLocked(recGoalParked, goalRecord{
		Reason:         reason,
		Turn:           turn,
		Attempts:       attempts,
		Retryable:      retryable,
		RetryableClass: string(class),
	})
	// Emit while still holding s.mu (see ClearGoal): keeps event order
	// matching log order under a concurrent clear/update. OnEvent must not
	// call back into this Session — that would deadlock on s.mu, held here.
	s.emit(Event{
		Type: EventGoalParked, GoalReason: reason, GoalTurn: turn, GoalAttempts: attempts,
		GoalRetryable: retryable, GoalRetryableClass: string(class),
	})
	// Mirror the same classified reason + attempts into the runtime-only
	// ambient-segment fields (see the goalParked field's doc comment on
	// *Session) — still under the s.mu this function already holds, so a
	// concurrent goalParkedSegment read can never observe the journaled
	// record and this signal out of step with each other.
	s.goalParked = true
	s.goalParkedReason = reason
	s.goalParkedAttempts = attempts
	s.mu.Unlock()
	return true
}

// clearGoalParkedAtEntry resets the ambient parked-goal signal (see the
// goalParked field's doc comment on *Session) at the very top of PursueGoal,
// before that call touches anything else — including its own first worker
// turn. This is what makes "gone after resume" and "never visible during
// the goal loop's own turns" structural rather than incidental: whether
// this PursueGoal call is the server's activity-driven resume of a goal
// this exact session parked, or an entirely fresh RegisterGoal, any signal
// left over from a PRIOR exit-park episode describes a park this call is
// about to supersede either way, so it is cleared unconditionally on every
// entry — never conditioned on whether goalParked happens to be set, since
// the overwhelmingly common case (no prior park) makes that check pure
// overhead for no behavioral difference.
func (s *Session) clearGoalParkedAtEntry() {
	s.mu.Lock()
	s.goalParked = false
	s.goalParkedReason = ""
	s.goalParkedAttempts = 0
	s.mu.Unlock()
}

// PursueGoal prompts a worker and evaluates each completed turn.
//
// It records worker failures and parks the goal when retries end. Failure handling clears the goal only for context overflow or repeated evaluator failures. A canceled context leaves the goal active.
//
// The loop reads the current goal at each turn boundary. It discards results from an earlier goal generation.
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

	// Clear any parked signal left over from a prior exit-park episode
	// before anything else — see clearGoalParkedAtEntry's doc comment. This
	// runs unconditionally, even on the early "goal cleared" returns just
	// below: either way, this call supersedes whatever park the leftover
	// signal was describing.
	s.clearGoalParkedAtEntry()

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
		// evaluator failure handling and recordGoalEvalFailed):
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
		// The drained batch's attachments ride to the worker turn beside
		// that block: operatorMessagesBlock renders text only and announces
		// each prompt's attachment count, so these bytes are what the count
		// refers to. An image an operator sent mid-goal would otherwise be
		// dropped at exactly this boundary — see queuedBlobs (queue.go).
		if attempts, err := s.promptTurnWithRetry(ctx, directive, turn, snap.gen, queuedBlobs(queued)...); err != nil {
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
			// Worker failures: every remaining shape of worker-turn
			// exhaustion — the deterministic budget (goalWorkerRetries)
			// running out, the retryable-class budget
			// (goalRetryableMaxAttempts) running out, or the non-idempotency
			// gate stopping retries early after a tool already executed —
			// now EXIT-PARKS instead of clearing the goal, superseding both
			// the clear this package used before this commit AND GitHub
			// issue #61's in-loop `continue` self-re-arm (see the removed
			// comment this replaces, and the worker failure handling
			// section for the full incident and rationale). The only
			// worker-turn failure that still clears is context overflow,
			// immediately below — a deterministic failure no amount of
			// waiting can fix, unlike every case reaching this point.
			//
			// class/retryable are derived directly from the returned err via
			// provider.AsRetryable, not from checking whether
			// promptTurnWithRetry happened to wrap it in
			// *goalRetryableExhaustedError: errors.As traverses that
			// sentinel's Unwrap chain down to the same *provider.RetryableError
			// an ordinary (non-exhausted) retryable failure carries, so this
			// reads the true classification uniformly whether the retryable
			// budget was genuinely exhausted (attempts == goalRetryableMaxAttempts,
			// wrapped) or retrying merely stopped early after a tool call
			// (attempts far fewer, raw err) — both are still worth recording
			// accurately, exactly as goal.stalled already does for the same
			// failing attempt (see promptTurnWithRetry).
			class, retryable := provider.AsRetryable(err)
			// classifyProviderExhausted (shared with promptTurnWithRetry's
			// own call below, so the two sites can never drift): by the time
			// promptTurnWithRetry returns an error here for a
			// provider-exhausted failure, its own tier budget
			// (goalProviderExhaustedMaxAttempts) has already been spent
			// retrying it, so this IS a genuine exhaustion, not the
			// fail-fast-on-attempt-one shape the pre-fix permanent branch
			// produced. Reclassifying it as retryable/goalClassProviderExhausted
			// here — rather than leaving it to fall into the permanent branch
			// below — keeps the resulting goal.parked record and
			// classifyGoalWorkerError reason honest: "provider capacity
			// exhausted", never "permanent provider error".
			retryable, class, _ = classifyProviderExhausted(err, retryable, class)
			if !retryable && provider.IsContextOverflow(err) {
				// Issue #62, layer 1: a deterministic context/prompt
				// overflow gets its own distinct clear reason instead of a
				// park — waiting cannot fix it, unlike every case above (see
				// worker failure handling on this deliberate,
				// documented asymmetry) — and the error is returned AS-IS
				// (not wrapped) so last_turn.error (server/journal.go's
				// recordTurnEnd) surfaces exactly err.Error()'s clear,
				// deterministic message — see provider.Error.Error().
				// Checked only once retryable is ruled out: overflow is
				// never classified retryable, so the two are disjoint.
				s.clearGoal(err.Error())
				return nil, err
			}
			// NEP-5272 defect 1: a permanent-classified error (see
			// promptTurnWithRetry's fail-fast branch above) is, like context
			// overflow, never classified retryable — but unlike context
			// overflow it does NOT clear: the malformed request shape that
			// produced it might be fixed by something else entirely before a
			// later resume, so it falls through to the same park path every
			// other worker-turn exhaustion uses. permanent is threaded
			// through only to select a more accurate classified reason (see
			// classifyGoalWorkerError) and a distinct tier name on the
			// returned sentinel (see goalWorkerParkedError) — it changes no
			// other behavior on this path.
			permanent := !retryable && provider.AsPermanent(err)
			// Every remaining case parks: journal a durable, classified
			// goal.parked record (see recordGoalParked/
			// classifyGoalWorkerError) and return the sentinel WITHOUT
			// clearing — the goal stays active for an external caller (the
			// server's activity-driven auto-arm, upstream of this package)
			// to resume with a fresh PursueGoal call. Like every other
			// per-turn goal record, a false return here means a concurrent
			// ClearGoal or UpdateGoal raced this turn to completion: fall
			// back to the same goalStatus-driven stale-discard-vs-clean-stop
			// split every other record in this loop already uses (see
			// recordGoalEvalFailed's caller for the identical shape) rather
			// than ever parking a generation that is no longer current.
			if !s.recordGoalParked(turn, attempts, retryable, permanent, class, snap.gen) {
				if _, stale := s.goalStatus(snap.gen); stale {
					continue
				}
				return &GoalResult{Achieved: false, Turns: turn - 1, Reason: "goal cleared"}, nil
			}
			return nil, &goalWorkerParkedError{err: err, attempts: attempts, retryable: retryable, permanent: permanent, class: class}
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
			// Evaluator failures: a failing evaluator call is advisory, not
			// fatal —  evaluateGoal already spent its own
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
// Each retry re-issues the same directive. When the previous attempt left
// that directive sitting unanswered at the tail of history — the common
// case, see directiveReuseEligible below — the retry reuses that exact
// message (runAgenticLoop, engine.go) instead of appending a second copy;
// otherwise it falls back to calling Prompt again, which appends a fresh
// one. See docs/design/goal-retry-directive-reuse.md for the reuse design
// and its invariants. Either way, neither Prompt nor its internal loop body
// has a partial-turn resume point to retry from below itself, so this is
// the same "ask again" a human operator would do by hand, just automatic
// and bounded. That is harmless when the failed
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
// # Four independent budgets, chosen by error classification
//
// Every failed attempt is first classified via provider.AsRetryable (never
// by matching error text — see provider/retryable.go), then re-checked via
// provider.AsProviderExhausted (see goalClassProviderExhausted's doc
// comment) since an exhausted error is wrapped provider.MarkPermanent, not
// provider.MarkRetryable, and needs its own local override to be treated as
// weather rather than a doomed request. A DETERMINISTIC failure (not
// classified retryable, not provider-exhausted) runs the fast path exactly
// as described above: goalWorkerRetries additional attempts, goalRetryDelay's
// short backoff. A RETRYABLE (or provider-exhausted) failure runs one of
// three further loops, depending on its class, and none of them increments
// the deterministic counter, so surviving any of them costs a turn nothing
// against goalWorkerRetries:
//
//   - provider.RetryableStreamTruncated (a response stream that died before
//     its terminal event) runs its OWN, much smaller loop, up to
//     goalStreamTruncatedMaxAttempts attempts, spaced by goalRetryDelay's
//     same SHORT schedule the deterministic tier uses — see that constant's
//     doc comment for why: waiting longer can never raise a stream ceiling,
//     so this class must not be allowed to ride the long weather schedule
//     below.
//   - a provider-exhausted failure (an account-level usage/quota wall) runs
//     its own loop, up to goalProviderExhaustedMaxAttempts attempts, on the
//     SAME long jittered schedule (goalRetryableBackoff) ordinary weather
//     uses — see goalProviderExhaustedMaxAttempts' doc comment for why it
//     needs its own counter rather than sharing retryableAttempt below.
//   - every other retryable class runs its own loop, up to
//     goalRetryableMaxAttempts attempts, spaced by goalRetryableBackoff's
//     much longer jittered schedule (see the doc comment on that function).
//
// If any of the three budgets is exhausted, this function returns an error
// PursueGoal recognizes and parks the turn instead of clearing the goal (see
// PursueGoal's doc comment): the ordinary retryable and stream-truncated
// tiers wrap it in *goalRetryableExhaustedError first (see that type's doc
// comment); the provider-exhausted tier returns the bare err, since
// PursueGoal's caller reclassifies it directly via
// provider.AsProviderExhausted rather than needing a dedicated wrapper.
//
// The non-idempotency gate below (stop retrying once a tool has executed
// this attempt) applies identically to all three retry-shaped budgets:
// retrying after a tool call ran is unsafe regardless of why the subsequent
// call failed.
//
// gen is the calling turn's goalSnapshot generation, threaded straight
// through to recordGoalStalled so a stall record for an attempt is never
// journaled once an UpdateGoal has moved the goal past this turn's
// generation — see recordGoalStalled and PursueGoal's stale-discard handling.
// blobs are the attachments PursueGoal's turn-boundary drain collected for
// this turn (nil on every turn with no operator mail). They ride with the
// directive on the attempts that actually APPEND one — attempt 1 and the
// fallback branch below — and are deliberately absent from the
// directive-reuse branch, which appends nothing at all: that branch re-runs
// the previous attempt's still-unanswered message, which already carries
// these very blob parts.
func (s *Session) promptTurnWithRetry(ctx context.Context, directive string, turn int, gen uint64, blobs ...*message.Blob) (attempts int, err error) {
	var deterministicAttempt, retryableAttempt, truncatedAttempt, exhaustedAttempt int
	// anchorID identifies the message directiveReuseEligible and
	// dropUnansweredDirective both measure their tail from — the point
	// immediately before whichever directive is CURRENTLY this turn's live,
	// still-unanswered one. Captured once before attempt 1's own call
	// appends the turn's first directive, then advanced (see the fallback
	// branch below) exactly when a fresh directive replaces the one it
	// named.
	//
	// It must NOT stay pinned to the turn's start for the whole turn: a
	// fallback append (below) can leave undroppable residue behind an
	// earlier directive forever (a denied tool call's own ToolResult, say —
	// dropUnansweredDirective correctly refuses to touch it). A fixed
	// anchor's tail then never again shrinks to a droppable shape, so
	// EVERY later fallback re-appends yet another duplicate and drops none
	// — up to one per remaining attempt over a long outage
	// (goalRetryableMaxAttempts = 12), the exact NEP-5272 growth this
	// package exists to eliminate, reopened on this one path. Re-anchoring
	// to right before the fresh directive each fallback appends means the
	// NEXT attempt's tail is that directive alone, so directiveReuseEligible
	// picks it up and reuse resumes — bounding the damage to the one
	// fallback copy that was unavoidable, exactly like the per-attempt
	// anchor this package used before it could reuse a directive at all.
	anchorID := s.lastMessageID()
	for {
		attempts++
		toolsBefore := s.toolExecutions()
		var perr error
		switch {
		case attempts == 1:
			// Nothing to reuse yet: the ordinary path appends the directive
			// as history's first mention of it this turn.
			_, perr = s.PromptWithOrigin(ctx, directive, "", "", blobs...)
		case s.directiveReuseEligible(anchorID):
			// The tail after anchorID is exactly the previous attempt's own
			// unanswered directive (see directiveReuseEligible) — reuse it
			// instead of appending a second copy: run the turn loop against
			// history as it stands, answering that same message. See
			// docs/design/goal-retry-directive-reuse.md §3.
			_, perr = s.runAgenticLoop(ctx)
		default:
			// Not safe to reuse: either anchorID is no longer in history
			// (maybeAutoCompact folded it away since it was captured — see
			// the design's §6 risk) or the tail is some other shape this
			// package must never touch on its own (the three-message
			// interrupted-turn tail §5 deliberately keeps today's behavior
			// for; a denied tool's result; delivered operator mail — see
			// dropUnansweredDirective's doc comment). dropUnansweredDirective
			// drops the previous attempt's dangling directive from LIVE
			// history when it safely can (the interrupted-turn shape); it is
			// a harmless no-op otherwise. Either way, fall back to appending
			// a fresh directive exactly as every attempt did before this
			// package could reuse one.
			s.dropUnansweredDirective(anchorID)
			// Re-anchor to right before the fresh directive about to be
			// appended (see anchorID's own doc comment above for why this
			// must happen here): whatever dropUnansweredDirective did or did
			// not remove, THIS is the new boundary a later attempt's tail is
			// measured from, so a subsequent reuse check sees only what
			// happens from here on, never the residue this fallback is
			// leaving behind for good.
			anchorID = s.lastMessageID()
			_, perr = s.PromptWithOrigin(ctx, directive, "", "", blobs...)
		}
		if perr == nil {
			return attempts, nil
		}
		err = perr
		if errors.Is(err, context.Canceled) {
			return attempts, err
		}
		class, retryable := provider.AsRetryable(err)
		// A provider-exhausted error (an account-level usage/quota wall,
		// provider.ErrKindProviderExhausted) is wrapped provider.MarkPermanent
		// by the adapter, not provider.MarkRetryable — provider.AsRetryable
		// above already returned false for it — but for the GOAL LOOP it
		// behaves like weather, not a doomed request: the wall lifts on its
		// own (see goalProviderExhaustedMaxAttempts' doc comment for the
		// live incident this closes). classifyProviderExhausted (shared with
		// PursueGoal's own identical call above, so the two sites can never
		// drift) folds it into the local retryable/class variables here,
		// rather than adding a fourth classification threaded separately
		// through every branch below, reusing the exact same tier-dispatch,
		// stall-recording, and park-recording machinery the other three
		// tiers already exercise.
		retryable, class, providerExhausted := classifyProviderExhausted(err, retryable, class)
		// Stream truncation is retryable-CLASS (it parks on exhaustion,
		// carries its class on every stall record, and never spends the
		// deterministic budget) but runs its OWN, much smaller budget on
		// the SHORT schedule — see goalStreamTruncatedMaxAttempts for why
		// it must ride neither of the other two tiers. Provider-exhausted
		// gets its own budget for the same reason: it must never share a
		// counter with, and be silently starved or padded by, ordinary
		// overload/rate-limit weather in the same turn.
		truncated := class == provider.RetryableStreamTruncated
		// exhausted decides, for a retryable failure, whether THIS attempt
		// is the one that exhausts its tier's budget — computed before the
		// tier counter is incremented below, so the comparison reads as
		// "one more than the retries already spent, including this one,
		// would meet or exceed the ceiling".
		exhausted := retryable && ((truncated && truncatedAttempt+1 >= goalStreamTruncatedMaxAttempts) ||
			(providerExhausted && exhaustedAttempt+1 >= goalProviderExhaustedMaxAttempts) ||
			(!truncated && !providerExhausted && retryableAttempt+1 >= goalRetryableMaxAttempts))
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
		if !providerExhausted && provider.AsPermanent(err) {
			// NEP-5272 defect 1: a provider error classified permanent (an
			// HTTP 400 invalid_request_error naming a structurally
			// malformed request — e.g. an orphaned tool_use left over from
			// an earlier bug) is, like context overflow above, deterministic
			// and never worth retrying: the exact same request will fail
			// the exact same way on attempt 2 as it did on attempt 1, so
			// three identical guaranteed-to-fail model calls (the production
			// shape this closes: "goal worker turn parked after 3
			// deterministic-tier attempt(s)") buy nothing. Fail fast after
			// the single stall record above (permanent is never classified
			// retryable, so waiting is already false): no backoff wait, no
			// further attempt.
			//
			// Unlike context overflow, this does NOT return a bare
			// classification PursueGoal's caller clears on — a malformed
			// request shape may be fixed by something else entirely (an
			// orphan-tool-call repair, an operator edit further up the
			// history) between now and a later resume, so PursueGoal's
			// caller falls through to its ordinary park path instead (see
			// PursueGoal's error handling: retryable is false and
			// IsContextOverflow is false, so this reaches the "every
			// remaining case parks" branch unmodified).
			//
			// providerExhausted is excluded from this branch even though
			// provider.AsPermanent(err) is ALSO true for it (see
			// goalClassProviderExhausted's doc comment: the adapter wraps
			// it provider.MarkPermanent too) — an account wall is not a
			// malformed request, and must not fail fast on attempt one. It
			// falls through to the tier-dispatch section below instead,
			// where the providerExhausted branch handles it.
			return attempts, err
		}
		if toolGateStops {
			// This attempt executed a tool call before failing: see the
			// non-idempotency doc above. Retrying would reissue the
			// directive and risk re-running that tool, so stop now instead
			// of waiting and trying again — regardless of classification.
			return attempts, err
		}
		// NEP-5272 defect 2. Before docs/design/goal-retry-directive-reuse.md,
		// NOT every branch below this point was about to retry — the three
		// budget-exhaustion returns just below (deterministic, and the two
		// goalRetryableExhaustedError cases) PARK instead, and a parked
		// attempt's directive must stay in live history verbatim (see
		// dropUnansweredDirective's doc comment on the embedded-operator-
		// block case). That distinction no longer needs a call site of its
		// own here: nothing in this loop drops or re-appends anything on a
		// retry path anymore — the dispatch at the top of the loop (see
		// directiveReuseEligible) decides reuse-vs-append fresh, once, right
		// before the NEXT attempt's own call runs, using the exact same
		// anchorID a park below leaves untouched. A parking return below
		// simply never reaches that dispatch again, so the exhausting
		// attempt's directive — and any operator mail embedded in it — is
		// never touched, with no separate care required at each return.
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
		if truncated {
			truncatedAttempt++
			if exhausted {
				return attempts, &goalRetryableExhaustedError{err: err, class: class}
			}
			// Short schedule, no jitter: a stream ceiling is hit by
			// LONG turns, so attempts are naturally minutes apart already
			// — the wait only needs to clear a momentary network blip.
			if werr := waitGoalRetryBackoff(ctx, truncatedAttempt); werr != nil {
				return attempts, werr
			}
			continue
		}
		if providerExhausted {
			exhaustedAttempt++
			if exhausted {
				// Budget exhausted: return the bare underlying err (never
				// wrapped) so PursueGoal's caller — which classifies
				// directly via provider.AsProviderExhausted(err), the same
				// as it does for provider.AsRetryable(err) — sees straight
				// through to the real classification without needing a
				// dedicated sentinel type. goalRetryableExhaustedError exists
				// only so promptTurnWithRetry can signal its OWN budget gave
				// out to its own tests (see that type's doc comment);
				// provider-exhausted's own tests can assert directly on
				// exhaustedAttempt/attempts instead.
				return attempts, err
			}
			// Same long jittered schedule ordinary weather uses (see
			// goalProviderExhaustedMaxAttempts' doc comment for why: an
			// account wall is worth waiting out, exactly like overload/
			// rate-limit weather, and RecoverHint is deliberately never
			// parsed into an exact wake time).
			if werr := waitGoalRetryableBackoff(ctx, exhaustedAttempt); werr != nil {
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

// tailAfterAnchor returns the slice of s.history strictly after the message
// identified by anchorID, and whether the lookup succeeded. anchorID == ""
// names the very start of history (used for a turn whose directive was the
// session's first-ever message). A non-empty anchorID missing from CURRENT
// history — maybeAutoCompact folded away the message it names since it was
// captured (see lastMessageID's doc comment and the design's §6 risk) —
// reports ok=false: the caller must not guess a fallback position, since
// every other message in a folded history is unrelated to this turn.
//
// Callers must hold s.mu; both directiveReuseEligible and
// dropUnansweredDirective need the exact same anchor-to-tail lookup and
// previously duplicated it inline.
func (s *Session) tailAfterAnchor(anchorID string) ([]message.Message, bool) {
	idx := -1
	if anchorID != "" {
		for i, m := range s.history {
			if m.ID == anchorID {
				idx = i
				break
			}
		}
		if idx == -1 {
			return nil, false
		}
	}
	return s.history[idx+1:], true
}

// directiveReuseEligible reports whether the tail of history after anchorID
// is EXACTLY the single unanswered directive message a previous attempt
// appended — the only shape promptTurnWithRetry's retry dispatch reuses
// instead of re-appending (see docs/design/goal-retry-directive-reuse.md
// §3). It reuses isSafeToDropDirectiveTail's existing shape check but adds
// its own length==1 gate: isSafeToDropDirectiveTail's OTHER approved shape —
// the three-message interrupted-turn tail (directive, partial assistant
// reply, synthetic tool result; §5 of the design) — is deliberately
// EXCLUDED from reuse, because reusing it would hand the retried turn back
// the model's own partial output and a synthetic tool result: a
// model-visible behavior change this fix does not make. That shape, and any
// shape isSafeToDropDirectiveTail does not approve at all (a denied tool's
// result, delivered operator mail — see dropUnansweredDirective's doc
// comment), fall through to the caller's drop-and-reappend fallback instead.
//
// anchorID missing from CURRENT history is reported as NOT eligible (see
// tailAfterAnchor): the caller falls back to dropUnansweredDirective (a safe
// no-op in that case) plus an ordinary Prompt call, exactly the path every
// attempt took before this fix existed.
func (s *Session) directiveReuseEligible(anchorID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	tail, ok := s.tailAfterAnchor(anchorID)
	if !ok {
		return false
	}
	return len(tail) == 1 && isSafeToDropDirectiveTail(tail)
}

// lastMessageID returns the ID of the last message in history, or "" if
// history is empty — used only by the retry-dedup bookkeeping below, taken
// right before an attempt's own s.Prompt call appends anything.
//
// An identity, not a length: s.Prompt's FIRST action is maybeAutoCompact
// (engine.go), which can splice s.history to an entirely different length
// before the directive is even appended. A length snapshot taken here would
// go stale the instant that splice runs, then either silently miss a
// truncation it should have made (the snapshotted length no longer maps to
// the right position, or is now past the end of a shrunk history) or, on a
// history that grew back past the stale length by other means, cut an
// arbitrary interior point of this attempt's own work. A message ID has no
// such failure mode: dropUnansweredDirective below looks it up fresh every
// time, and safely does nothing if compaction folded it away entirely.
func (s *Session) lastMessageID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.history) == 0 {
		return ""
	}
	return s.history[len(s.history)-1].ID
}

// dropUnansweredDirective is promptTurnWithRetry's FALLBACK for the one
// retry shape directiveReuseEligible does not cover: the three-message
// interrupted-turn tail (directive, partial assistant reply, synthetic tool
// result — see interruptedTurnError in engine.go and §5 of
// docs/design/goal-retry-directive-reuse.md). For the common shape — the
// directive alone, unanswered — promptTurnWithRetry no longer calls this at
// all; it reuses that exact message instead (runAgenticLoop, engine.go), so
// no duplicate is ever appended and nothing here needs to run.
//
// This originated as NEP-5272 defect 2's mitigation (operator finding on
// box hyper-lemon): before the reuse fix, EVERY retry re-issued the
// directive through Prompt, which appends whatever text it is given as a
// brand-new user message with no notion of "this is a retry, don't
// duplicate it" — N failed attempts left N unanswered copies in history,
// each inflating every LATER request's input cost. The reuse fix closes
// that for the directive-alone shape at its root (no append, so nothing to
// drop); this method remains the mitigation for the one shape reuse
// deliberately excludes (see directiveReuseEligible's doc comment for why)
// and for the compaction-fold edge case below.
//
// anchorID is the ID lastMessageID captured before attempt 1's own append —
// see promptTurnWithRetry. dropUnansweredDirective looks up anchorID fresh
// in the CURRENT s.history: if it is no longer present (maybeAutoCompact
// folded it into a summary in the meantime), there is no safe way to
// identify this turn's own tail, so this is a no-op — leaving one duplicate
// directive in history is better than truncating an unrelated interior
// point (see lastMessageID's own doc comment).
//
// Called only from promptTurnWithRetry's top-of-loop dispatch fallback,
// which runs strictly BEFORE the next attempt's own call — never on a path
// that is about to park (a park returns without ever reaching that dispatch
// again). A parked attempt's tail therefore always survives verbatim,
// including any embedded operator mail (next paragraph) — nothing will ever
// re-append it.
//
// A failed, no-tool-executed attempt is not guaranteed to leave behind
// nothing but the directive or the interrupted-turn trio, either — a
// plugin's tool.execute.before hook can DENY a tool call, which still
// appends a ToolResult message without incrementing toolExecCount (see
// engine.go's runToolCall), and the model can then make a further request
// in the SAME Prompt call that fails; a queued prompt can also be delivered
// into that same window (Prompt's own tool-call-boundary drain). Either
// produces additional, already-DELIVERED messages in the tail — a denied
// tool's result, an "OPERATOR MESSAGES" block already journaled
// prompt.dequeued("injected") — that must never be discarded.
// isSafeToDropDirectiveTail below only approves the two shapes a failed
// attempt with NO tool execution and NO intervening delivery can produce:
// the directive alone, or the directive plus the interrupted-turn trio. Any
// other shape in the tail is left untouched — this method simply does not
// apply to that attempt, rather than risk dropping delivered mail.
//
// # Residual gap — deliberately NOT fixed here
//
// This mutates only the LIVE in-memory s.history; it does not and cannot
// touch the durable session log. Prompt's own append already persisted a
// recMessage record for the interrupted-turn trio via appendWithUsage
// before promptTurnWithRetry ever saw the failure — that record is on disk,
// and the append-only session log (see docs/session-storage-and-queue.md) has no
// sanctioned mechanism this package can reach for retracting or amending an
// already-journaled record without engine.go/store.go changes, which
// docs/design/goal-retry-directive-reuse.md §4 rejects outright (a new
// retract record risks wedging a session in a mixed-version fleet — see
// that section). So a resumed session (LoadSession, after a crash or
// restart mid-retry on THIS one shape) still replays the dropped trio from
// disk even though live history does not carry it — an accepted, narrower
// version of the gap this whole fix otherwise closes for the far more
// common directive-alone shape.
func (s *Session) dropUnansweredDirective(anchorID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tail, ok := s.tailAfterAnchor(anchorID)
	if !ok {
		return
	}
	if !isSafeToDropDirectiveTail(tail) {
		return
	}
	end := len(s.history) - len(tail)
	s.history = s.history[:end:end]
}

// isSafeToDropDirectiveTail reports whether tail is EXACTLY what a single
// failed, no-tool-executed attempt could have appended to history: the
// directive alone (RoleUser), or the directive plus an interrupted turn's
// partial assistant message and its synthetic tool-result message
// (RoleUser, RoleAssistant, RoleTool — see interruptedTurnError in
// engine.go). Any other shape means something besides this attempt's own
// unanswered directive landed in the gap — a denied tool call's result, an
// injected operator message, more than one round of either — and must
// never be dropped (see dropUnansweredDirective's doc comment).
//
// The role shape alone is NOT enough for the three-message case: a
// tool.execute.before hook DENY also produces [RoleUser, RoleAssistant,
// RoleTool] — a real assistant turn with a genuine ToolCall, denied rather
// than executed, so toolGateStops stays false exactly like an interrupted
// turn. isInterruptedToolResultMessage's content check is what tells the
// two apart.
func isSafeToDropDirectiveTail(tail []message.Message) bool {
	switch len(tail) {
	case 1:
		return tail[0].Role == message.RoleUser
	case 3:
		return tail[0].Role == message.RoleUser &&
			tail[1].Role == message.RoleAssistant &&
			tail[2].Role == message.RoleTool &&
			isInterruptedToolResultMessage(tail[2])
	default:
		return false
	}
}

// isInterruptedToolResultMessage reports whether m is EXACTLY the synthetic
// tool-result message interruptedToolResults (engine.go) builds for a
// stream that died before its terminal event: every ToolResult part's
// Content must render interruptedTurnErrorText verbatim, and there must be
// at least one. A denied tool call's own ToolResult carries the hook's own
// deny message instead, and a real executed tool's error result carries
// the tool's own output, so neither is ever mistaken for this one — even
// though both share the same three-message role shape.
func isInterruptedToolResultMessage(m message.Message) bool {
	if len(m.Parts) == 0 {
		return false
	}
	for _, p := range m.Parts {
		tr, ok := p.(*message.ToolResult)
		if !ok || tr.Content.Text() != interruptedTurnErrorText {
			return false
		}
	}
	return true
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
//
// Reason is err.Error() verbatim for every class except
// goalClassProviderExhausted — a review finding on the fix that introduced
// that class: err.Error() for a provider-exhausted error starts with
// "[permanent] ..." (the adapter wraps it provider.MarkPermanent — see that
// constant's doc comment), which reads as self-contradicting next to this
// SAME record's own Retryable:true/RetryableClass:"provider_exhausted"
// fields. That one class instead renders through classifyGoalWorkerError,
// the same classified rendering recordGoalParked already uses, so a
// goal.stalled record for this class reads consistently with its own
// classification fields instead of echoing raw permanent-branch text.
func (s *Session) recordGoalStalled(err error, turn, attempt int, retryable bool, class provider.RetryableClass, waiting bool, gen uint64) bool {
	s.mu.Lock()
	if !s.goalActive || s.goalGen != gen {
		s.mu.Unlock()
		return false
	}
	reason := err.Error()
	if class == goalClassProviderExhausted {
		reason = classifyGoalWorkerError(retryable, false, class)
	}
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
	// A clear (operator DELETE, or PursueGoal's context-overflow branch)
	// always supersedes any parked signal still standing from an earlier
	// exit-park episode — see the goalParked field's doc comment: there is
	// no longer an active goal for the ambient segment to describe.
	s.goalParked = false
	s.goalParkedReason = ""
	s.goalParkedAttempts = 0
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
//
// Also clears the runtime goalParked/goalParkedReason/goalParkedAttempts
// presentation fields (same condition-changed gating as goalGen) so a plain
// Prompt landing between this call and the next PursueGoal entry never
// renders goalParkedSegment's ambient block quoting a stale park episode
// against the new condition text — see the inline comment at the clear site.
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
	// Clear the runtime parked-presentation fields (see the goalParked field's
	// doc comment on *Session) in the same critical section as the condition
	// change, not just on the next PursueGoal entry. Without this, a plain
	// Prompt landing in the window between this UpdateGoal call and the next
	// PursueGoal turn would render goalParkedSegment's ambient block quoting
	// the OLD park episode's reason/attempts against the NEW condition text —
	// a stale, confusing pairing. Gated on the condition-changed branch only
	// (mirrors the goalGen bump above, which is also skipped on a same-
	// condition no-op): a no-op UpdateGoal changes nothing about the goal's
	// state, so there is nothing stale to invalidate — the parked signal
	// still accurately describes the one goal, under its one unchanged
	// condition, that is still waiting to resume.
	s.goalParked = false
	s.goalParkedReason = ""
	s.goalParkedAttempts = 0
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
// evaluator failure handling and evaluateGoal/runEvaluatorWithRetry): a provider
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
// (see evaluator failure handling: a model that already failed to
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
// schedule promptTurnWithRetry uses for the worker turn's WEATHER tier
// (goalRetryableMaxAttempts, goalRetryableBackoff/waitGoalRetryableBackoff —
// see GitHub issue #61 and retry handling) — the two
// paths ride out the same shared provider weather, so they share a budget's
// shape, each keeping its own counter.
//
// A response stream classified provider.RetryableStreamTruncated is
// retryable-CLASS but must never ride that weather budget, for exactly the
// reason documented on goalStreamTruncatedMaxAttempts: waiting longer cannot
// raise a stream ceiling, so burning up to ~30 minutes per evaluator call
// against a truncating stream buys nothing a much shorter wait wouldn't —
// it instead runs its OWN, much smaller budget (goalStreamTruncatedMaxAttempts)
// on the SHORT schedule (waitGoalRetryBackoff), mirroring
// promptTurnWithRetry's own truncated tier exactly, including its own
// counter kept separate from retryableAttempt below.
//
// A non-retryable provider error is returned immediately, unretried: unlike
// a worker turn, the evaluator call is cheap and tool-less, so this budget
// exists purely to survive transient weather, not to paper over a provider
// that is permanently broken — see evaluateGoal's caller, PursueGoal, for
// what happens when this ultimately returns an error (the boundary counts
// as failed; it is never immediately fatal on its own). context.Canceled is
// never retried, matching every other backoff wait in this package.
func (s *Session) runEvaluatorWithRetry(ctx context.Context, condition string, evaluator message.ModelRef, systemPrompt string) (string, error) {
	var retryableAttempt, truncatedAttempt int
	for {
		out, err := s.runEvaluator(ctx, condition, evaluator, systemPrompt)
		if err == nil {
			return out, nil
		}
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		class, retryable := provider.AsRetryable(err)
		if !retryable {
			return "", err
		}
		if class == provider.RetryableStreamTruncated {
			truncatedAttempt++
			if truncatedAttempt >= goalStreamTruncatedMaxAttempts {
				return "", err
			}
			if werr := waitGoalRetryBackoff(ctx, truncatedAttempt); werr != nil {
				return "", werr
			}
			continue
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
	transcript, truncated := renderConversationBounded(s.History(), goalEvaluatorTranscriptBudgetBytes(evaluator))
	if truncated {
		transcript = goalEvaluatorTruncationNotice + transcript
	}
	content := "GOAL CONDITION:\n" + condition + "\n\nCONVERSATION TRANSCRIPT:\n" + transcript
	req := &provider.Request{
		Model:  evaluator,
		System: []string{systemPrompt},
		Messages: []message.Message{{
			ID:    newID("msg"),
			Role:  message.RoleUser,
			Parts: message.Parts{&message.Text{Text: content}},
		}},
		MaxTokens: 256,
		// SessionKey names this session for an adapter that forwards it as
		// a routing/cache-affinity hint (see provider.Request.SessionKey).
		SessionKey: s.ID,
		// The evaluator is a classifier, not a reasoning task: it always
		// pins EffortOff, never the session's own level (see docs/goal-loop.md).
		// Since a7c5cce, EffortOff sends the literal
		// "off" on openaicompat and no thinking block on anthropic — both
		// routes now spend none of the evaluator's MaxTokens 256 budget on
		// reasoning. openai Responses is a known residual: reasoningEffort
		// omits the reasoning object for EffortOff exactly as it does for
		// EffortUnset (provider/openai/transcode.go), and a gpt-5-class
		// model reasons by default with no adapter-level way to disable it
		// — so an evaluator on that route can still spend its budget on
		// reasoning. (Issue #124.)
		Effort: message.EffortOff,
	}
	// The evaluator's stream gets the same idle watchdog worker turns get
	// (see armIdleWatchdog): it runs at EVERY goal turn boundary, so a
	// permanently silent evaluator stream would wedge PursueGoal forever
	// while holding the run slot — no goal.eval_failed, no turn.end, and
	// the prompt queue never drains.
	ctx, watch, release := s.armIdleWatchdog(ctx)
	defer release()
	stream, err := prov.Stream(ctx, req)
	if err != nil {
		return "", watch.explain(err)
	}
	defer stream.Close()

	var deltas strings.Builder
	var doneText string
	for {
		ev, err := stream.Next()
		watch.kick()
		// Identity comparison, deliberately not errors.Is: adapters return
		// the LITERAL io.EOF only as the normal post-terminal
		// end-of-iteration signal. A TRUNCATED stream's error (see
		// provider.MarkStreamTruncated) wraps an underlying io.EOF, so
		// errors.Is would match it too — and treat text cut off
		// mid-verdict as a complete verdict, achieving (or re-prompting)
		// a goal on words the evaluator never finished.
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", watch.explain(err)
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
// role-labeled, each part rendered as text and capped at goalPartCap. This
// has no length bound of its own — see renderConversationBounded, which
// runEvaluator actually calls, for the evaluator-model-sized budget.
func renderConversation(history []message.Message) string {
	var b strings.Builder
	for _, m := range history {
		b.WriteString(renderMessageBlock(m))
	}
	return strings.TrimSpace(b.String())
}

// renderMessageBlock renders one message exactly as renderConversation's
// loop body did before this function was split out — role-labeled, each
// part capped at goalPartCap — so both renderConversation and
// renderConversationBounded share one rendering rule instead of drifting.
func renderMessageBlock(m message.Message) string {
	var b strings.Builder
	b.WriteString(strings.ToUpper(string(m.Role)))
	b.WriteString(":\n")
	for _, p := range m.Parts {
		b.WriteString(truncateForGoal(renderPart(p)))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}

// goalEvaluatorTruncationNotice prefixes a bounded transcript
// (renderConversationBounded) whenever it actually dropped earlier
// messages, so the evaluator — and an operator reading a goal.eval record
// later — never mistakes a truncated transcript for the whole session.
const goalEvaluatorTruncationNotice = "[earlier conversation omitted to fit the evaluator's context budget]\n\n"

// renderConversationBounded is renderConversation's budget-aware sibling:
// the fix for the live incident on box bx-01m0x8996, whose evaluator died
// with "context exhausted: prompt 245332 tokens > limit ..." because
// renderConversation(s.History()) has no bound at all — it grows with the
// WHOLE session transcript forever, while the main session is protected by
// automatic compaction (engine/compact.go) and the evaluator never was.
//
// It walks history from the NEWEST message backward, accumulating rendered
// blocks (renderMessageBlock — the exact same per-part goalPartCap rendering
// renderConversation uses, so nothing here changes how one message renders,
// only how many are kept) until the next block would push the running total
// past budgetBytes, then stops and reverses the kept slice back into
// chronological order.
//
// "Prefer summary + tail" falls out of this walk for free rather than
// needing a second summarization path of its own: Compact (engine/
// compact.go) splices its summary message directly into s.history in place
// of the range it folded, tagged with the compactionSummaryIDTag prefix
// (isCompactionSummaryID). The backward walk here stops the INSTANT it
// includes such a message — even with budget still unspent — because that
// message already IS the compacted record of everything before it walking
// further back would just render already-summarized content a second time.
// So whenever automatic compaction has run at all, the evaluator naturally
// gets exactly "the latest compaction summary plus every raw message after
// it, bounded to what fits" with no new summarization call, no new stored
// field, and no coupling to compaction's internals beyond the one ID-prefix
// helper it already exports to this package.
//
// The newest message is always kept, however large, rather than dropped
// outright: an evaluator call with an empty transcript could never assess
// anything. A single oversized message still gets its own per-part cap from
// renderMessageBlock/truncateForGoal, so this is bounded too, just not by
// budgetBytes.
func renderConversationBounded(history []message.Message, budgetBytes int) (transcript string, truncated bool) {
	if len(history) == 0 {
		return "", false
	}
	kept := make([]message.Message, 0, len(history))
	used := 0
	for i := len(history) - 1; i >= 0; i-- {
		block := renderMessageBlock(history[i])
		if len(kept) > 0 && budgetBytes > 0 && used+len(block) > budgetBytes {
			break
		}
		kept = append(kept, history[i])
		used += len(block)
		if isCompactionSummaryID(history[i].ID) {
			break
		}
	}
	for l, r := 0, len(kept)-1; l < r; l, r = l+1, r-1 {
		kept[l], kept[r] = kept[r], kept[l]
	}
	return renderConversation(kept), len(kept) < len(history)
}

// goalEvaluatorReserveTokens sets aside room, in the evaluator model's OWN
// token budget, for everything in the request besides the transcript: the
// system prompt (goalEvaluatorSystem/goalEvaluatorStrictSystem, both well
// under 700 bytes), the "GOAL CONDITION" preamble and condition text, and
// the 256-token MaxTokens output reply. Deliberately generous relative to
// those actual sizes — goalEvaluatorContextBudgetFraction below is what
// does the real safety work; this constant only keeps a degenerate tiny
// window (goalEvaluatorFallbackContextWindowTokens) from computing a
// negative or implausibly small transcript budget.
const goalEvaluatorReserveTokens = 2048

// goalEvaluatorContextBudgetFraction bounds the evaluator transcript to this
// fraction of the evaluator model's context window, after
// goalEvaluatorReserveTokens is set aside — the same "stay well under the
// hard limit, don't cut it exactly at the edge" shape
// defaultCompactionThreshold (engine/compact.go) uses for the main session.
// A fraction well under 1.0 matters more here than it does there:
// bytesPerTokenEstimate's 4-bytes/token conversion (reused from compact.go,
// not reinvented — see goalEvaluatorTranscriptBudgetBytes) is a crude
// heuristic, not the provider's real tokenizer, and this budget has no
// second line of defense the way compaction's threshold-then-hard-overflow
// does: an evaluator call that still overflows has nothing left to retry
// into.
const goalEvaluatorContextBudgetFraction = 0.5

// goalEvaluatorFallbackContextWindowTokens is the transcript budget's floor
// for an evaluator model modelmeta has NO ENTRY for at all (an unrecognized
// ref, a custom gateway alias) — mirrors minAutoContextWindowTokens
// (engine/context_window.go), the exact same floor automatic compaction
// refuses to ARM below. A genuinely unrecognized evaluator model still gets
// a real, bounded budget from this floor instead of falling back to the
// fully unbounded renderConversation(s.History()) that produced the "prompt
// 245332 tokens > limit" evaluator failure on bx-01m0x8996 in the first
// place.
//
// This floor must NOT be reused as a stand-in for "the model's real window
// is small" — see goalEvaluatorTranscriptBudgetBytes's doc comment for why
// resolveContextWindow (which folds that case into this same floor, for
// automatic-compaction-ARMING purposes) is deliberately NOT what this
// function calls.
const goalEvaluatorFallbackContextWindowTokens = minAutoContextWindowTokens

// goalEvaluatorTranscriptBudgetBytes returns the byte budget
// renderConversationBounded must fit the rendered CONVERSATION TRANSCRIPT
// field inside, derived from the EVALUATOR model's own context window —
// never the main session model's, which can be (and on bx-01m0x8996, was —
// a 1,000,000-token model against an evaluator whose own limit the incident
// error names) far larger than the evaluator's own.
//
// This calls modelContextWindowLookup (modelmeta.ContextWindow) DIRECTLY —
// deliberately NOT resolveContextWindow, despite that function existing
// for exactly this "look up a model's context window" job and this
// function's own earlier revision having called it. A review finding
// caught why that was wrong: resolveContextWindow's minAutoContextWindowTokens
// floor answers "should automatic compaction ARM for this window" — a
// window below the floor is treated as bogus/untrustworthy metadata and the
// function reports (0, disabled), identically to a model with NO metadata
// at all. Calling it here silently conflated two different evaluator
// models: a genuinely UNRECOGNIZED one (no table entry — this really
// should fall back to a floor) and a REAL, SMALL, KNOWN one (gpt-4's
// documented 8_192-token window is the table's own example of a legitimate
// entry under the 16k floor) — both funneled into the SAME
// goalEvaluatorFallbackContextWindowTokens (16k) fallback, so a real
// 8_192-token evaluator got a budget roughly TWICE its actual window: the
// exact overflow class this whole fix exists to close. Calling
// modelContextWindowLookup directly and trusting ANY positive, KNOWN
// window — however small — fixes that: the floor here applies only to a
// true "no entry at all" miss, never to "the real entry is small."
func goalEvaluatorTranscriptBudgetBytes(evaluator message.ModelRef) int {
	windowTokens, ok := modelContextWindowLookup(evaluator)
	if !ok || windowTokens <= 0 {
		windowTokens = goalEvaluatorFallbackContextWindowTokens
	}
	budgetTokens := int(float64(windowTokens)*goalEvaluatorContextBudgetFraction) - goalEvaluatorReserveTokens
	if budgetTokens < goalEvaluatorReserveTokens {
		budgetTokens = goalEvaluatorReserveTokens
	}
	return budgetTokens * bytesPerTokenEstimate
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
