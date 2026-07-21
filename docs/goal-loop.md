# Goal-loop resilience: forensic root cause and the state-machine fix

## Incident

Two production sessions were found with an active goal that had stopped
making progress: `ses_41813d5a411c2ba5.jsonl` and, earlier,
`ses_55e4ae35d8344540.jsonl`. Both show the same shape. Immediately after a
`goal.eval` "NOT MET" verdict, the fixed-template guidance message is
appended to the log as the next directive — and then nothing. No assistant
turn, no error record, no further `goal.eval`. In `ses_41813d5a411c2ba5.jsonl`
the guidance message is timestamped `2026-07-09T05:20:12Z`; the very next
record in the file is a bare `goal.cleared`, followed by a message
timestamped `2026-07-09T12:12:52Z` — nearly seven hours later — that reads:

> "Your goal loop was interrupted. Do exactly this and nothing else: cd
> /work/tssdk && git add -A && git commit ... && git push ..."

That `goal.cleared` and the recovery message are not something the engine
produced. They are a human, seven hours later, noticing the session had gone
silent with an active goal, clearing it by hand, and manually steering the
agent to at least commit and push whatever it had. `ses_55e4ae35d8344540.jsonl`
shows the identical pattern twice in a row: a `goal.set` → guidance message →
silence → (manually cleared) → `goal.set` (a manual resume) → guidance
message → silence → (manually cleared) again.

