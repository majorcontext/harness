# Context compaction: summarize-and-truncate

## Motivation

Issue #62 layers 1-2 (implemented alongside this doc) make a context/prompt
overflow fail fast and visible instead of silently stalling. They do not
stop it from happening: a goal re-armed on a long session still marches
straight at the provider's input cap, one turn at a time, until it falls
off. Layer 3 is the primitive that lets a session avoid the cliff instead of
just failing cleanly at it: fold old turns into a synthetic summary message,
shrinking the request without losing the thread. **Design only — no code or
API in this branch.**

## 1. Decisive shape: fold whole turns into one synthetic message

A compaction operation replaces a **prefix** of the live history — from the
start of the currently-replayed history (message 0, or a previous
compaction's own summary message, see §3) through some cutoff — with a
single synthetic message summarizing it. It never touches a suffix and
never touches the middle; the tail (the most recent N turns) always
survives verbatim, because that is the context most likely to still matter
and least safe to compress.

**The cutoff always lands on a turn boundary.** A "turn" here is exactly
what `engine/engine.go`'s `Prompt` produces: one user message, followed by
every assistant/tool message up to and including the turn-ending assistant
message. Turns never interleave and a turn's tool_call/tool_result pairs are
always contiguous, so a cutoff between two turns can never split a
`ToolCall` from its `ToolResult` — this is what makes
`message.ResolveOrphanToolCalls`'s invariant (§4) free, not something
compaction has to re-derive.

The synthetic summary is an ordinary canonical message: `RoleUser`, one
`Text` part, produced by a tool-less model call (mirroring
`GoalOptions.Evaluator`'s pattern exactly — a new `Config.CompactorModel`,
resolved through the same `Config.Providers` registry) over the folded
range, rendered with the same part-flattening `goal.go` already has
(`renderConversation`/`renderPart`, exported for reuse). `RoleUser` is
chosen deliberately over `RoleAssistant`: every transcoder already handles a
leading user message at the start of a request, so the summary can never
violate a provider's alternating-role expectations regardless of where a
future truncation lands it. The rendered text is prefixed with a fixed,
unambiguous marker (`"[Conversation summary — replaces N earlier
messages]\n\n..."`) so the model reads it as compressed background, not as
something the user just said.

## 2. Two triggers, one operation

**Explicit: `POST /session/{id}/compact`.** Body: `{"keep_turns": 4}`
(optional; default from `Config.CompactKeepTurns`, itself defaulting to 4).
Everything older than the last `keep_turns` complete turns is folded.
Requires the same run-slot exclusivity `prompt_async`/`goal` already claim
(`claimForPrompt`) — compaction mutates history exactly like a turn does,
so it cannot run concurrently with one, and reuses that 409 "busy" /
"awaiting-input" contract rather than inventing a new one. `keep_turns` ≥ the
turns actually present is a no-op, 200, `folded_count: 0` — not an error,
same leniency `ClearGoal` gives a repeated clear. Response: the updated
`sessionJSON` (usage/messages reflect the fold immediately; see issue #62
layer 2's `usage` block for why that's the field an orchestrator already
polls).

**Automatic: `Config.AutoCompactThreshold`** (a fraction, e.g. `0.8`; `0`,
the default, disables it — opt-in, never a surprise). Checked at the very
start of `Session.Prompt`, before the new user message is appended: if the
last completed turn's `LastUsage().InputTokens` (issue #62 layer 2) exceeds
`threshold * limit`, where `limit` comes from the embedded models.dev
catalog for the session's current model, compaction runs transparently
(`keep_turns` from config) before the prompt proceeds. Checking at
`Prompt`'s start rather than immediately after the previous turn keeps it
out of the goal loop's between-turn evaluator call — the evaluator's own
request is tiny and tool-less, unrelated to the worker's context size — and
out of `streamTurn`'s hot path. A model absent from the catalog never
triggers auto-compaction (no guessed limit, ever); it remains reachable only
through the explicit endpoint.

Both paths call the same internal operation, so there is exactly one place
that has to get the turn-boundary/journal/replay invariants right.

## 3. Durable journaling: an append, never a rewrite

**The session log stays append-only — always** (AGENTS.md's first
invariant). Compaction never rewrites or deletes an on-disk line; a raw
`.jsonl` file remains the complete forensic record, readable out of band
even after everything in it has been folded away from the engine's own
replayed view. This mirrors the repo's existing stance on not destroying
data to hide it (AGENTS.md's "cleansing marshals hide poison" debugging
invariant is the same instinct in reverse: don't let a projection make you
*think* the underlying record is gone).

Compaction appends exactly two records, in order:

1. An ordinary `message` record for the summary — appended through the
   existing `Session.append` path, so it gets a normal message ID, a normal
   `message` journal/event, and counts like any other message. Nothing
   downstream needs to know it's special.
2. A new `compaction` record: `{"through_id": "<last folded message's
   ID>", "count": <N folded>}`. `through_id` is the *only* field replay
   trusts for correctness; `count` is carried purely for observability
   (`GET /session/{id}` could surface a `compactions` counter the same way
   `Goal`/`LastTurn` are surfaced today) and is never used to decide what to
   drop.

**`LoadSession` replay:** scanning hits `compaction` after the summary's own
`message` record is already in `s.history` (chronological order guarantees
this — the summary is always appended immediately before the marker, and
nothing else can interleave because compaction holds the same run-slot
exclusivity §2 requires live). Replay finds the index of `through_id` in the
in-progress `s.history`, asserts the very next entry is the summary message
just appended (a mismatch is corruption — fail loud, exactly like every
other `LoadSession` invariant violation, never silently guess), and
re-slices: `s.history = s.history[throughIdx+1:]`, i.e. drop everything up
to and including `through_id`, keep the summary onward. This is the same
"replay projects a different in-memory shape than the raw log" trick
`ResolveOrphanToolCalls` already performs at load time — compaction is a
second, independent projection layered on the same replay pass, no new I/O.

**Compounding is free.** Because the fold always operates on the *current*
prefix of `s.history` — which, after an earlier compaction, already starts
with that compaction's own summary message — a later compaction naturally
folds "previous summary + newer turns" into one fresh summary. No
special-casing "is this already-summarized history" is needed; it's turns
either way.

`GET /session/{id}/message` (and any other consumer of `Session.History`)
sees the **projected** view — summary plus surviving tail — identical to
what the model itself is sent. An operator who needs the pre-compaction
transcript reads the raw `.jsonl` directly; the HTTP surface intentionally
shows what the engine is actually working from, not an archive.

## 4. Invariant preservation, explicitly

- **`LoadSession` replay** — §3 above: one assert-checked splice, no
  mutation of the on-disk log, corruption (a `through_id` that isn't
  immediately followed by its summary) fails loud rather than silently
  producing a wrong-shaped history.
- **`message.ResolveOrphanToolCalls`** — never at risk, because a fold
  boundary is always a turn boundary (§1): a turn's `ToolCall`/`ToolResult`
  pair is contiguous and entirely inside or entirely outside the fold, and
  the summary message itself carries no `ToolCall` parts, so it can never
  itself be a source of an orphan. `ResolveOrphanToolCalls` runs on the
  *projected* history exactly as it does today; it never sees the raw log.
- **`question.asked`/`question.answered`** — compaction requires the same
  "not busy, not awaiting-input" claim §2 already needs, so a *pending*,
  unanswered question can never end up mid-fold or split across the
  boundary — the operation simply isn't reachable while one is open. An
  already-answered question/answer pair fully inside a folded range is safe
  to lose exactly like any other folded turn: nothing outside the message
  history references those records by ID (`Session.pendingResumeAnswer`
  carries formatted text, not a pointer into history), so folding them away
  changes nothing observable.
- **Goal state** — `Session.goalActive`/`goalCondition` (`engine/goal.go`)
  live outside message history entirely and are untouched by a fold; a goal
  re-armed on a freshly-compacted session resumes exactly as it does today.
- **Issue #62 layers 1-2 still apply.** Compaction reduces the *chance* of
  hitting the provider's cap; it is not a guarantee (a single turn can still
  overshoot a freshly-compacted history, and auto-compact is opt-in). When
  the cap is hit anyway, classification and fail-fast behavior are
  unchanged — compaction and classification are complementary, not
  alternatives.

## 5. What this doc deliberately leaves undecided

- The exact summarization prompt/system message for `CompactorModel`.
- Whether `keep_turns` should instead be a token budget (harder to make
  deterministic; turns are simpler to reason about and to test).
- Surfacing a `compactions` counter or history on `GET /session/{id}`
  (straightforward addition once implemented, not required for the
  primitive itself).
