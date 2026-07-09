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