The goal's own directive in both sessions instructs the worker to `apt-get
install -y nodejs npm` and install a TypeScript SDK toolchain — commands that
can emit megabytes of installer/build output. The engine's `bash` tool had no
enforced output cap wired through `Config` (a 100KB post-hoc truncation
existed as a hardcoded constant, tail-only, not configurable), so a large
enough burst of tool output from exactly the kind of command these goals ran
is a plausible trigger for whatever made the next worker turn fail. But the
log is silent about *what* failed — because that is exactly the bug: nothing
recorded the failure at all.

## Root cause

`engine/goal.go`'s `PursueGoal` loop called the worker turn like this:

```go
if _, err := s.Prompt(ctx, directive); err != nil {
    return nil, err
}
```

Any error from `s.Prompt` — a provider timeout, a rate limit, a stream error
triggered while handling an oversized tool result, anything — propagated
straight out of `PursueGoal` with **no goal.eval, no goal.stalled, no
goal.cleared**. `goalActive` stayed `true` in memory and in the session log.
The server's `runGoal` (`server/handlers.go`) does journal a `session.error`
for such an error, but never clears the goal, so the session parks forever
with an active goal that no automated process will ever revisit — a zombie.
Only a human reading the log tail could tell the loop had died and clear it
by hand, which is exactly what happened, hours later, in both incidents.

## The fix

Two independent layers, both TDD red-first (see `engine/goal_test.go` and
`engine/bash_test.go`):

1. **Goal-loop resilience** (`engine/goal.go`): a worker-turn error now goes
   through `promptTurnWithRetry`, which retries the same directive up to
   `goalWorkerRetries` (2) additional times, recording a durable
   `goal.stalled` record/event (carrying the error and the attempt number)
   for every failed attempt — the loop can never go silent again. If every
   attempt fails, the goal is **cleared** (`goal.cleared`, carrying the error
   as `GoalReason`) before the error is returned, so `goalActive` can never be
   left `true` with nothing left to explain it. A `context.Canceled` error
   (DELETE /goal, shutdown drain) is never retried or treated as a failure —
   it is a deliberate, resumable stop, and the goal is left untouched. See the
   state-machine diagram in the `goal.go` package doc. (**Superseded by
   NEP-4849**: exhausting this budget no longer clears — it PARKS, exactly
   like the retryable-class budget below — see "Worker-turn exit-park and
   activity-driven resume" further down.)

2. **Bash output cap** (`engine/bash.go`): tool output is now bounded by a new
   `Config.BashOutputCap` knob (default 96KB) enforced by a `cappedWriter`
   during capture — not by buffering the full output and truncating
   afterward — so a runaway command (an `apt-get`/`npm install` storm is the
   real-world trigger) can allocate only `O(cap)` memory and can never dump
   megabytes into a single message. The cap keeps both the head (so the
   command and its early output stay visible) and the tail (so a trailing
   error banner stays visible), joined by a `"N bytes truncated"` marker,
   before the output ever reaches the message log or the next provider
   request built from it.

## Non-goals / things this does not change

- `goal.stalled` is a pure trace record: it never flips `goalActive` by
  itself (see `LoadSession`'s `scanLog` switch in `store.go`).
- `evaluateGoal`'s own error path — a provider error while asking the
  evaluator, or two unparseable replies in a row — is **no longer**
  unchanged from the worker-turn path. It used to clear the goal outright
  (see "Round 3" below); as of NEP-4792 it is advisory, mirroring the
  worker-turn retry machinery instead of bypassing it. See "Evaluator
  resilience" below for the current behavior — this bullet exists only so a
  reader following an old link lands somewhere accurate.

## Round 3: closing the evaluator's own zombie path (superseded by NEP-4792)

The paragraph below describes the fix as it shipped originally: any
`evaluateGoal` error — a provider error, or two unparseable replies in a
row — cleared the goal outright, on the theory that the goal's own state
was "still accurate and worth preserving for a human-triggered resume."
Production experience proved that theory wrong (see "Evaluator resilience"
below): a transient evaluator hiccup killed sessions where the worker model
was making fine progress. The clear-on-any-evaluator-error behavior is no
longer current; it is kept here as a record of what changed and why.

Originally: an evaluator call that failed outright (a provider error, or two
unparseable replies in a row, see `errEvaluatorUnparseable`) was the one
edge out of ACTIVE that had no clear-and-explain treatment. One production
session (`ses_01kx3ts0pjfap950bmr9b2js0b.jsonl`) hit exactly this: the
worker turn succeeded, the evaluator returned unparseable output twice in a
row, `session.error` was emitted, and the goal stayed active in the log
forever — turns=0, no `goal.eval` ever, nothing to explain the silence
beyond that one error record. The original fix made a failing evaluator
call clear the goal (unless the error was a cancelled context) before
returning, the same "no third way out of ACTIVE" principle as the
worker-turn fix above. See `TestPursueGoalUnparseableTwiceClearsGoal`
(rewritten under NEP-4792 — see below).

## Review follow-up: two findings on the initial fix

The initial fix above shipped retries with no delay between attempts and no
discussion of what a retry actually re-runs. Both were flagged in review and
are fixed here, in `engine/goal.go`:

### 1. Retries now wait — capped exponential backoff

Back-to-back retries with zero delay do essentially nothing against the two
transient causes the doc above and the code comments name: a rate limit and a
momentary 5xx. Both usually need at least a little wall-clock time to clear.
`promptTurnWithRetry` now waits between attempts via `waitGoalRetryBackoff`,
on the schedule computed by `goalRetryDelay`:

| after attempt | wait before next attempt |
|---|---|
| 1 | 1s |
| 2 | 4s |
| 3+ (hypothetical, `goalWorkerRetries` is 2 today) | ×4 each time, capped at 30s |

The wait is context-cancellable (`select` on the timer and `ctx.Done()`), so
a deliberate abort (DELETE /goal, shutdown drain) ends it immediately instead
of sleeping out the rest of the schedule — same "leave the goal exactly as it
is" semantics as a `context.Canceled` from `s.Prompt` itself.

Tested in `engine/goal_test.go` inside `testing/synctest` bubbles
(`TestPursueGoalRetriesTransientWorkerError` asserts the exact 1s+4s elapsed
schedule; `TestPursueGoalRetryBackoffCancellable` asserts a cancellation
arriving mid-wait cuts the schedule short) — per AGENTS.md, timer-dependent
logic is bubble-tested, never a real wall-clock sleep in the test binary.
`TestGoalRetryDelaySchedule` pins the schedule as a pure function,
independent of the loop.

### 2. Retries are not idempotent, and that is now explicit and partially gated

A retry does not resume the failed turn. `s.Prompt(ctx, directive)` is called
again with the identical directive text, which `Prompt` treats as a brand new
user turn from scratch. That is harmless if nothing happened yet — but
`Prompt`'s own loop is `model call -> tool calls -> model call -> ...` until
end-of-turn, so a single attempt can execute one or more tool calls, append
their results, and only then hit a provider error on a *later* model call
within that same attempt (exactly "a provider error after tool calls
executed" — the case this review finding names). Retrying that attempt
re-prompts a model that still believes it needs to satisfy the original
directive, and nothing stops it from re-issuing the same tool call(s) a
second time: a shell command re-run, a file re-written. Whether that is
actually safe is entirely tool-specific and this package cannot know it in
general.

This is now stated prominently on `promptTurnWithRetry`'s doc comment (not
just here), and it is gated where it *is* detectable: `Session` tracks a
monotonic tool-execution counter (`toolExecCount`, incremented in
`runToolCall` in `engine.go`, once per tool call that actually executes).
`promptTurnWithRetry` snapshots it before each attempt and, if an attempt
fails after the count moved, treats the failure as non-retryable — it records
the `goal.stalled` for that attempt and returns immediately, without waiting
or trying again, rather than reissuing a directive that could re-run
whatever already ran. `TestPursueGoalNoRetryAfterToolExecution` is the
red-first test: a worker call executes a tool, the next worker call always
fails, and the test asserts the tool ran exactly once and no third provider
call — or fourth, fifth, ... — is ever made.

**This is not a general fix and is documented as such, not implied as
safety it doesn't have.** A failure before an attempt's first tool call is
still retried (correctly — nothing to redo), but if *that* retry attempt
later executes a tool and then fails again on a still-later call, the
identical risk resurfaces one attempt later. There is no bound on how many
times this can recur short of `Prompt` gaining a resumable, sub-turn
checkpoint, which it does not have today. Tools that are not idempotent
(anything that mutates external state — `bash`, `write_file`, `edit_file`)
remain at risk of double execution whenever a worker-turn retry happens to
follow a tool call within the same attempt; this document and the doc
comment on `promptTurnWithRetry` are the explicit acknowledgment the review
asked for, not a claim that the risk is eliminated.

## Retryable-class backoff and self-re-arm (GitHub issue #61)

### Incident

Two production days, one shared Anthropic overload wave: four separate goal
loops (`ses_01kx6423nef95t30vxgs36p80s`, `ses_01kx6423rne45s73xx1r816g1n`, and
two more on other sessions, all on volume `harness-dev-sessions-v2`) died
within minutes of each other with

```
engine: goal loop stalled: anthropic: Overloaded (overloaded_error)
```

The fix described above (`promptTurnWithRetry`) already treats every
worker-turn error identically: `goalWorkerRetries` (2) extra attempts,
~5 seconds of total backoff, then a permanent clear. That is exactly
backwards for `overloaded_error` — Anthropic-side capacity weather that
"routinely lasts several minutes," not the kind of failure five seconds of
patience was ever going to fix. Every one of the four incident goals resumed
cleanly the instant a human manually re-armed it once the wave passed — the
strongest possible evidence that these particular stalls were premature, not
genuine.

### The fix: classify, then split the budget

**1. Error classification lives at the provider layer, not the engine.**
`provider.RetryableError` (`provider/retryable.go`) is a typed wrapper an
adapter attaches to an error it recognizes as transient provider weather; the
engine recovers it with `errors.As` (`provider.AsRetryable`) — there is no
string-matching of error text anywhere in `engine/goal.go`. `RetryableError`
unwraps to the original error (so `errors.Is` and any existing `%w`-wrapping
still works, including through `engine`'s own `interruptedTurnError`) and its
`Error()` prefixes the message with the class (e.g. `[retryable:overloaded]
anthropic: Overloaded (...)`), so anything that only ever calls `.Error()` — a
journaled `goal.stalled` reason, a `turn.end` error — names the class for
free, with no extra plumbing.

- `provider/anthropic` classifies HTTP 529 (`RetryableOverloaded`), HTTP 429
  (`RetryableRateLimited`), and any other 5xx (`RetryableServerError`) — both
  from the ordinary HTTP-status error path and from the mid-stream `"error"`
  SSE event (keyed on the wire's own `type` field: `overloaded_error`,
  `rate_limit_error`, `api_error`), which is the exact shape the incident
  hit.
- `provider/openaicompat` classifies HTTP 429 and any 5xx the same way (no
  dedicated "overloaded" status on that generic wire).
- Everything else — 400s, authentication failures — is left unmarked and
  fails exactly as fast as before. A bad request will never succeed no
  matter how long the loop waits; only transient provider weather earns the
  long budget below.

**2. `promptTurnWithRetry` runs two independent budgets, chosen per
attempt.** A failure that is *not* classified retryable takes the original,
completely unchanged fast path described above. A failure that *is*
classified retryable instead runs its own loop:

- `goalRetryableMaxAttempts` (12) attempts, versus `goalWorkerRetries`'s 2.
- `goalRetryableBackoff`'s schedule: 5s, 10s, 20s, 40s, 80s, 160s, then
  capped at 5 minutes — roughly 30 minutes of total waiting in the worst
  case, versus ~5 seconds for the deterministic path. Each wait applies
  "equal jitter" (half the scheduled delay fixed, half randomized) via the
  `goalJitterFunc` seam, specifically so that many goal loops hitting the
  *same* shared overload wave (as all four incident sessions did) don't all
  retry in lockstep and re-hit the still-recovering provider at the same
  instant.
- These retryable-class attempts **never increment `goalWorkerRetries`'s
  counter.** A provider overload wave does not spend down the same
  fast-fail allowance a bad request would; a goal that survives a long
  outage via this path still has its full deterministic budget intact for
  whatever comes next.
- The existing non-idempotency gate (stop retrying the instant a tool has
  executed during the failing attempt — see the review-follow-up section
  above) applies identically to both budgets. Retrying after a tool call ran
  is unsafe regardless of why the next call failed.

Every failed attempt — deterministic or retryable — still gets exactly one
`goal.stalled` record, so the loop can never go silent (the original,
`goal.go`-level invariant this whole document exists to protect). A
retryable-class record additionally carries `retryable: true`,
`retryable_class` (`overloaded` / `rate_limited` / `server_error`), and
`waiting: true` — except the *final* one, if the retryable budget is
actually exhausted, which flips `waiting` to `false` to mark that the loop is
giving up on waiting and about to do something else (see below). This is
what lets a session log or a live SSE subscriber tell "waiting out provider
weather" apart from "genuinely stuck" without decoding `goal_reason` text —
`Session.goal` (`GET /session/{id}`, `GET /session/{id}/wait`) surfaces the
same three fields from the most recent `goal.stalled` record, reset by
`goal.set`/`goal.eval`/`goal.achieved` exactly like `attempt` already is.

### Self-re-arm: park, don't die

**Superseded by NEP-4849 — see "Worker-turn exit-park and activity-driven
resume" further down.** The section below describes the fix as it shipped
originally for issue #61: the retryable budget's exhaustion stayed *inside*
`PursueGoal`, retrying the same directive on the loop's own next iteration
without ever returning. That "park in-process" shape is kept here verbatim
as the historical record of the design this section chose over the rejected
server-timer alternative — the choice to retry via the ordinary turn loop
rather than invent new scheduling state is still the right call and is
unchanged. What changed is what happens once the budget is *actually*
exhausted: staying inside `PursueGoal` to retry turned out to have its own
cost in production — the run slot stayed pinned to the parked loop for the
whole outage, so a prompt queued during a long provider outage could only
ever be injected mid-turn into a doomed attempt, never dispatched as its own
ordinary turn the way it would against any other idle session. NEP-4849
changes the exhaustion branch to exit `PursueGoal` instead (freeing the run
slot) while keeping this section's classification, schedule, and budget
(`goalRetryableMaxAttempts`, `goalRetryableBackoff`) completely intact.

This is deliverable 4 of the issue, and the one with a real design choice
behind it. Two shapes were on the table:

- **(chosen) Park the turn in-process, bounded by `MaxTurns`/wall-clock.**
  When the retryable budget is exhausted, `promptTurnWithRetry` returns a
  distinguished `*goalRetryableExhaustedError` (still wrapping the
  underlying error and its class) instead of the bare error.
  `PursueGoal` recognizes this type and, instead of clearing the goal the
  way it clears a deterministic exhaustion, retries the *same directive* on
  the next iteration of its own turn loop — which, because that consumes an
  ordinary turn (the `for turn := 1; ...; turn++` loop's own increment),
  reaches exactly the same already-durable, already-resumable "max turns
  exhausted" terminal state (`goal` left **active**, `turn.end` outcome
  `max_turns_exceeded`) that an ordinary long-running goal reaches today —
  or, if `MaxTurns` is unlimited (0), keeps parking indefinitely, each cycle
  bounded by real wall-clock backoff time rather than hot-spinning, which is
  the same opt-in "no turn limit" contract `MaxTurns == 0` already carries.
- **(rejected) Have the server re-arm the goal automatically after a
  cooldown.** A `Server`-side timer that re-POSTs `/session/{id}/goal` once
  some cooldown elapses after an exhaustion. Rejected because it adds an
  entirely new piece of state to the state machine (a scheduled, in-memory-
  only re-arm timer, alongside `goalState`, that does not
  survive a process restart and duplicates logic the loop already has) for a
  case the chosen design already handles for free by reusing an existing,
  already-durable terminal state. It would also require deciding a *second*
  budget (how many cooldown-triggered re-arms before really giving up) on
  top of the retryable-class budget this fix already introduces.

The result: a retryable-class exhaustion is **never** a dead stall requiring
an operator to notice silence and re-POST by hand (the exact zombie shape the
original incident report for this document, above, describes) — it is
either an ordinary "keep working, same directive" continuation, or, once
`MaxTurns` is reached, the same resumable "max turns" pause every other
long-running goal can hit. Every state along the way is durably explained by
a `goal.stalled` record naming the retryable class, not inferred after the
fact.

See `engine/goal.go`'s package doc ("Round 4") for the full state diagram
and `TestPursueGoalRetryableErrorLongBackoffThenRecovers` /
`TestPursueGoalRetryableBudgetExhaustedParksInsteadOfClearing` for the
tests (both run inside a `testing/synctest` bubble — no real sleeps, per
AGENTS.md).

## Evaluator resilience: advisory failures, bounded terminal (NEP-4792)

### Incident

Round 3 above closed the "evaluator failure leaves a zombie goal" hole by
clearing the goal on ANY `evaluateGoal` error — a provider error, or two
unparseable replies in a row. That traded one incident for another:
production data showed two fleet boxes die mid-HEALTHY-work because the
tool-less evaluator call hit a transient provider hiccup (or, once, a
stretch of oddly-worded replies neither attempt could parse) while the
worker model itself was making fine progress. Unlike a worker-turn error —
expensive to retry blindly, see `promptTurnWithRetry`'s non-idempotency doc
above — a failing evaluator call risks nothing by being retried or, failing
that, simply skipped for one turn: the worker keeps working either way, and
the only thing a bad verdict can do wrong is delay noticing completion, not
corrupt anything.

### The fix: in-boundary retry, then advisory failure, then a bounded terminal

`evaluateGoal` now rides out a failure in-boundary before the boundary ever
counts as "failed," mirroring `promptTurnWithRetry`'s own error
classification:

- A provider error classified `provider.AsRetryable` gets the SAME
  retryable schedule and budget the worker turn uses
  (`goalRetryableBackoff`, `goalRetryableMaxAttempts`) — the two paths
  share provider weather, so they share a budget's shape, each keeping its
  own counter.
- A non-retryable provider error is not retried in-boundary at all (the
  call is cheap; a permanently broken provider needs the boundary-failure
  path below, not a wasted second attempt).
- An unparseable reply still gets its original one extra attempt, but that
  attempt now uses a STRICTER system prompt (`goalEvaluatorStrictSystem`)
  instead of repeating the same instructions verbatim to a model that
  already failed to follow them once.

If `evaluateGoal` still errors after all that — the retryable budget
exhausted, a non-retryable error, or two unparseable replies even with the
stricter re-ask — the boundary "fails," but failing a boundary no longer
clears the goal. `PursueGoal` journals a durable `goal.eval_failed` record
(carrying the error and the CONSECUTIVE failure count), substitutes a fixed
evaluation-unavailable notice for the next turn's guidance — never the raw
error text, and never a stale NOT-MET reason from turns ago — waits a short
backoff (`goalRetryDelay`, keyed on the consecutive count), and `continue`s:
the worker gets another ordinary turn. A later boundary that DOES parse a
verdict (MET or NOT MET) resets the consecutive count to zero — the horizon
below is about a STREAK, not a lifetime total, so one good evaluation undoes
any number of prior bad ones.

### The horizon: a bounded, loud terminal

Infinite advisory failures would just be Round 3's zombie-goal risk wearing
a disguise (a goal that LOOKS active but whose evaluator has been dead for
hours, silently). After `goalEvalFailureLimit` (5) CONSECUTIVE failed
boundaries, `PursueGoal` clears the goal with a dedicated reason ("goal
evaluator failed at N consecutive turn boundaries") and returns a distinct
sentinel error type (`*goalEvaluatorExhaustedError`, recognized via
`IsGoalEvaluatorExhausted`) instead of a bare error — a caller can tell this
terminal apart from an ordinary worker-turn exhaustion via `errors.As`,
never by string-matching `GoalReason`. Unlike every advisory boundary below
the horizon, this terminal DOES emit `session.error`: it must be LOUD, since
past this point nothing else will ever explain the goal's silence. The
server (`server/journal.go`) maps it to a dedicated `turn.end` outcome,
`outcomeEvaluatorExhausted` ("evaluator_exhausted"), a sibling of
`outcomeContextExhausted`/`outcomeMaxTurnsExceeded` — consumers never have
to string-match `GoalReason` to distinguish it.

`GoalSummary`/`GET /session` surface the current consecutive count as
`eval_failures` (omitted at zero), reset on any `goal.eval` /
`goal.achieved` / `goal.cleared` / `goal.updated` record, exactly mirroring
how `attempt`/`retryable`/`waiting` already reset on the worker-retry path.
`goal.eval_failed`, like `goal.stalled`, is a pure trace record on resume —
`LoadSession`'s fold does not change resume state from it, only the
in-memory consecutive counter used while the loop is live.

Five is deliberately much smaller than `goalRetryableMaxAttempts` (12): by
the time a boundary counts as "failed" at all, the in-boundary retryable
budget has already ridden out one boundary's worth of ordinary provider
weather, so five separate TURNS of failure (each potentially minutes apart,
each after its own full worker turn) is a much stronger signal of a truly
broken evaluator than exhausting a single boundary's in-boundary retry
budget ever is.

See `engine/goal.go`'s package doc ("Round 6") for the full narrative,
`TestPursueGoalEvaluatorUnparseableTwiceIsAdvisory`,
`TestPursueGoalEvaluatorRetryableErrorRecoversWithinBoundary`, and
`TestPursueGoalEvaluatorTerminalAfterConsecutiveFailureLimit` (all
`testing/synctest`-bubbled) for the tests, and
`server/goal_eval_resilience_test.go` for the server-side outcome/journal
coverage.

## Worker-turn exit-park and activity-driven resume (NEP-4849)

### Incident

OpenRouter returned HTTP 404s for a worker turn — a genuinely non-retryable,
non-overload failure, so `provider.AsRetryable` correctly classified it as
the fast, 3-attempt/~5s deterministic budget (`goalWorkerRetries`), not the
long retryable one. That budget exhausted in seconds; the goal cleared
(`goal.cleared`), `session.error` fired, and the box then sat idle for
**hours** with nothing further ever explaining or resuming it — a human had
to notice the silence and manually re-`POST /goal`, the exact zombie-adjacent
failure mode "The fix" above was originally meant to close, just reached from
the "successfully explained, then abandoned" side instead of the "silently
zombied" side.

The retryable tier's own #61 fix (see "Self-re-arm: park, don't die" above)
was too passive in the opposite direction: it never actually left
`PursueGoal`, so the run slot stayed pinned to the parked loop for the
**entire** outage — a prompt queued during that outage (see the Prompt queue
section of AGENTS.md) could only ever be injected mid-turn into a doomed
worker attempt, never dispatched as its own ordinary turn the way it would
against any other idle session. And every parked cycle re-spent a fresh
`goalWorkerRetries`-shaped schedule internally, with no cross-cycle memory of
how long the outage had already run.

### The fix: exit-park both tiers, resume on activity

Every way a worker turn can exhaust its retry budget — the deterministic
tier, the retryable tier, or the non-idempotency gate stopping retries early
once a tool has already executed this attempt (see "Retries are not
idempotent" above) — now returns OUT of `PursueGoal` entirely instead of
either clearing the goal or looping in place. The loop journals a durable,
generation-gated `goal.parked` record (gated exactly like `goal.stalled`/
`goal.eval_failed` above — a park racing a concurrent `UpdateGoal` is
silently discarded, never attributed to a condition that is no longer
current) and returns a distinct sentinel, `*goalWorkerParkedError`
(`engine.IsGoalWorkerParked`), **without** ever calling `clearGoal` —
`goalActive` stays true, `ActiveGoal()` keeps reporting the same condition,
and `LoadSession` folds `goal.parked` as a pure trace record, exactly like
`goal.stalled`. Unlike `goal.stalled`/`goal.eval_failed`, whose `Reason`
carries the raw `err.Error()` text, `goal.parked`'s `Reason` is deliberately
CLASSIFIED (`classifyGoalWorkerError`) — never a provider's raw error text —
because a park is a durable, potentially long-lived terminal that an
operator-facing surface can read long after the triggering request and its
raw provider detail are gone, unlike the two per-attempt trace records that
are read close to the moment they were written.

Freeing the run slot this way is what closes the #61 retryable-tier gap
above: a queued prompt dispatches as a normal turn the instant the slot is
free, and the server's **pre-existing** activity-driven auto-arm
(`maybeAutoArmGoal`, upstream of `engine` — see AGENTS.md's "Prompt queue"
section) re-enters the loop with a fresh `PursueGoal` call the next time any
ordinary prompt turn completes — no new timer, no new resume machinery, the
same mechanism an ordinary idle goal already relies on. `runGoal`'s own tail
deliberately never auto-arms itself (the pre-existing anti-churn property
documented at `server/handlers.go`'s `maybeAutoArmGoal` — a park does not
immediately respawn a loop against an empty queue).

On the server side, a worker-parked sentinel maps to `session.error` plus a
distinct `turn.end outcome=worker_parked` (`server/journal.go`'s
`turnEndOutcome`), and `goalTracker` folds the `goal.parked` record into a
third `paused` presentation arm — `pause_reason: "worker_failure"` — sitting
between the existing `"restart"` (boot-time, no loop was ever attached) and
`"provider-backoff"` (the loop IS alive, merely waiting) arms in
`pauseView`'s precedence. `compositeState` forces `idle` for `worker_failure`
exactly like it does for `restart`: no loop is actually driving the goal
until the next auto-arm or an operator re-POST, unlike `provider-backoff`,
which keeps reading `goal-running`. The `worker_failure` presentation resets
everywhere `restart`'s does (`goal.set`/`achieved`/`cleared`/`updated`,
`handleGoal`'s re-arm branch) plus in `maybeAutoArmGoal`'s own successful
arm, so a resumed loop is never seen carrying a stale pause.

There is deliberately **no streak horizon** on parking, unlike the
evaluator's `goalEvalFailureLimit` (5) bounded terminal above: parking is
immediate at exhaustion, every time, with no cross-park counter. A parked
goal stays parked-and-armed indefinitely until either activity resumes it or
an operator issues `DELETE /session/{id}/goal` — the only clear path a
parked goal has.

### An ambient, model-facing signal

The durable `goal.parked` record and the server's boot-only `goal.paused`
presentation both explain a park to an OPERATOR looking at the session from
outside. Neither says anything to the MODEL itself: an agent prompted
mid-outage — a queued prompt dispatching once the exit-park frees the run
slot, or any other ordinary turn — would otherwise see nothing indicating a
supervising goal exists, is still armed, and will resume on its own.
`engine/goal_parked_status.go` closes that gap the same way `mcp_status.go`
and `process.go` already do for their own degraded/live states: a small
ambient text block — a third occupant alongside the process and MCP
segments — computed fresh from live `Session` state
(`goalParked`/`goalParkedReason`/`goalParkedAttempts`) and appended only to
the newest user message of a request that is NOT itself one of this loop's
own worker turns (`PursueGoal`'s `clearGoalParkedAtEntry` call makes that
structural: the flag is always false again before this loop's very first
worker turn of a resumed run). The text is deliberately CLASSIFIED, matching
the record's own leak rule — never the raw provider error.

This signal is **not persisted** and does not survive a process restart —
`LoadSession` never restores it. That is a real, accepted asymmetry: after a
restart mid-park, a fresh `Prompt` call sees no ambient block at all.
Visibility in that case comes from a different surface entirely — the
server's boot-only `goal.paused` presentation (`pause_reason: "restart"`),
which is operator-facing, not model-facing, and reads the durable
`goal.parked` trace record directly rather than this runtime-only field.

### The asymmetry: context overflow still clears

Context overflow (issue #62) is the one deliberate exception that keeps
clearing exactly as before this round. Every other worker-turn exhaustion
this round covers is a failure that MIGHT resolve if the loop simply waits
and tries again later (a dead provider that gets fixed, an outage that ends,
an operator intervening) — parking is a bet that time helps. Context
overflow can never resolve by waiting: the same, now-too-long request fails
identically on every future attempt no matter how long the goal sits parked,
so parking it would just be a slower-burning zombie, not a fix. Clearing
immediately, with a reason a human or automation can act on right away
(compact, shorten the goal, start over), is strictly more honest than a park
that can never self-resolve.

See `engine/goal.go`'s package doc ("Round 7") for the full narrative and
state diagram, `TestPursueGoalWorkerFailsPermanentlyParksGoal`,
`TestPursueGoalRetryableBudgetExhaustedParksInsteadOfClearing`, and
`TestPursueGoalStaleWorkerFailureDiscarded` (`engine/goal_update_test.go`,
proving a park never lands for a stale generation) for the engine-side
tests; `server/goal_worker_park_test.go`
(`TestTurnEndOutcomeWorkerParked`, `TestForcesIdlePauseIncludesWorkerFailure`,
`TestGoalTrackerPauseViewPrecedence`, `TestGoalWorkerParkFreesRunSlotForQueuedPrompt`,
`TestGoalWorkerParkResumesOnNextPromptActivity`,
`TestGoalWorkerParkPauseSurvivesRestartAsRestartReason`) for the server-side
outcome/pause/resume coverage; and `engine/goal_parked_status_test.go` for the
ambient-segment tests.

## Operational reliability

Goal-supervised turns are retried by the loop above and fail visibly with a
journaled reason (`goal.stalled`, then `goal.parked` — never `goal.cleared`
— if every retry budget, deterministic or retryable, is exhausted; see
"Worker-turn exit-park and activity-driven resume" above. Context overflow
remains the one exception that still clears). Plain `prompt_async` turns get
none of that: they are not retried, and a provider stream that dies mid-turn
silently ends them. The
signature of that silent death is a final assistant message containing
reasoning parts only — no text, no tool_call. Consequently, multi-step or
long-running work dispatched over an unreliable link should be wrapped in a
goal even when no evaluation condition is actually interesting, just for the
retry/visibility behavior; and anything polling a plain prompt must treat an
idle session whose last assistant message is reasoning-only as a failure to
investigate, not as completion.
