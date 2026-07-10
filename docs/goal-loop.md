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
   state-machine diagram in the `goal.go` package doc.

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

- `evaluateGoal`'s own error path (a provider error while asking the
  evaluator) is unchanged: it still returns the error directly without
  clearing the goal, since that failure is in the evaluator, not the worker,
  and the goal's own state is still accurate and worth preserving for a
  human-triggered resume.
- Two unparseable evaluator replies in a row (`errEvaluatorUnparseable`) is
  unchanged for the same reason.
- `goal.stalled` is a pure trace record: it never flips `goalActive` by
  itself (see `LoadSession`'s `scanLog` switch in `store.go`).

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

## Operational reliability

Goal-supervised turns are retried by the loop above and fail visibly with a
journaled reason (`goal.stalled`, then `goal.cleared` if every DETERMINISTIC
retry is exhausted — a retryable-class exhaustion parks instead, see above).
Plain `prompt_async` turns get none of that: they are not
retried, and a provider stream that dies mid-turn silently ends them. The
signature of that silent death is a final assistant message containing
reasoning parts only — no text, no tool_call. Consequently, multi-step or
long-running work dispatched over an unreliable link should be wrapped in a
goal even when no evaluation condition is actually interesting, just for the
retry/visibility behavior; and anything polling a plain prompt must treat an
idle session whose last assistant message is reasoning-only as a failure to
investigate, not as completion.
