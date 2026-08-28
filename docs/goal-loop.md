# Goal loop

This document is the technical system of record for the current goal-loop
contract. See `docs/history/goal-loop-resilience.md` for incident history and
the sequence of earlier fixes.

## Control loop and evaluator

`Session.PursueGoal(ctx, condition, GoalOptions)` drives the ordinary `Prompt`
loop toward a natural-language completion condition. Turn 1 prompts the raw
condition; after **every** turn an independent, TOOL-LESS evaluator model
(`GoalOptions.Evaluator`, resolved through the same `Config.Providers` registry,
`MaxTokens` 256) is asked to answer `MET: <reason>` / `NOT MET: <reason>`
(parsed leniently). The evaluator request always pins `message.EffortOff`
(`runEvaluator`, `engine/goal.go`) — it is a classifier, not a reasoning task,
and it never inherits the session's own effort level. On openaicompat,
`EffortOff` sends the literal `"off"`; on anthropic, it emits no thinking
block — both routes now spend none of the evaluator's 256-token budget on
reasoning. (Issue #124.) The openai Responses route is a known residual:
`reasoningEffort` (`provider/openai/transcode.go`) omits the `reasoning`
object for `EffortOff` exactly as it does for `EffortUnset`, and a
gpt-5-class model reasons by default with no adapter-level way to disable
it — so an evaluator on that route can still spend its budget on reasoning.
A NOT MET verdict re-prompts
with a fixed-template guidance message carrying the reason; MET returns
`Achieved`. `MaxTurns` (0 = unlimited) bounds it. Evaluation is advisory: a
retryable-class provider error from the
evaluator call rides the matching in-boundary backoff before the boundary
counts as failed — the long weather-tier schedule
(`goalRetryableMaxAttempts`, ~30min) for `overloaded`/`rate_limited`/
`server_error`, or the short stream-truncation tier
(`goalStreamTruncatedMaxAttempts`, 3 attempts, ~5s) for a stream cut before
its terminal event — `runEvaluatorWithRetry` mirrors `promptTurnWithRetry`'s
own per-class split exactly (see below); two unparseable replies in a row
(the second re-asked with a stricter prompt) or a non-retryable provider
error also fail the boundary immediately. A failed boundary no longer
clears the goal — it journals a durable `goal.eval_failed` record (carrying the consecutive
failure count), substitutes a fixed evaluation-unavailable notice for the next
turn's guidance in place of the evaluator's text, and `continue`s: the worker
keeps working. Any later boundary that DOES parse a verdict (MET or NOT MET)
resets the consecutive count to zero — the horizon is a streak, not a
lifetime total. Only after `goalEvalFailureLimit` (5) consecutive failed
boundaries does the loop treat the evaluator as durably broken: it clears the
goal with a dedicated reason, and the server maps that terminal to a
`session.error` plus a distinct `turn.end outcome=evaluator_exhausted` — loud
and machine-distinguishable, since every failure below the horizon is
deliberately silent apart from the journaled record.
Durable `goal.set` / `goal.eval` / `goal.eval_failed` / `goal.parked` /
`goal.achieved` / `goal.cleared` records land in the session log, so
`LoadSession` restores an active goal (condition only; counters reset) via
`Session.ActiveGoal()` — resume never auto-runs it, the caller decides. The
loop also emits `goal.*` engine events so the server journals them. Config
`goal_evaluator_model` supplies the evaluator for `harness run -goal` and
`POST /session/{id}/goal`.

## Evaluator transcript bounds

The evaluator's own `CONVERSATION TRANSCRIPT` field is BOUNDED, independent
of whatever context window the MAIN session model has and independent of
whether automatic compaction (below) has fired at all.
`renderConversationBounded` (`engine/goal.go`, called from `runEvaluator`)
replaced the old unconditional `renderConversation(s.History())`: box
bx-01m0x8996, a real long session, died with "engine: goal evaluator failed
at 5 consecutive turn boundaries: context exhausted: prompt 245332 tokens >
limit ..." because the evaluator's prompt grows with the WHOLE session
transcript forever — unlike the main session, which automatic compaction
protects, the evaluator had no bound of its own at all. The budget comes
from `goalEvaluatorTranscriptBudgetBytes`, which resolves the EVALUATOR
model's own context window via `modelContextWindowLookup`
(`modelmeta.ContextWindow`) — the same table automatic compaction's
`resolveContextWindow` (`engine/context_window.go`) reads, called
DIRECTLY rather than through `resolveContextWindow` itself: that
function's `minAutoContextWindowTokens` floor answers "should automatic
compaction ARM for this window," so a real, small, KNOWN window (gpt-4's
documented 8,192 tokens) reports identically to a genuinely UNRECOGNIZED
model (0, disabled) — conflating them would give a real small-window
evaluator a budget roughly double its actual limit, the exact overflow
class this fix closes. `goalEvaluatorTranscriptBudgetBytes` trusts ANY
positive, known window from the table, however small, and falls back to
`goalEvaluatorFallbackContextWindowTokens` (mirroring
`minAutoContextWindowTokens`'s value) only when the model has NO entry at
all. It also reserves headroom for the system prompt and MaxTokens' output
budget, and applies a conservative fraction (`goalEvaluatorContextBudgetFraction`,
0.5) on top of the same crude ~4-bytes-per-token estimate
(`bytesPerTokenEstimate`) automatic compaction's own resilience fallback
uses — reused, not reinvented.
`renderConversationBounded` walks history from the NEWEST message backward,
accumulating rendered blocks until the budget would be exceeded, and
prefers "summary + tail" for free rather than summarizing a second time:
Compact (`engine/compact.go`) already splices its own summary message in
place of whatever range it folded, tagged with the `compactionSummaryIDTag`
prefix, so the backward walk simply STOPS the instant it includes such a
message — everything before it is already captured there. The newest
message is always kept regardless of budget (an empty transcript can never
be assessed); a truncated transcript is prefixed with
`goalEvaluatorTruncationNotice` so the evaluator, and an operator reading a
later `goal.eval` record, never mistakes a bounded view for the whole
session.

## Retryable-class backoff

A worker-turn error (`s.Prompt` failing) is retried by `promptTurnWithRetry`
on one of FOUR independent budgets, chosen by classification via
`provider.AsRetryable` — never by matching error text.

One class skips every budget. Before it selects a budget,
`promptTurnWithRetry` tests `provider.AsPermanent` — its fail-fast check
(`engine/goal.go`) — and fails fast: a permanent error gets ONE attempt and
no retry.
`provider.MarkPermanent` marks a malformed request shape. The anthropic
adapter applies it to an HTTP 400 `invalid_request_error`, and to the same
error type mid-stream (`provider/anthropic/anthropic.go:114` and `:484`),
only after `parseContextOverflow` rules overflow out — the two are disjoint.
A retry never repairs a malformed request, and each attempt costs a full
turn at full input price. A permanent error still PARKS, exactly like every
budget exhaustion; it never clears. `permanent` is threaded through only to
select a more accurate classified reason and tier name
(`classifyGoalWorkerError`, `goalWorkerParkedError`), so an operator can
tell a single-attempt park from `goalWorkerRetries`+1 identical attempts.

One shape is wrapped `provider.MarkPermanent` by the adapter but is
DELIBERATELY EXCLUDED from this fail-fast branch: `provider.AsProviderExhausted`
— an ACCOUNT-level usage/quota wall (PR #174's `provider.ErrKindProviderExhausted`,
originally added for task-child resumability; see `engine/session_manager.go`'s
`FailKindProviderExhausted`). An adapter marks it permanent for ordinary
HTTP-retry purposes (no short backoff schedule outlives a monthly quota),
but a wall lifts on its own, unchanged, so treating it as a doomed malformed
request silently kills goal supervision on the very first usage-limit
rejection. Live evidence: box bx-01m0x8996 parked after "1 permanent-tier
attempt(s)" on "You have reached your specified API usage limits" and never
resumed without an operator `DELETE` + re-register. `promptTurnWithRetry`
and `PursueGoal`'s worker-turn handling both check
`provider.AsProviderExhausted` explicitly and fold a positive result into
their local `retryable`/`class` bookkeeping (`class` set to the dedicated
marker `goalClassProviderExhausted`, never one of `provider.RetryableClass`'s
real values) — reusing the existing stall/park recording machinery rather
than adding a fourth field throughout.

A deterministic
failure (not classified retryable, not permanent) gets `goalWorkerRetries` (2) additional
attempts on the short schedule (~5s total: 1s, then 4s). A provider error
classified `overloaded`/`rate_limited`/`server_error` gets a separately
budgeted `goalRetryableMaxAttempts` (12) backoff (~30min total, jittered, 5s
doubling to a 5min cap) that never spends the deterministic budget. A
provider error classified `provider.RetryableStreamTruncated` — a response
stream that died before its terminal event, with no HTTP status or inline
error to classify from (see the idle-stream watchdog below) — gets its own
`goalStreamTruncatedMaxAttempts` (3) budget on the SAME short schedule the
deterministic tier uses (~5s total): truncation is retryable, but it is not
weather — waiting longer never raises a stream ceiling, and every retry
re-prompts a full turn at full input cost — so it must ride neither the fast
deterministic budget nor the long weather-tier one. A `provider.AsProviderExhausted`
failure gets its OWN `goalProviderExhaustedMaxAttempts` budget (equal in
size to `goalRetryableMaxAttempts`, on the identical jittered schedule) —
never the ordinary weather counter, so a concurrent overload spell and an
account wall in the same turn can never silently share or steal from one
another's budget. It deliberately rides the SAME schedule ordinary weather
uses rather than computing a wait from the provider's own `RecoverHint`
("you regain access on <date>"): `RecoverHint`'s format varies by provider
and by plan (see `provider.Error.RecoverHint`'s doc comment), so it is
never parsed into a duration, only ever quoted verbatim to a model-visible
caller. A wall that clears within the budget (a burst rate limit that
reached this classification, or a short-lived cap) resumes the worker turn
— and the whole goal — with NO operator action; a wall measured in hours or
days still exhausts the budget and parks, but honestly classified via
`classifyGoalWorkerError`'s dedicated branch ("provider account usage limit
exhausted the retry budget"), never as a permanent, unretriable request.
Every attempt records a
`goal.stalled` record regardless of tier, so the loop is never silent.
Exhausting ANY of the four budgets — or the non-idempotency gate stopping
retries early once a tool has already executed this attempt — PARKS the goal
instead of clearing it: `PursueGoal` exits, journals a durable, CLASSIFIED
`goal.parked` record (never raw provider error text — the same leak rule
`goal.eval_failed` follows), and returns a distinct `*goalWorkerParkedError`
sentinel (`engine.IsGoalWorkerParked`) WITHOUT calling `clearGoal` —
`goalActive` stays true, the condition is untouched, generation-gated exactly
like `goal.stalled`/`goal.eval_failed` so a park racing a concurrent
`UpdateGoal` is silently discarded rather than attributed to a condition the
model never saw. This supersedes both this package's earlier
deterministic-tier clear and GitHub issue #61's in-loop retryable-tier
self-re-arming `continue` — the latter pinned the run slot to the parked loop
for the whole outage; exiting instead frees the slot, so a queued prompt
dispatches as an ordinary turn during a long outage instead of only ever
being injected mid-turn into a doomed attempt. Context overflow (issue #62)
is the one deliberate exception and still clears immediately, never parks:
no amount of waiting fixes an oversized request, so parking it would just be
a slower-burning zombie instead of a fix. Parking has no streak horizon
(unlike the evaluator's 5-boundary terminal above) — every exhaustion parks
immediately, and `DELETE /session/{id}/goal` remains the only clear path for
a parked goal.

## Directive reuse across retries

Each retry re-issues the SAME directive, and `Prompt` appends whatever text
it gets as a brand-new user message — it has no notion of "this is a retry,
do not duplicate." Left alone, N failed attempts leave N unanswered copies of
one directive, and every LATER request pays for all of them. `Prompt`
persists each copy before the provider call that fails, so the duplicates
reach the durable log, not just live history.

`promptTurnWithRetry` therefore never appends a second copy for the common
case. It tracks one `anchorID`, naming the point right before this turn's
CURRENT, still-unanswered directive — starting as `lastMessageID`, captured
once before attempt 1 — then dispatches each retry one of three ways
(`engine/goal.go`, `tailAfterAnchor` shares the anchor-to-tail lookup; see
docs/design/goal-retry-directive-reuse.md):

- Attempt 1 calls `Prompt`, which appends the directive.
- A retry whose tail after `anchorID` is EXACTLY the previous attempt's
  unanswered directive (`directiveReuseEligible`) calls `runAgenticLoop`
  instead. That runs the turn loop against history as it stands and appends
  nothing, so the existing message is answered rather than duplicated.
- Any other tail falls back to `dropUnansweredDirective` plus `Prompt`, then
  re-anchors: `anchorID` moves to `lastMessageID`, the point right before
  the fresh directive `Prompt` is about to append. A later attempt's reuse
  check then measures from that new directive, never from the turn's
  original start.

`runAgenticLoop` is `Prompt`'s own loop body, split out unchanged
(`engine/engine.go`). `Prompt` still appends and then calls it, so `Prompt`'s
observable behavior is identical: same events, same `emitStatus`, same usage
accounting. Note that `maybeAutoCompact` stays in `Prompt` and does NOT run
on the reuse path. That is deliberate, and the reason is that history did
not grow: the reuse path is reachable only when the tail is exactly one
message, so no new completed turn appeared to fold since attempt 1 already
ran the check. (`maybeAutoCompact` folds only COMPLETED turns, so it would
never have folded the unanswered tail directive itself.) One narrow
residual: history sitting right at the threshold, where appending a
directive would tip it over, no longer triggers a mid-outage fold. That is
accepted — the outage that piles up retries is also when the summarizer's
own provider call fails, and compaction is best-effort anyway.

`dropUnansweredDirective` remains the fallback for the interrupted-turn tail
(the directive plus a partial assistant message and its synthetic
tool-result message), and for any tail a denied tool call or delivered mail
makes undroppable. It anchors on a message ID, never on a history length,
and `isSafeToDropDirectiveTail` approves only that interrupted-turn shape
and the bare directive. Any other tail is left untouched — a denied tool's
result, or an already-delivered "OPERATOR MESSAGES" block, must never be
discarded. It mutates only live history and can never retract a journaled
record, which is why the reuse path above, not a retraction, is what keeps
the log clean. `promptTurnWithRetry`'s re-anchor above bounds an undroppable
residue's cost to ONE extra duplicate directive for the rest of the turn,
never one per remaining attempt: re-anchoring past it lets reuse resume on
the very next attempt instead of re-appending against a tail that can never
shrink back to a droppable shape again.

## Idle-stream watchdog

An idle provider stream — one that goes silent with no bytes, no
`EventDone`, no error, ever — is bounded by a per-request idle-stream
watchdog (`engine/stream_watchdog.go`, `Config.StreamIdleTimeout`, config key
`stream_idle_timeout_s`): every stream event resets its timer, and on expiry
it cancels the request's child context and converts the resulting
cancellation into a classified `provider.RetryableStreamTruncated` error
instead of an anonymous "context canceled" — this is what feeds the
stream-truncation tier above. It defaults to 5 minutes (mirroring Codex's
`stream_idle_timeout_ms`), a negative value disables it, and it guards the
worker turn, the goal evaluator, and the compaction summarizer's streams
alike (`armIdleWatchdog` wraps all three, so a silent stream at any of them
can no longer wedge the session forever while holding the run slot).

## Automatic compaction fallback

Automatic compaction's over-threshold check
(`maybeAutoCompact`/`estimatePromptTokensFromHistory`, `engine/compact.go`)
has its own resilience fallback: a provider route that reports all-zero
input usage on a turn that DID complete is treated as missing data, never as
"0 tokens, never over" — the check falls back to a crude ~4-bytes-per-token
estimate walked from the actual session history so the overflow-prevention
layer keeps functioning instead of going permanently dark on that route,
which otherwise runs to a hard context overflow that clears (never parks) an
active goal.

## Context-window resolution

The goal loop uses the session's normal context-window policy. A positive
`Config.ContextWindowTokens` value is pinned for the session. A negative value
is an explicit opt-out. Otherwise, `resolveContextWindow` derives the window
from the static `modelmeta` table and `SetModel` re-derives it after a model
switch.

A known model below `minAutoContextWindowTokens` remains valid but does not arm
automatic compaction. A registry miss returns `ErrUnknownContextWindow`. When
`Config.RequireContextWindow` is true, session creation, model selection, and
prompting refuse that model. When it is false, the session retains the legacy
disabled-compaction behavior.

Read `docs/models-and-providers.md` for the refusal policy and
`docs/design/context-compaction.md` for compaction behavior.

## Server state and recovery

On the server, a worker-parked sentinel maps to `session.error` plus a
distinct `turn.end outcome=worker_parked`, and `goalTracker` folds the
durable `goal.parked` record into a third `paused` arm (`pause_reason:
"worker_failure"`, alongside the existing boot-only `"restart"` and live
`"provider-backoff"`) — `compositeState` forces `idle` for it, and for a
restart pause, unless a turn is actually running, which reads `busy`: forced
idle must never mask a live turn (an ordinary prompt, or the resume prompt
that eventually re-arms the goal, can be streaming while the goal itself
sits parked), whereas provider-backoff's loop is merely waiting and keeps
reading `goal-running` regardless of whether a turn happens to be running.
Resume needs no new machinery: the existing activity-driven
`maybeAutoArmGoal` re-arms any active goal — parked or not — the next time an
ordinary prompt turn completes, resetting the `worker_failure` presentation;
`runGoal`'s own tail deliberately never auto-arms (the same anti-churn
property that already stops a freshly-parked goal from immediately
respawning a loop against an empty queue).

## Structured server logging

`harness serve` can also make this turn/goal lifecycle visible on stderr:
`server.Options.Logger`, when set (`cmd/harness/main.go` wires a
`slog.NewJSONHandler(os.Stderr, nil)` logger into it for `serveCmd`), emits a
structured line at every `recordTurnEnd` call (INFO for outcome "completed",
WARN otherwise) and at the `goal.*`/`session.error` durable-record choke
points — a heartbeat for the life of the box instead of logging only at
boot/config/MCP wiring, matching Codex's own structured stream-retry
logging. Nil (the default) disables all of it; every call site nil-guards
first, so an unset Logger is exactly the prior silent behavior.

## Parked-goal ambient status

A worker-parked goal is also surfaced in-session, model-facing:
`Session.goalParked` (set when a park lands, cleared at every `PursueGoal`
entry) drives a third ambient status segment — alongside the process and MCP
segments — appended to the newest user message of any turn that is NOT
itself one of this loop's own worker turns, naming the classified reason and
stating the goal resumes automatically. It is runtime-only and never
persisted; after a process restart, visibility reverts entirely to the
boot-only `goal.paused`/`pause_reason: "restart"` presentation instead — a
deliberate, documented asymmetry.

## Updating an active goal

The condition itself is adjustable mid-loop. `Session.UpdateGoal` rewrites an
active goal's condition, journals a durable `goal.updated` record, and emits
`EventGoalUpdated` — same lock-and-emit-under-`s.mu` shape as `RegisterGoal`;
a same-condition update is a silent no-op, updating an inactive goal errors.
`PursueGoal` takes a per-turn snapshot (condition, a runtime-only generation
counter, active) instead of closing over the original parameter, so a live
loop picks up new text at its very next turn boundary — both the worker
directive and the evaluator call. The generation counter guards stale
verdicts: if `UpdateGoal` lands while an evaluator call for generation N is
in flight, a MET (or stalled) verdict for N is discarded on return — no
`goal.achieved`, no `goal.eval`, the loop just continues against the new
condition, never a false-positive completion against text the model never
saw. `ClearGoal` is unaffected — it keys on `goalActive`, not condition
equality, so it still stops the loop at every point it does today.

## Goal tool and host behavior

A built-in `goal` session tool (gated on `Config.GoalTool`) lets the model
inspect or drive its own goal in-process: no HTTP round-trip, no run-slot
claim. `status` reports `{active, condition}`; `set` arms a new goal via
`RegisterGoal` (errors telling the model to use `adjust` if a goal is already
active); `adjust` rewrites an active goal's condition via `UpdateGoal`. There
is deliberately **no `clear` action** — see below.

`Config.GoalTool` is on whenever `goal_evaluator_model` is configured, in
`harness run` and `harness serve` alike, entirely independent of the `-goal`
flag — a plain `harness run -p ...` with that config set still registers the
tool. But what happens after `set`/`adjust` differs by host: `harness serve`
auto-arms (see `maybeAutoArmGoal` below) — the loop actually starts running
once the current turn ends. Plain `harness run` (no `-goal`) has no such
auto-arm step: a tool-driven `set` call registers and journals the goal
(`goal.active` becomes true) but nothing ever calls `PursueGoal` for it, so
it never actually starts evaluating — the process runs its one `Prompt` call
and exits with the goal armed but inert. Only `harness run -goal <condition>`
itself drives `PursueGoal` to completion.

## HTTP goal updates and auto-arm

`POST /session/{id}/goal` on a busy session no longer flatly 409s. A running
goal loop updates its condition in place (`status: "updated"`, 200 — no
second loop, no run-slot claim; the loop picks it up at its next turn
boundary). A plain prompt holding the slot with no goal yet active registers
the goal (`RegisterGoal` needs no run slot) and then retries the claim once,
closing the race against that same prompt's own `runPrompt` tail: if the
retry wins the now-freed slot, the loop spawns immediately and the response
reports `status: "started"` (202); otherwise the prompt's tail is still
ahead of us, its own auto-arm check (`maybeAutoArmGoal`) will claim the slot
and spawn the loop itself once that tail finishes, and the response reports
`status: "armed"` (202) — either way the loop starts exactly once, never
zero times, never twice, no further client action needed. This is also how
the `goal` tool's own `set` action takes effect: arming a goal mid-turn, the
same auto-arm path starts the loop the instant the current turn ends. A
workdir held by a genuinely different session still 409s,
unchanged.

## Operator-only clear

No self-clear is deliberate: a goal-supervised agent must never be able to
cancel its own supervision from inside a running turn, so the `goal` tool
has no `clear` action — `DELETE /session/{id}/goal` remains the only clear
path, and it is operator-only.

## Deliberate exclusions

The goal loop is a **plan-artifact-free, gate-free** control loop: it is
`Prompt` plus a read-only evaluator call, with no plan document, no edit/plan
mode, and no permission gate.
