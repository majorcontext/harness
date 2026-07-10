# Question/elicitation primitive: `ask_user`

## Motivation

`question.asked` has been reserved in the plugin event vocabulary since v1
(`plugin/hooks.go`'s `EventQuestionAsked`, documented in `PROTOCOL.md` as
"reserved, no emit site yet") with no tool, no emit site, and no wire
surface behind it. Agents run headless (goal loop) and interactively
(`prompt_async`) with no way to stop and ask the human something —
model-side, that gap gets papered over with a guess, or a tool call that
acts rather than asks first. This document proposes the primitive that
fills the reservation: one built-in tool, one concrete blocking-semantics
mechanism, and the wire/plugin surface it needs. Design only; no code or
API in this branch.

## 1. The tool: `ask_user`

One built-in tool, installed unconditionally like `bash`/`read_file` (see
`engine/engine.go`'s `newSession`). It takes a *batch* of questions in one
call, matching the reserved event shape (`questions: [...]`) exactly — a
model needing three related answers spends one turn-ending round trip, not
three.

```json
{
  "type": "object",
  "properties": {
    "questions": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "question": {
            "type": "string",
            "description": "The question text to show the user."
          },
          "options": {
            "type": "array",
            "items": { "type": "string" },
            "description": "Optional enumerated choices. Omit for a freeform text answer."
          },
          "multi": {
            "type": "boolean",
            "description": "Only meaningful with options: whether more than one may be selected. Defaults to false (single-select)."
          }
        },
        "required": ["question"]
      }
    }
  },
  "required": ["questions"]
}
```

Description (model-facing, `bash`'s tone): "Ask the user one or more
questions and stop until they answer. Use this instead of guessing when a
decision needs the user's input — a choice between options, a missing
piece of information, confirmation before an irreversible action. The
current turn ends here; the answer arrives as the next prompt."

`options`/`multi` are per-question, not call-wide, so a batch can mix a
yes/no question with a freeform one. No `required: bool` per question —
requiredness is a consumer/UI concern (§4), not something the engine
enforces.

## 2. Blocking semantics: the turn ends at the question

The central decision is not really "block" vs. "pending marker" — those are
two shapes of the same anti-pattern (parking a goroutine, or a tool result,
on an event that may never arrive) and both fight invariants
`engine/goal.go` and `message.ResolveOrphanToolCalls` already establish.
The decision: **`ask_user` always resolves synchronously and
deterministically, and its side effect is to end the current turn.**

In `runToolCall`'s built-in path, `ask_user`'s `Run`:

1. Persists a durable `question.asked` record (mirroring `recGoalSet` in
   `engine/store.go`) keyed on the tool call's own `CallID`, and sets
   in-memory `s.awaitingQuestion` (guarded by `s.mu`, same pattern as
   `goalActive`/`goalCondition`).
2. Emits the plugin `question.asked` event (its first emit site) and an
   analogous engine `Event` for `OnEvent`/SSE.
3. Returns an ordinary, **fully resolved** tool result: `is_error: false`,
   content `"N question(s) recorded (call <call_id>); waiting for the
   user's answer as the next prompt."`

Step 3 is load-bearing. The tool call is never left without a result —
there is no "pending" `tool_result` shape, because every provider wire
protocol this engine transcodes to requires a `tool_use`/`tool_call` to be
followed immediately by its result (see `message.ResolveOrphanToolCalls`'s
doc comment and the incident it fixes). `ask_user`'s result is real and
final the instant the tool runs; "waiting" is a *session*-level fact
(`s.awaitingQuestion`), never a *message*-level one — safer than a design
where the tool result itself claims "no answer yet" and the model is
trusted to notice and stop.

`Session.Prompt`'s tool loop (`engine/engine.go`) already exits early when
a round produces zero tool calls; this adds one more exit: **if any tool
call executed this round was `ask_user`, the turn ends here regardless of
`stop`.** Other tool calls batched into the same assistant message still
execute and get real results first; only the loop-continuation is skipped.
No new error path, no `session.error` — this is an ordinary end of turn.

**Why not block, with or without a timeout.** A channel-fed blocking `Run`
can be built abort-safe (the same `select`-on-`ctx.Done()` shape as
`waitGoalRetryBackoff`), but it holds `Session.Prompt`'s call stack — and
the session's one run slot (`claimForPrompt`) — for the entire wait. A
timeout that returns "no answer yet" avoids the indefinite hold but
reintroduces the pending-tool_result hazard: the model must decide whether
to poll again, and one that keeps re-calling `ask_user` on timeout is the
zombie-goal shape `goal.go`'s state machine exists to eliminate. **Why not
record-and-continue.** Returning "recorded, keep going" without ending the
turn defeats the point: the model needed the answer to proceed and now has
neither it nor a stopping point.

**Against the goal loop.** `PursueGoal` calls `promptTurnWithRetry`, which
calls `s.Prompt`; with turn-ends-at-question that returns a **nil error**
— indistinguishable, to the retry machinery, from a completed turn. Left
unhandled, the loop would immediately ask the evaluator, get NOT MET,
re-run the worker with the same unanswered question, and most likely ask
again — bounded by `MaxTurns`, so not a true infinite wedge, but it burns
the whole turn budget re-asking, then reports "max turns" indistinguishably
from an ordinary stall. The fix: a check in `PursueGoal`'s loop, parallel
to its `goalActiveWith(condition)` clean-stop checks — after a worker-turn
attempt succeeds, **before** calling `evaluateGoal`, check
`s.awaitingQuestion`. If set, return immediately —
`GoalResult{Achieved: false, Turns: turn, Reason: "awaiting_input"}`, nil
error — **without clearing the goal.** This is a new pause state alongside
ACTIVE/STALLED/ACHIEVED/CLEARED: it leaves `goalActive` true (unlike
CLEARED) and consumes no retry attempt (unlike STALLED). The no-zombie
invariant holds because `question.asked` is itself the durable record
explaining the pause, exactly as `goal.stalled` explains a retry. Resuming
(§3) re-enters with `Registered: true`, same condition.

**Interactive `prompt_async` sessions** need no special-casing: the turn
ends, `runPrompt` records `"completed"` (a question is not an error), the
session goes idle-with-a-pending-question (§3), and the human's reply
arrives as an ordinary next `prompt_async`.

**Restart/resume mid-question.** `LoadSession`'s `scanLog` gains a
`question.asked`/`question.answered` case, mirroring
`recGoalSet`/`recGoalAchieved`: `question.asked` sets `s.awaitingQuestion`
with no history repair needed, because — per the design above — the
`ask_user` call already has a complete, ordinary `tool_result` in history
the moment it was recorded. Unlike a mid-stream crash (what
`message.ResolveOrphanToolCalls`/`interruptedTurnError` defend against),
there is no orphaned `ToolCall` to repair here; that is the point of
resolving it synchronously rather than deferring it. `question.answered`
(written the moment a new prompt arrives while awaiting — see §3) clears
it on replay, same as `recGoalCleared` clears `goalActive`. A paused goal
restores as an ordinary active goal; the pause itself is not separately
persisted goal-side, since `question.asked`/`answered` already carries it.

## 3. Wire surface

**Event**, giving `question.asked` its emit site with the payload already
reserved:

```json
{
  "type": "question.asked",
  "session_id": "ses_...",
  "properties": {
    "call_id": "call_...",
    "questions": [
      { "question": "Which environment?", "options": ["staging", "prod"], "multi": false },
      { "question": "Anything else to note?" }
    ]
  }
}
```

`plugin.QuestionAskedProperties` joins `FileEditedProperties` et al. in
`plugin/hooks.go`. Absent `options` means freeform, absent `multi` means
single-select — matching the JSON Schema's own "omitted means default".

**Session status.** Reuse the composite-state field
(`sessionJSON.State`/`compositeState` in `server/handlers.go`) rather than
a second status axis: add `"awaiting-input"`, ranked *above*
`goal-running`/`busy` — the same reasoning that already ranks
`goal-running` above the raw busy bit ("a poller must not have to reason
about two fields," per `sessionJSON.State`'s own doc comment). A pending
question is not "idle" in the sense of nothing to do, so it becomes a
tri-state value, not `idle` plus metadata. The question detail itself
(`call_id`, `questions`) rides in a new `sessionJSON.Question`, present
exactly when state is `awaiting-input` — same shape as `Goal`/`LastTurn`.

**`GET /session/{id}/wait`** gains `until=awaiting-input` alongside `idle`
and `goal-done` (`server/wait.go`'s `waitConditionMet`): resolves
immediately if a question is already pending, otherwise blocks until one
appears or the wait times out. This is the park point the consumer loop in
§3/§4 actually needs — it wakes when a question *appears*, so the waiter
can go answer it. The inverse observer (waiting for paused work to
un-pause) needs no new condition: a pending question keeps the composite
state off `idle`, so `until=idle`/`until=goal-done` already cover it. No
separate `GET .../question` is needed: `GET /session/{id}` and the wait
response already carry the pending question the instant it's set.

**`POST /session/{id}/answer`** is the write side:

```json
{
  "call_id": "call_...",
  "answers": [
    { "question": "Which environment?", "selected": ["staging"] },
    { "question": "Anything else to note?", "text": "no" }
  ]
}
```

400s if `call_id` doesn't match the pending question (a stale/unknown
answer, e.g. after an orchestrator retry), 404s on an unknown session,
409s if nothing is pending. On success it persists `question.answered`
(clearing `s.awaitingQuestion`), formats the answers into one deterministic
text block, and delivers it through the **same path as `prompt_async`** —
a plain `Session.Prompt` user message. That is the "answer arrives as the
next prompt" mechanism from §2, not a side-channel into the already-
resolved tool_call.

Delivery splits on whether a goal is paused, because `PursueGoal`'s
contract forbids running it concurrently with `Prompt` (it drives Prompt
itself — goal.go's "Must not be called concurrently with itself or
Prompt"):

- **No active goal** (interactive session): `/answer` formats the answers
  and delivers them as a plain `Session.Prompt` user message — the
  ordinary `prompt_async` path, exactly as above.
- **Goal paused on the question**: `/answer` must NOT call `Prompt`
  itself — pairing a direct `Prompt` with a re-spawned `PursueGoal` races
  the invariant (two `Prompt` callers; `claimForPrompt` rejects one, but
  *which* one is a race), and a bare re-spawn drops the answer entirely,
  because `PursueGoal` hardcodes the raw condition as turn 1's directive.
  Instead, `/answer` persists `question.answered`, clears
  `s.awaitingQuestion`, and re-spawns `PursueGoal` with a new
  `ResumeAnswer` field (alongside `Registered`): when set, turn 1's
  directive is the condition plus a formatted "the user answered your
  questions:" block, consumed once and never repeated on later turns. One
  driver (the goal loop), one `Prompt` caller, answer delivered in-band.

For interactive sessions, a client that skips `/answer` entirely still
un-wedges the session correctly, since `Session.Prompt` clears
`s.awaitingQuestion` (and persists `question.answered`) on any new user
message, not only ones routed through `/answer`. A goal-paused session
needs one more guard: while a goal is *running*, `claimForPrompt` already
409s a bare `prompt_async`, but a *paused* goal has returned from
`PursueGoal`, so the run slot is free — an unguarded prompt would consume
the answer without resuming the goal, leaving `goalActive` set with
nothing driving it (a zombie pause). `handlePrompt` therefore 409s when
`s.awaitingQuestion && goalActive`, with an error naming `/answer` as the
resume path — the goal owns the session; `/answer` is the one write that
resumes it. A headless loop is just: `wait?until=awaiting-input` →
`POST /answer` → the goal resumes itself.

## 4. Plugin consumption

A plugin subscribed to `question.asked` sees the event the instant the
tool runs, with everything needed to *render* the question elsewhere and,
symmetrically, to answer it back via `POST /session/{id}/answer` using
client credentials it already has — plugins are trusted local processes
with the orchestrator's own reach (PROTOCOL.md's "Trust model"), so no new
scoping is needed for a plugin to both observe and answer on behalf of a
remote system. Two generic consumer shapes fall out of the payload alone:

- **A notification plugin** turns `question.asked` into an outbound message
  on whatever channel it integrates with, rendering `options`/`multi` as a
  poll or reply buttons where supported, freeform otherwise, and posts the
  human's reply back via `/answer` using `call_id` to correlate.
- **An issue-tracker plugin** files or updates a ticket carrying the
  question text, using its own subsequent lifecycle (resolution, a
  specific comment) as the trigger to call `/answer` — a question
  interrupts a headless run exactly like a ticket transitions the work
  item it mirrors.

Both need nothing beyond the event payload and PROTOCOL.md's existing
client-API reach — no new hook, no new client API method.
`question.asked`'s existing fire-and-forget delivery contract (bounded
per-plugin queue, best-effort, no reordering within one plugin) applies
unchanged: a dropped event is not fatal, since the pending question is
also durably queryable (`GET /session/{id}`, `wait?until=answered`) — the
event is a low-latency nudge, session state is the source of truth.

## 5. Non-goals and migration

- **No permission system.** `ask_user` is model-initiated elicitation, not
  a gate on tool calls — it does not reopen AGENTS.md's "Deliberately
  absent: no permission system, no `permission.ask` hook, no approval UI."
  The model calls it like any other tool; nothing requires asking before
  acting.
- **No answer validation against the schema.** The engine does not enforce
  that a `selected` answer is one of the offered `options`, or that
  `multi: false` got exactly one selection — a consumer/UI concern (§4),
  matching the engine's existing stance on tool-argument validation.
- **No change for hosts that never emit `question.asked`.** The type has
  been reserved and un-emitted since v1; adopting this design starts
  emitting it and adds the tool, wire fields, and `awaiting-input` state.
  A host that does not adopt it changes nothing — no existing field or
  endpoint is repurposed; everything here is additive (new tool, a
  payload for a type that previously had none, a new state value, a new
  endpoint), the bar PROTOCOL.md's Versioning section already sets for a
  minor addition.
- **Deferred:** answer types beyond text/single-select/multi-select (file
  uploads, structured forms). `options`+`multi` covers the elicitation
  shapes seen in practice; a richer payload is an additive schema change
  under the same versioning rule, not a redesign.
