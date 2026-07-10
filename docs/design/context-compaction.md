# Context compaction: summarize-and-truncate (issue #62, layer 3)

## Motivation

Layers 1 and 2 (see `feat/context-observability`) give an operator a
classified `provider.ErrKindContextOverflow` failure and a running
`Usage`/`LastUsage`/`last_activity_at` picture of a session's size, but
neither one relieves the pressure — the only remedy today is to abandon the
session. This proposes the primitive that acts on that signal: fold a
contiguous prefix of old turns into one synthetic summary message, durably,
in place, without disturbing the session's identity or goal state. Design
only; no code in this branch.

## 1. Trigger: automatic threshold, explicit endpoint as the escape hatch

**Automatic (primary).** `Session.Prompt` already snapshots the session's
full history into every request; layer 2's `LastUsage()` reports the input
token count of the *last* such request — the best available proxy for "how
big would the next one be," since history only grows. Recommend: a check at
the very top of `Prompt`, after `ensureInstructions`/`ensureSkills` and
before the turn loop, run on *every* call (bare `prompt_async` and every
goal-loop worker turn alike, since `PursueGoal` drives everything through
`Prompt`) — no separate scheduler, no goal.go changes needed. If
`Config.ContextWindowTokens > 0` (opt-in; a fresh `Config` leaves it zero,
disabling this entirely — the engine has no built-in per-model table) and
`haveLastUsage && lastUsage.InputTokens >= threshold * ContextWindowTokens`
(threshold from `Config.CompactionThreshold`, defaulting to 0.8 when zero,
mirroring `newSession`'s existing zero-fills-a-default pattern for
`BashTimeout`), compact before streaming the turn. The check runs BEFORE
the incoming user message is appended (i.e. between the ensure* calls and
`s.append` in `Prompt`): boundaries then always fall on completed-turn
edges, the summary never has to account for a prompt that hasn't been
answered yet, and the just-arrived message can never be folded into its
own summary. `Usage()` (cumulative)
is deliberately NOT the signal — it sums every turn ever run, which is not
"how large is the next request."

**Explicit: `POST /session/{id}/compact`.** Always available regardless of
threshold — pre-emptive compaction ahead of a known-large tool result,
operator-triggered cleanup, or a caller that disables the automatic path
entirely and drives it manually. Optional JSON body `{"keep_turns": N,
"model": {...}}` overrides `Config.CompactionKeepTurns`/
`Config.CompactionModel` for this call only; `keep_turns` has a hard floor
of 1 (a 400 on 0 or negative) — the most recent turn is never foldable,
so `s.history` can never collapse to a lone summary the model would have
to answer with zero real context. Response: `{"turns_folded": N,
"first_id", "last_id", "summary": {...}}`; `turns_folded: 0` (200, not an
error) when there is nothing worth folding — see §2's minimum-fold rule.

Both paths funnel through one `Session.Compact(ctx, CompactOptions)` method;
the automatic path just calls it with defaults before `streamTurn`, and it
takes the same run-slot discipline described in §4.

## 2. Mechanism

**Range selection.** Compaction always folds a **contiguous prefix of whole
turns**, never a partial one. A turn starts at a `RoleUser` message and runs
to (not including) the next `RoleUser` message or end of history — because
every turn's tool exchanges are fully resolved, real or synthetic, before
the next user message is ever appended (`interruptedToolResults` in
`engine/engine.go`, `message.ResolveOrphanToolCalls` as defense-in-depth on
load), a `RoleUser` message boundary is *by construction* a point where no
`ToolCall` is ever waiting on its `ToolResult`. Requiring **both** ends of
the folded range to land on such a boundary — `FirstID` names the first
folded turn's leading `RoleUser` message, `LastID` names the last message
before the first *kept* turn's `RoleUser` message — makes "never orphan a
tool_use across the boundary" and "never split an assistant message from
its required results" structural guarantees, not something the compaction
code has to reason about per call. `goal.*` records are untouched on
principle: they live in `Session` fields (`goalActive`/`goalCondition`),
never in `s.history`, so a range that folds the messages explaining a goal
leaves the goal state itself exactly as it was.

Recommend keeping the most recent `Config.CompactionKeepTurns` turns (2, if
unset) verbatim always; if fewer than that many complete turns exist yet,
compaction is a no-op this cycle (nothing gained, tried again as history
grows).

