# Goal retry: reuse the unanswered directive, never duplicate it

Status: design, not yet implemented.
Supersedes: the "durable retract record" option rejected in §4.

## 1. The defect

`PursueGoal` retries a failed worker turn by calling `s.Prompt` again with
the SAME directive string. `Prompt` appends whatever text it gets as a new
user message (`engine/engine.go:892`) and persists it immediately
(`appendWithUsage` -> `persistMessage`, `engine/engine.go:773`), BEFORE the
provider call that then fails. `Prompt` has no notion of "this is a retry of
a directive already in history."

N failed attempts therefore write N identical, unanswered directives to the
durable log. Every later request in the session pays input cost for all of
them. The production complaint on box hyper-lemon was "a single wake of the
box added 3 more."

`dropUnansweredDirective` (`engine/goal.go`) removes the copy from LIVE
`s.history` before the next attempt. It cannot touch the log. So a resumed
session (`LoadSession`) replays every duplicate that the live session had
already pruned. The fix closes only the in-process half.

## 2. Invariants

Any implementation MUST hold all of these. Write the test for each one.

1. **No duplicate in the log.** After N failed attempts followed by a
   success, the session log contains EXACTLY ONE directive user message for
   that turn.
2. **No duplicate in live history.** Same count, in `s.history`. Live and
   replayed history must be equal, message-for-message, at every point.
3. **Live equals replay.** `LoadSession` on the log must produce a history
   equal to the live `s.history`. This is the invariant the current fix
   breaks; it is the point of the change.
4. **No delivered content is ever lost.** A denied tool's `ToolResult`, and
   an already-journaled `prompt.dequeued("injected")` "OPERATOR MESSAGES"
   block, must survive every retry path. The block can ride INSIDE the
   directive string (`PursueGoal`'s turn-boundary drain bakes it in before
   `promptTurnWithRetry` sees it), so a directive is not always "just the
   condition."
5. **A parked attempt keeps its tail verbatim.** No next attempt ever comes,
   so nothing re-appends what a park discards.
6. **No journal format change.** No new record type, no new field an older
   binary must understand. See §4.
7. **Nothing is deleted from the log, ever.** The log stays append-only in
   the strict sense: the fix prevents a write, it does not retract one.

## 3. Design

Split `Prompt` into two parts:

- the existing public `Prompt(ctx, text)`, which appends the user message
  and then runs the turn loop, and
- an internal turn-loop entry point that runs against history AS IT STANDS,
  appending no user message.

`promptTurnWithRetry` calls the public `Prompt` on attempt 1. On a retry it
inspects the tail after `anchorID` using the EXISTING
`isSafeToDropDirectiveTail` shape check:

- Tail is exactly the unanswered directive: call the internal entry point.
  The directive already in history is reused. Nothing is appended, nothing
  is deleted, and the log already holds exactly one copy.
- Tail is any other shape: fall back to today's behavior for that attempt.
  Correctness before cleanliness.

`dropUnansweredDirective` is then unnecessary for the bare-directive case
and is removed from it. See §5 for the interrupted-turn case, which is NOT
resolved by this note.

### Why this and not the alternatives

Eliminating the write is strictly safer than compensating for it. There is
no record to retract, so there is no replay-time deletion to get wrong, no
new record type, and no version-skew matrix to reason about.

## 4. Rejected: a durable retract record

The obvious alternative adds a `recRetract` record naming the message IDs
dropped, and has `LoadSession` remove them on replay. Reject it.

`LoadSession`'s record switch (`engine/store.go`, ~line 653) has NO
`default` case. An unknown record type is SILENTLY IGNORED, never rejected.
So in a mixed-version fleet:

1. A new binary writes a retract record.
2. An old binary loads the session, ignores the record, keeps the
   duplicates, then runs `Compact` and journals `firstID`/`lastID`
   computed over a history that still contains them.
3. A new binary loads the session, applies the retract, removes those
   messages, then reaches that compact record — whose boundary ID may now
   be absent. `spliceCompact` returns a hard error and the session never
   loads again.

That is the same permanent-wedge class as NEP-5272 and NEP-5292, reached
through a third door. A cost optimization must not be able to wedge a
session. NEP-5292's `turns_folded` heal would soften step 3, but relying on
one fix to make another one safe is a poor trade when a design with no
format change exists.

## 5. Deliberately out of scope

The interrupted-turn tail — the directive PLUS a partial assistant message
and its synthetic tool-result message (`interruptedTurnError`,
`engine/engine.go`) — is not resolved here. Reusing the directive leaves
those two messages in history, so the retried turn would show the model its
own partial output. That may well be more truthful than hiding it, but it
is a model-visible behavior change and it needs its own evidence. This note
keeps today's live-only drop for that shape and accepts the durable
duplicate it leaves. File it separately.

## 6. Risks

- Splitting `Prompt` touches the single hottest path in the engine. The
  refactor must be pure: the public `Prompt`'s observable behavior —
  events, status emission, usage accounting, auto-compaction placement —
  must not change at all. Prove it with the existing engine suite before
  adding new behavior.
- `maybeAutoCompact` runs inside `Prompt` BEFORE the append. The internal
  entry point must keep that ordering, or a retry could fold the very
  directive it is about to reuse. `anchorID`'s identity lookup already
  tolerates a fold; the reuse path must re-check that the directive is
  still present and fall back to a normal `Prompt` if it is not.