**Churn guard (hysteresis).** When the pressure lives in the KEPT region —
a single giant tool result in one of the last turns — folding the prefix
cannot relieve it: the next check would still be over threshold, re-fold an
ever-shrinking prefix, and burn one summarization round-trip per turn until
layer 1's overflow classification finally fires. The automatic trigger
therefore carries a one-flag hysteresis: after an automatic compaction, it
does not fire again until `LastUsage().InputTokens` has dipped below the
threshold at least once since (the flag lives beside the session's other
in-memory trigger state; it deliberately does NOT persist — a reload
re-evaluates from scratch, and the worst post-reload cost is one extra
summarization attempt). The explicit `/compact` endpoint ignores the flag:
an operator override is exactly the case where re-folding on demand is
wanted. A summary message produced by an earlier compaction is an ordinary
`RoleUser` message like any other — it can itself be folded into a *later*
compaction's range with no special case; a "summary of a summary" is just
another old turn.

**Who writes the summary.** One tool-less model call, mirroring the
evaluator shape `engine/goal.go` already establishes (`runEvaluator`): a
request built from exactly the folded range's messages (independently
transcodable, since a whole-turns range has no dangling tool call at either
edge) plus a dedicated compaction system prompt asking for a concise,
information-preserving summary — user intent, decisions and rationale,
concrete facts a later turn depends on (file paths, commands, values, error
text), explicitly not tool-call minutiae verbatim. Model: `CompactOptions.
Model`, defaulting to the session's *own* current model when unset — unlike
`GoalOptions.Evaluator` (which must be a genuinely independent judge),
summarization needs competence, not independence, so defaulting removes a
config burden from the automatic trigger's every-turn check. The resulting
summary becomes a fresh `RoleUser` message (new `ID`, `CreatedAt` = compaction
time), its text prefixed with a synthesized-and-visibly-marked banner —
same spirit as `message.SyntheticOrphanResultText` — so a transcript or
`GET /session/{id}/message` reader can never mistake it for something the
human actually typed. Note the spliced history then opens with two
adjacent `RoleUser` messages (summary, then the first kept turn's user
prompt) — a shape ordinary operation never produces. This is load-bearing
on existing transcoder behavior, not luck: the Anthropic adapter merges
adjacent same-role messages (transcode.go's alternation handling, already
tested), and the OpenAI-compat wire accepts consecutive same-role items
natively. An implementer changing the summary's role or the transcoders'
same-role handling must re-check this pairing.

**Usage accounting.** The summarization round-trip is real spend, so its
tokens are added to the cumulative `Usage()` like any other provider call —
but it must NOT overwrite `LastUsage()`: the automatic trigger reads
`LastUsage()` as "how large is the next worker request," and a small
summarization call would mask the very pressure that triggered compaction
(and re-trigger logic would misread the session as small). `LastUsage()`
updates only on worker-turn requests. Durability: cumulative `Usage()`
survives a restart only because `LoadSession` re-sums each `recMessage`'s
`rec.Usage` — and the summary is deliberately NOT a `recMessage` — so the
`compact` record carries its own `usage` field, and `LoadSession`'s replay
adds it into the cumulative sum ONLY — unlike `recMessage` replay, which
also sets `s.lastUsage`/`s.haveLastUsage`, the compact record's usage must
never touch those, or reload would violate the live rule above (a reloaded
session would report the small summarization call as its "last request
size" and defeat the re-trigger check). Without the field at all, the
summarization spend would silently vanish from `Usage()` on every reload.

**Failure handling.** If the summarization call errors (rate limit,
transient 5xx, or the range itself is too large to summarize in one call —
a real possibility for one giant tool result), compaction aborts cleanly:
no journal write (§3 below never happens without a summary in hand first),
no history mutation, an emitted `compaction.failed` event/`OnEvent`, and —
for the automatic trigger — the turn simply proceeds uncompacted, at the
same risk layer 1 already classifies and fails fast on if it actually
overflows. Compaction is a best-effort relief valve, not a load-bearing
correctness mechanism; failing loud into an existing, already-handled
failure mode is strictly better than blocking the caller's real turn on it.

**Journal shape.** One new record type, alongside `goal.*`:

```json
{
  "type": "compact",
  "created_at": "...",
  "usage": { "input_tokens": 8214, "output_tokens": 512 },
  "compact": {
    "first_id": "msg_...",
    "last_id": "msg_...",
    "turns_folded": 12,
    "summary": { "id": "msg_...", "role": "user", "parts": [...], "created_at": "..." }
  }
}
```

The top-level `usage` reuses the existing `record.Usage` field
(`store.go`), exactly as `recMessage` carries it, and holds the
summarization call's spend (see "Usage accounting" above). `turns_folded`
is the one field name, used identically here, in the `/compact` response,
and in the `history.compacted` event.

`compactRecord.Summary` is the full `message.Message` to splice in, carried
*inline* — not a lightweight marker record followed by an ordinary
`message` record for the summary. That two-record shape was considered and
rejected: it reopens exactly the crash window this design otherwise avoids
for free (see §3). One record, one `json.Marshal`, one `Write` call — the
same discipline every other record already follows.

**`LoadSession` replay.** `scanLog`'s switch (`store.go`) gains one case:
on `recCompact`, find `FirstID`/`LastID` within `s.history` accumulated so
far (guaranteed present, in order, since a `compact` record can only be
written chronologically after those messages were themselves durably
appended) and splice — `s.history = append(s.history[:start],
append([]message.Message{summary}, s.history[end+1:]...)...)`. Not found is
treated as corruption (an explicit error, matching scanLog's "corruption
anywhere else is an error" rule), never a silent best-effort guess. The
existing post-loop `message.ResolveOrphanToolCalls(s.history)` call still
runs unchanged afterward — it is a no-op across a compaction boundary by
construction (§2), and still protects any orphan elsewhere in the surviving
history exactly as before. A live, resident session performing compaction
(§4) runs the identical splice function directly on `s.history`, so the two
paths — reload and live — can never drift apart.

## 3. Crash discipline

The write is atomic-per-line like every other record: `compact` is
`json.Marshal`ed and appended in one `Write`, only *after* the summarization
call already succeeded. A crash before that write lands leaves the original
messages exactly as they were (nothing was ever deleted — compaction never
rewrites or removes existing log lines, only appends one new record whose
absence is indistinguishable from "compaction never started"). A crash
*during* the write leaves a truncated final line, which `scanLog`'s
existing rule already exists to handle: a corrupt or incomplete line is
tolerated silently only when it is the log's last line (crash mid-write),
which it always is here — a session has exactly one writer goroutine at a
time, serialized on `s.mu`, so any crash mid-append always lands on the
current last line. No new corruption-handling code is needed in `scanLog`
at all; a torn `compact` write degrades to "compaction never happened,"
never to a partially-spliced or ambiguous history.

## 4. Interaction with the resident session

Compaction runs **only when the session is not mid-turn**, and it enforces
that the same way an ordinary prompt does: by claiming the run slot.
- The automatic trigger runs *inside* an already-claimed turn (right after
  `claimForPrompt`, before `streamTurn` ever calls the provider) — it is
  never a concurrent operation, just an extra step folded into a turn that
  already owns the slot.
- The explicit endpoint claims the slot itself, exactly like
  `prompt_async`'s claim path: `409` if the session is
  already running, same as any other write. No new slot type, no
  compaction-specific concurrency to reason about.

Once the summary is in hand (the slow, network-bound part, done **without**
holding `s.mu` — same pattern `streamTurn` already uses via `s.History()`),
`Compact` re-acquires `s.mu` once to splice `s.history` and persist the
`compact` record in the same critical section `append` already uses
(mutate, then persist, one lock hold) — a reader calling `History()`,
`Usage()`, or `LastUsage()` concurrently either sees the pre- or
post-compaction state, never a half-spliced one. Because only the slot's
single claimant can ever call `Compact`, there is no second writer to race
against within that section either.

### Live event surface

Anything tailing the event stream (`GET /event`, SSE) must see the
compaction, not just readers of durable state: a successful compaction
emits TWO things, in order. First the summary itself flows through the
ordinary message-event path (`EventMessage` → server journal, the same
route every other message takes), so an `events.jsonl` tailer receives the
summary CONTENT — the durable `compact` record carries the summary inline
rather than as a `recMessage`, so without this emission a tailer would
hold a dangling id for a message it never received. Then a
`history.compacted` engine event (journaled via the server's `emitDurable`
path like `session.status`) carrying `{first_id, last_id, turns_folded,
summary_id}`, where `summary_id` refers to the message the tailer just
saw. A tailer replaying from a `from` cursor older than the compaction
sees the original messages, the summary message, and the compaction event
— the event is the reconciliation signal telling it which prefix the
summary replaced. The `compaction.failed` event (above) is its
fire-and-forget counterpart.

## 5. Non-goals

- **No local tokenizer.** Compaction relies entirely on the provider's own
  reported `Usage`, never a bundled token-counting library — same stance
  the engine already takes toward token accounting.
- **No selective/sparse folding.** Always a contiguous oldest-first prefix,
  never an importance-ranked or sparse subset of turns.
- **No cross-session compaction**, no compaction of `goal.*` records (they
  are never part of `s.history` and are never touched).
- **No answer/content validation of the summary** against the original —
  matches the engine's existing non-stance on validating model output.
- **Additive, matching the bar `PROTOCOL.md`'s Versioning section sets for
  the plugin wire protocol.** `scanLog`'s replay switch has no `default`
  case today, so a binary built before this design silently ignores an
  unrecognized `compact` record and simply replays every underlying message
  verbatim — a safe, harmless degraded read (more tokens, identical
  content), never a corrupt one, since compaction never deletes a log line.
  `Config.ContextWindowTokens` defaulting to zero (disabled) means no
  existing deployment changes behavior by upgrading; the new endpoint,
  record type, and config fields are all purely additive.
