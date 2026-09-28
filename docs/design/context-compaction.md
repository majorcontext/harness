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
`Config.ContextWindowTokens > 0` and
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

**Where `ContextWindowTokens` comes from (added to close a gap).** This
design originally left `ContextWindowTokens` fully
opt-in — "the engine has no built-in per-model table" — on the theory that
whatever embeds the engine would set it. In production that theory failed
silently: the boxes platform set it nowhere, so every box ran with
automatic compaction permanently disarmed, and a session eventually died
with a raw `context exhausted` provider error instead of ever compacting.
`newSession` (`engine/context_window.go`, `resolveContextWindow`) now
derives it itself when the embedder leaves it zero: package `modelmeta`
holds a curated table of `provider/model` -> context-window tokens, sourced
from models.dev's `limit.context` field (bifrost's own `/v1/models` was
investigated and ruled out — it returns the bare OpenAI listing shape with
no context-length field at all). Precedence is explicit config >
model-derived > disabled, and a model-derived value below
`minAutoContextWindowTokens` (16k) leaves compaction disabled rather than
arming a nonsense threshold — logged at INFO, not WARN, because the table
legitimately keeps some genuinely small windows (gpt-4's true 8,192), so a
below-floor value is a known-good-but-small model, not corrupt metadata. `SetModel` re-runs the same
derivation against the new model unless the session's window was pinned by
explicit config, so a mid-session model switch keeps the window (and
therefore whether compaction is armed at all) matched to whichever model is
actually running. One INFO log line at session start (and again on any
switch that changes the effective window) names the resolved window and its
source (`config`/`model-derived`/`disabled`) — the operator signal that
would have made a disarmed compaction visible well before the
box died.

**Explicit: `POST /session/{id}/compact`.** Always available regardless of
threshold — pre-emptive compaction ahead of a known-large tool result,
operator-triggered cleanup, or a caller that disables the automatic path
entirely and drives it manually. Optional JSON body `{"keep_turns": N,
"model": {...}}` overrides `Config.CompactionKeepTurns`/
`Config.CompactionModel` for this call only; `keep_turns` has a hard floor
of 1 (a 400 on 0 or negative) — the most recent turn is never foldable,
so `s.history` can never collapse to a lone summary the model would have
to answer with zero real context. Response: `{"turns_folded": N,
"first_id", "last_id", "summary": {...}, "skip_reason": "..."}`;
`turns_folded: 0` (200, not an error) when there is nothing worth folding —
see §2's minimum-fold rule. `skip_reason` is present (never on a real fold)
and names exactly why: `not_enough_turns` and `lone_existing_summary` are
free — the provider is never called — while `summarizer_empty` means the
provider WAS called and billed but returned nothing usable (see
`engine.CompactResult.SkipReason` and its `SkipReason*` constants; review
follow-up on PR #136, Finding C — a single `turns_folded: 0` used to
collapse all three distinct situations into one indistinguishable shape).

The automatic path calls `Session.Compact(ctx, CompactOptions)` directly,
with defaults, before `streamTurn`. The explicit path funnels through
`Session.RunCompactCommand` instead, which calls `Compact` on a native
session — see below for the delegated lane, where it does not. Either way
it takes the same run-slot discipline described in §4.

**A session delegated to the Claude Code CLI, and a switch away from it.**
`PromptWithOrigin` skips this whole section — `ensureInstructions`,
`ensureSkills`, and `maybeAutoCompact` alike — for any turn the session's
CURRENT model routes to the Claude Code CLI backend
(`engine.ClaudeCodeProviderFamily`, `engine/claude_code_backend.go`): that
turn manages its own context end to end, and harness's own journal is only
ever a passive record of what streamed back. `applyClaudeCodeUsage` still
sets `Session.LastUsage()` on every delegated turn, from the usage of the
turn's last main-thread API call (the `message.usage` of its last
"assistant" envelope; the "result" event's usage is the sum over all calls
and goes into `Session.Usage()` only) — but that figure describes the CLI's
OWN internal, self-compacted context, not harness's journal. The two can differ by orders of
magnitude on a long-running delegated session, because harness's journal
is never itself compacted while delegated.

This matters the moment `SetModel` switches such a session to a
harness-native model: the very next `Prompt` now takes the native path,
whose request transcodes harness's REAL journal — not whatever the CLI last
reported. Trusting the stale, wrong-scale `LastUsage()` figure here would
compare the wrong number against the new model's window and skip
compaction, forwarding a potentially huge, never-once-compacted journal to
a provider that rejects it outright ("prompt too long") — as seen on a
3,667-message, 5-day delegated run.

`SetModel` therefore arms a `forceCompactionCheck` flag exactly when the
PRIOR model was claude-code-delegated and the new one is not (never on a
native-to-native switch, where `LastUsage()` stays a valid harness-journal
signal regardless of which native model produced it, and it CLEARS on a
switch back INTO delegation, which disarms harness's own trigger entirely
per the paragraph above). The flag is durable, not memory-only: it is true
exactly when the session's CURRENT model is native and the most recently
recorded usage-defining event was a delegated turn. `store.go`'s replay
fold reconstructs it from the same `recModel`/`recMessage` records that
already carry the provider switch and the next real native usage — a
`recModel` record moving the provider off delegation arms it, a later
`recMessage` record carrying real native `Usage` disarms it — and
`snapshot.go` captures/restores it for the anchored-load fast path. This
matters because the stale signal the flag exists to distrust is itself
durable (`recClaudeCodeUsage`): a residency eviction or a process restart
between the `SetModel` switch and the next `Prompt` does not lose the
guard along with the live `*Session`. It also means an on-disk **snapshot**
(§4.5) cannot be allowed to silently claim the flag was false: a snapshot
anchored just past the `recModel` switch record — the normal shape after a
delegated run, a switch, and one on-idle checkpoint — would otherwise
disarm the whole mechanism permanently for exactly the sessions the
incident hit, since a snapshot never replays the record that would re-arm
it. Adding `forceCompactionCheck` to the snapshot schema therefore also
bumped `sessionSnapshotVersion`: a snapshot written by a binary that
predates the field cannot know it exists, so `readSessionSnapshot` must
discard it (any version mismatch means a full replay, see §4.5) rather
than decode a missing key as the field's zero value.

The next `maybeAutoCompact` call that sees the flag armed reads it WITHOUT
clearing it yet. Instead of reading `LastUsage()`, it estimates the prompt
size straight from `s.History()` (the same crude byte-count fallback §1's
zero-usage fallback already uses for "provider reports nothing usable"),
folding in the byte length of `Session.lastSystem` — the system segments
assembled for the most recent model call in this process, if any — so the
estimate accounts for the system prompt and skills/MCP catalog a native
request also carries, not history bytes alone (zero on a session's first
native call after a delegated run, since no such call has happened in this
process yet — a bound this design states openly, not one the estimate
hides; tool schema bytes are never folded in at all, so the estimate can
still run under the real request size by a margin this paragraph does not
bound further). An image `Blob` part is estimated at a flat ~1,600 tokens
each, matching Anthropic's own per-image ceiling after it resizes and
tiles an image for tokenization, regardless of the blob's encoded byte
size — counting a base64 payload's bytes at the same ~4-bytes-per-token
rate as text overstated a real image by close to an order of magnitude (a
1.5 MB screenshot reads as ~500k tokens under the byte rule) and could make
a session carrying one screenshot in its kept-turns tail appear permanently
over any native model's window. The check bypasses the churn-guard
cooldown (a regime change the guard's own latched state says nothing
about) and — unlike the ordinary automatic trigger, which is best-effort
and never blocks the caller's real turn — settles every `Compact` outcome,
including a real error, before returning (see below) rather than silently
falling back to the stale signal.

Every way a forced `Compact` call can end without folding enough to clear
the estimate — `SkipReasonNotEnoughTurns`, `SkipReasonLoneExistingSummary`,
`SkipReasonSummarizerEmpty` (a billed call that returned nothing usable), a
real fold whose re-estimate is still over the window, or the `Compact` call
itself erroring (a transport or rate-limit failure, or a deterministic one
such as the fold range itself overflowing the summarizer model's own
window) — is a conclusive outcome, handled the same way: it is reported
loudly (`compaction.failed`, naming the reason) but does **not** fail the
`Prompt` call. The flag is cleared and the request proceeds to the native
provider for its own real verdict instead. Failing the caller's every
future `Prompt` call forever, decided by a crude estimate (or a single
provider error) with no in-band recovery, traded one silent failure mode
(an opaque provider rejection) for a worse one (a permanently
un-promptable session); this way the caller still gets a diagnosable
reason on the attempt that discovered folding could not help, and every
attempt after that reaches the provider exactly as if the flag had never
armed. There is deliberately no further retry armed after this: an earlier
design re-armed a one-shot check the next time the journal grew past the
point a pass gave up at, but `maybeAutoCompact` runs before the incoming
user message is appended, so a terminated forced pass that lets the turn
through grows the journal by construction on every later `Prompt` call —
that mechanism could not distinguish "growth because a retry is due" from
"growth because the caller sent another prompt," and measurement showed it
reissuing the summarizer once per `Prompt` call indefinitely while pressure
persisted, exactly the per-turn billed-call shape this design otherwise
guards against. The mechanism instead stays off until either a native turn
lands real usage (`appendWithUsage` already clears the flag the moment
that happens — the ordinary trigger is trustworthy again from there) or the
model is switched again (`SetModel` re-arms it exactly as it did the first
time).

The prompt text is never recorded when this check fails: the check runs
before the incoming user message is appended (like `ensureInstructions`/
`ensureSkills` immediately above it), so a failed forced pass costs the
caller nothing but the round trip — the original text is still theirs to
resubmit, exactly as if the call had never been made. This applies equally
to the real-error case above (`Prompt` fails outright) and the conclusive
case (`Prompt` proceeds without ever having recorded the pre-compaction
attempt's own text).

**A typed `/compact` command** is not model input: `harness serve`
resolves it before it reaches the engine as a prompt
(`docs/design/slash-commands.md`) and dispatches it as
`POST /session/{id}/compact`. The engine learns no control verb from
prompt text: an untyped `/compact` prompt to a NATIVE session is
ordinary model input, and on a claude-code-delegated session the same
untyped text reaches the CLI unchanged and the CLI runs it as its own
command — the same `//x` exception `docs/design/slash-commands.md` §5
describes, since the delegated lane never distinguishes typed from
untyped text — keeping the caller's own origin, not `OriginEngine`.
`POST /session/{id}/compact` and the `run`-mode dispatcher both funnel
through `Session.RunCompactCommand`: on a native session it calls
`Session.Compact`; on a CURRENTLY-delegated session, which
`Session.Compact` itself still refuses (no journal to fold), it instead
issues the Claude Code CLI's own `compact` command — the CLI's published
control-request surface
(`@anthropic-ai/claude-agent-sdk`'s `sdk.d.ts`) has no separate compaction
trigger, so sending its command is the only mechanism. The command is
dispatched with `message.OriginEngine`, never the caller's own text or
provenance: harness decides to compact, the caller's literal prompt is
never what reaches the CLI. A delegated compaction still fires
`EventCompactionStarted` (the CLI's own "compacting" status), settling in
`EventClaudeCodeCompacted`, never `EventHistoryCompacted` — no harness
journal splice happens there.

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
wanted.

The same latch also fires on `SkipReasonSummarizerEmpty` (see "Failure
handling" below), not just on a real fold: that skip still cost a full
provider call, and pressure in the KEPT region is not the only way an
over-threshold turn can keep re-triggering an expensive no-op — a
summarizer that keeps returning empty text is the other. It deliberately
does NOT latch on `SkipReasonNotEnoughTurns`/`SkipReasonLoneExistingSummary`
(the two free skip reasons, never billed): latching there would permanently
disarm automatic compaction for an over-threshold session that simply
doesn't have enough turns yet to fold, since the guard only clears once
`LastUsage()` dips back under threshold (review follow-up on PR #136,
Finding A).

A summary message produced by an earlier compaction is an ordinary
`RoleUser` message like any other — it can itself be folded into a *later*
compaction's range with no special case; a "summary of a summary" is just
another old turn.

**Who writes the summary.** One tool-less model call, mirroring the
evaluator shape `engine/goal.go` already establishes (`runEvaluator`): a
request built from exactly the folded range's messages (independently
transcodable, since a whole-turns range has no dangling tool call at either
edge), plus one trailing `RoleUser` instruction message appended
unconditionally after that range — never sent bare — so the request always
ends in a user turn regardless of the folded range's own final message
(ordinarily `RoleAssistant`, the folded turn's final reply: sending the
range bare, as an earlier version of this call did, ends the wire request
in an assistant-role message, which the Anthropic Messages API treats as
assistant message prefill — some models reject prefill outright with a 400
`invalid_request_error`), plus a dedicated
compaction system prompt asking for a concise, information-preserving
summary — user intent, decisions and rationale, concrete facts a later turn
depends on (file paths, commands, values, error text), explicitly not
tool-call minutiae verbatim. Model: `CompactOptions.
Model`, defaulting to the session's *own* current model when unset — unlike
`GoalOptions.Evaluator` (which must be a genuinely independent judge),
summarization needs competence, not independence, so defaulting removes a
config burden from the automatic trigger's every-turn check. The resulting
summary becomes a fresh `RoleUser` message, its `ID` minted with the
`cmpsum_` prefix (`compactionSummaryIDTag`, `newID`) rather than the
ordinary `msg_` prefix every other message gets — a STRUCTURAL,
unforgeable marker of compaction origin that `isLoneExistingSummary` (§2's
"Failure handling") gates on, distinct from the banner text below, which is
display-only (review follow-up on PR #136, Finding B). `CreatedAt` =
compaction time; its text is prefixed with a synthesized-and-visibly-marked
banner — same spirit as `message.SyntheticOrphanResultText` — so a
transcript or
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

**Failure handling.** If the summarization call errors for a REAL reason
(rate limit, transient 5xx, a truncated stream, or the range itself is too
large to summarize in one call — a real possibility for one giant tool
result), compaction aborts cleanly: no journal write (§3 below never
happens without a summary in hand first), no history mutation, an emitted
`compaction.failed` event/`OnEvent`, AND the error propagates to the
caller — for the automatic trigger, the turn simply proceeds uncompacted,
at the same risk layer 1 already classifies and fails fast on if it
actually overflows; for the explicit endpoint, the caller sees the failure.
Compaction is a best-effort relief valve, not a load-bearing correctness
mechanism; failing loud into an existing, already-handled failure mode is
strictly better than blocking the caller's real turn on it.

A call that completes without a transport/stream error but returns no
usable text — an empty summary — is a DIFFERENT case, not a failure of this
kind: the model was asked, answered, and had nothing to add. Treating it as
the same hard-error shape as the above (`{"error":
"engine: compaction summary was empty"}` surfaced from an operator's
otherwise-ordinary `POST /session/{id}/compact` call) puts an operator
manually folding a large session in the position of treating "the model
said nothing" as fatal. Compaction instead reports it as the same
`turns_folded: 0` "nothing worth folding" shape §2's minimum-fold rule
already uses above — no journal write, no history mutation, no error to
the caller — while still emitting `compaction.failed`, so the attempt
remains visible to anything tailing events even though it is not surfaced
as a caller-visible error. This is `SkipReasonSummarizerEmpty`; it is the
one skip reason that DID cost a billed call, so its usage is still
accumulated into cumulative `Usage()` even though nothing else about the
attempt is durable (no journal write, no history mutation) — see "Usage
accounting" above and `engine.CompactResult.SkipReason`'s doc comment
(review follow-up on PR #136, Finding A). The one cheap, structural case
worth catching BEFORE ever calling the provider (`SkipReasonLoneExisting
Summary`): a fold range whose entire content is a single earlier
compaction's own summary message (no other turn alongside it) has nothing
left to reduce — re-summarizing an already-compressed summary is exactly
the shape that produced the empty-summary incident (a small `keep_turns`
landing a fold range dominated by a prior summary) — so that range is
skipped the same way, without a provider call at all. This is detected
structurally, by the summary message's `cmpsum_`-prefixed `ID` (see "Who
writes the summary" above), never by matching `CompactionSummaryBanner`'s
display text — a user-typed or pasted message that happens to start with
the exact banner string is a genuine turn with real content to fold, not a
lone existing summary, and matching on text alone treated it as one:
skipped forever, without ever calling the provider, which under the
automatic trigger meant the session never compacted again (review
follow-up on PR #136, Finding B).

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
    "summary": { "id": "cmpsum_...", "role": "user", "parts": [...], "created_at": "..." },
    "started_at": "...",
    "folded_tokens_est": 3200
  }
}
```

The top-level `usage` reuses the existing `record.Usage` field
(`store.go`), exactly as `recMessage` carries it, and holds the
summarization call's spend (see "Usage accounting" above). `turns_folded`
is the one field name, used identically here, in the `/compact` response,
and in the `history.compacted` event.

**Observability: duration and size.** Before `started_at`/
`folded_tokens_est` existed, nothing durable answered how long a
compaction took or whether duration scales with what it folded — only the
turn count and the record's own `created_at` (when the summarization call
*returned*). `started_at` is the wall-clock instant `Compact` commits to
attempting a summary (the same instant it emits `compaction.started`),
captured immediately before the blocking call; a reader derives elapsed
duration as `created_at - started_at` rather than a redundant stored
field. `folded_tokens_est` is `estimatePromptTokensFromHistory` applied to
only the folded range, the same crude heuristic `maybeAutoCompact` already
uses elsewhere for its own threshold check — cheap here because its cost
is bounded by fold size, not session history size, and it is the one
signal that correlates duration with scale: `turns_folded` alone cannot,
since a two-turn fold can carry one giant tool result or almost nothing.
Both fields are omitted from a record written by a build that predates
them, which a reader must treat as "unknown", never as zero.

A skipped or failed compaction (`not_enough_turns`, `lone_existing_summary`,
`summarizer_empty`, or a real summarization error) never journals a
`compact` record at all — unchanged by this addition. The two free skips
can recur every over-threshold turn until enough history accumulates,
so journaling them would be noise on every turn, not a rare event worth a
durable line; `summarizer_empty` is rarer and billed, but extending
journaling to it alone would break the existing symmetry that no
`TurnsFolded == 0` outcome writes a record. `compaction.failed` already
carries this visibility live, for anything tailing events.

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
compaction, not just readers of durable state. A live client also wants an
IN-PROGRESS signal, not just a settled one: `compaction.started` fires
exactly once, immediately before the blocking summarization call begins —
after `Compact` has committed to attempting a summary (past every
early-return skip and every journal-boundary error), but before the
summary exists. It carries `{first_id, last_id, turns_folded}` — the same
fold-range fields the eventual settlement carries, computed from the same
fold bounds, so a client can correlate "compacting N turns now" with that
settlement — but no `summary_id`, which does not exist yet. Live only,
like `compaction.failed` below: never journaled, since a `started` that
never resolves has nothing durable to reconcile against on replay. It is
always followed by exactly one of `history.compacted` or
`compaction.failed`, never left orphaned. A claude-code-delegated turn
also emits `compaction.started` on the CLI's own "compacting" status, and
settles it with `compaction.claude_code` (success) or `compaction.failed`
(failure or an unsettled stream).

A successful compaction then emits TWO more things, in order. First the
summary itself flows through the ordinary message-event path
(`EventMessage` → server journal, the same route every other message
takes), so an `events.jsonl` tailer receives the summary CONTENT — the
durable `compact` record carries the summary inline rather than as a
`recMessage`, so without this emission a tailer would hold a dangling id
for a message it never received. Then a `history.compacted` engine event
(journaled via the server's `emitDurable` path like `session.status`)
carrying `{first_id, last_id, turns_folded, summary_id,
compact_started_at, context_used_tokens, context_window_tokens}`, where
`summary_id` refers to the message the tailer just saw and
`compact_started_at` is when the blocking summarization call began — a
consumer derives duration as this durable record's own `recorded_at`
minus `compact_started_at`, never a client-observed delivery time, since
replay and SSE reconnect can deliver the record long after `recorded_at`
was assigned. `context_used_tokens`/`context_window_tokens` mirror
`Session.context` at the fold instant (see "The context-window gauge"
below), so a live SSE consumer's gauge updates the moment compaction
settles rather than waiting for that turn's own turn.end. A tailer
replaying
from a `from` cursor older than the compaction sees the original messages,
the summary message, and the compaction event — the event is the
reconciliation signal telling it which prefix the summary replaced. The
`compaction.failed` event (above) is its fire-and-forget counterpart.

**The Claude Code CLI's own compaction, forwarded for observability.** A
claude-code-delegated turn's `--output-format stream-json` protocol emits a
`system` envelope with subtype `compact_boundary` (and a `compact_metadata`
payload: `trigger`, `pre_tokens`, `post_tokens`) the moment the CLI compacts
its own internal context — verified against the published
`@anthropic-ai/claude-agent-sdk` npm package's `sdk.d.ts`
(`SDKCompactBoundaryMessage`) and the CLI's documented streaming-output
page. `consumeClaudeCodeStream`'s `"system"` case
(`engine/claude_code_backend.go`) forwards this as `EventClaudeCodeCompacted`
(`"compaction.claude_code"`). It carries none of `history.compacted`'s
journal-splice fields (`first_id`/`last_id`/`summary_id`): the CLI
compacted its OWN history, not a range of harness messages, so those
fields would name IDs that do not exist. Instead it carries a typed
`trigger`/`pre_tokens`/`post_tokens` payload (mirroring the CLI's own
`compact_metadata`) — a consumer reads these fields directly rather than
parsing the event's `text`, which carries the same data as a
human-readable string for logs only. It also carries
`compact_started_at`, the SAME shared field `history.compacted` carries
above: `consumeClaudeCodeStream` sets it to the instant this stream
observed the CLI's own preceding `"compacting"` status, zero when none
preceded the boundary in the same turn — a real, honest absence, not a
bug, distinct from the pre_tokens/post_tokens caveat below.

It also carries `context_used_tokens`/`context_window_tokens`, the SAME
shared fields `history.compacted` carries. Unlike `history.compacted`,
`context_used_tokens` here mirrors `post_tokens` directly rather than
`Session.ContextReading()`: at the instant this event fires, harness's own
`lastUsage` (which that call would read) still describes the CLI's PRE-
compaction turn — the CLI's own reported `post_tokens` is the only
signal at hand that already reflects the fold. It therefore shares
`post_tokens`' own absent-vs-zero caveat below. `context_window_tokens`
still reads `Session.ContextWindowTokens()`, unaffected by that caveat.

**Wire truth for the absent case.** Each of `trigger`/`pre_tokens`/
`post_tokens` is `omitempty` on `server.Event` (`server/journal.go`), and
each is ABSENT — the JSON key is missing, not present holding a zero value
— whenever the CLI's own envelope omitted `compact_metadata`, or omitted
that one field within it (the SDK's own type marks `post_tokens`
optional). This collapses two different real situations into one wire
shape a consumer cannot tell apart by the key alone: the CLI genuinely
reporting `0`, and the CLI reporting nothing at all — both serialize
identically (key absent) once the value has passed through the Go `int`
zero value and `omitempty`. This is a known, accepted limitation of the
underlying data, not a bug in this forwarding path. A console or any other
consumer MUST render "unknown" when a key is missing, never "0" — treating
an absent `pre_tokens`/`post_tokens` as a reported zero silently invents a
number the CLI never sent.

`compact_started_at` does not share this caveat: it is `time.Time` with
the `omitzero` tag, not `omitempty` on a plain value, so the zero time
alone already distinguishes "absent" from "a real, measured start" on
the wire — the same fix `FoldedTokensEst`'s `*int` applied to the engine
journal's own `compact_started_at`/`compact_folded_tokens_est` pair.
Fixing `pre_tokens`/`post_tokens` the same way is a separate change with
its own wire-compatibility question, not addressed here.

UNLIKE `compaction.failed`/`compaction.started` above, this event IS
journaled (`server/journal.go`'s `Publish` routes it through `emitDurable`,
not `publishLive`): it names no harness journal splice to reconcile on
replay, but it is still a fact about the session that happened at a point
in time, and a client that was not connected at that instant must still be
able to learn it happened later from an SSE bootstrap replay (`?from=N`)
or after a box hibernates and wakes. This is what actually closes the "the
console cannot even ask" gap the section above describes for a delegated
session — a live-only event closes it only for a tab that happens to be
open at the exact moment the CLI compacts, which is not a fix for the
gap's general shape.

`server/openapi.yaml`'s `Event` schema documents `compaction.claude_code`
and its `trigger`/`pre_tokens`/`post_tokens`/`compact_started_at` fields
alongside `history.compacted`/`compaction.failed`/`compaction.started`,
including the absent-vs-zero caveat above — the hand-written API contract
a caller reads instead of this design doc.

**Durable observability for the delegated lane.** Unlike the native lane,
a delegated compaction never leaves a `compact` record: `Session.Compact`
refuses outright on a delegated session, so there is no fold and nothing
to splice. Before this addition there was also nothing durable at all —
`compaction.claude_code` (above) is journaled at the SERVER's own
box-wide event log, not in this session's own store.go journal, so a
fleet operator reading a session's own log directly saw no trace that a
delegated compaction ever happened. `consumeClaudeCodeStream`
(`engine/claude_code_backend.go`) now writes a `claude_code.compact`
record to this session's own journal every time it observes a
`compact_boundary` envelope settle:

```json
{
  "type": "claude_code.compact",
  "created_at": "...",
  "claude_code_compact_trigger": "auto",
  "claude_code_compact_pre_tokens": 123456,
  "claude_code_compact_post_tokens": 7039,
  "claude_code_compact_started_at": "..."
}
```

`created_at` is when this stream observed `compact_boundary`.
`claude_code_compact_started_at` is when this SAME stream previously
observed a `"compacting"` status update, when it did — harness never
initiates or times the CLI's own compaction, so a start time is only ever
knowable when the CLI happens to report one first, in the same turn. It is
absent (never a guessed value) when no such status preceded the boundary:
the CLI's start and settlement can land in different turns, or a build
can omit the "compacting" status entirely. A record still journals in
that case, with whatever is known — trigger and token counts, at minimum
— rather than skipping observability altogether for lack of a duration.

Like the native lane, a failed or unsettled delegated compaction (a
`compact_result` other than `"success"`, or a turn whose stream ends with
compaction still outstanding) never journals a record — symmetric with
`compact`'s own "never record a skip" rule, and for the same reason: it
already has a live signal (`compaction.failed`), and a durable trace only
of successes keeps this record answering exactly one question, "how long
did a completed compaction take and how big was it," without also
becoming a general-purpose failure log.

This record is purely observational: nothing in `LoadSession`'s replay
switch needs a case for it, since it folds no session state. It also
carries no console-facing API surface of its own — it does not flow
through `GET /session/{id}/journal`'s projection, matching the sibling
`claude_code.usage`/`claude_code.session_id`/`claude_code.history_watermark`
records, none of which do either.

**The context-window gauge.** `Session.context` (`used_tokens`,
`window_tokens`) and the mirrored `turn.end` and `history.compacted` event
fields (`context_used_tokens`/`context_window_tokens`) expose the same two
numbers this section's trigger check compares. `compaction.claude_code` is
the exception: its `context_used_tokens` is the CLI's own `post_tokens`.
`used_tokens` is
`InputTokens + CacheReadTokens + CacheWriteTokens` from the most recent
completed turn, the identical expression `maybeAutoCompact` evaluates
against `window_tokens`, so a console gauge and automatic compaction agree
whenever the provider reported usage for that turn. They part company in one
case: when every input component is zero, `maybeAutoCompact` falls back to
`estimatePromptTokensFromHistory`, so compaction can act while `used_tokens`
still reads 0.

On the claude-code lane, `window_tokens` is the `contextWindow` the CLI
reports in the "result" event's `modelUsage` entry for the model named by
its "system"/"init" event. `recClaudeCodeUsage` records it, and a model
switch clears it. Until the CLI reports one, `window_tokens` is 0:
`modelmeta.ContextWindow` reports no figure for a claude-code ref, so
`resolveContextWindow` leaves the window unknown unless an explicit
configuration value sets it. `maybeAutoCompact` never runs for a delegated
turn and reads neither value.

**The retained measurement goes stale across a fold, on purpose — but the
reading does not have to go unknown.** A successful native compaction
removes the very history `LastUsage` was measured against, but
`LastUsage`/`maybeAutoCompact` must keep comparing against that retained
measurement — `compactHysteresis` already guards re-compaction separately,
and re-deriving it from a fold would defeat that guard. So `Session.Compact`
sets a second, independent flag (`contextUnknown`, exposed as
`Session.ContextUnknown()`) the moment a fold succeeds, and it stays set
until a turn next completes and measures a fresh `LastUsage`
(`appendWithUsage`/`applyClaudeCodeUsage`), the same event that already
updates the gauge. It never marks a skip
(`not_enough_turns`/`lone_existing_summary`/`summarizer_empty`): none of
those fold anything, so the retained reading still describes current
history.

An earlier version of this fix made every `used_tokens` projection report 0
whenever `contextUnknown` was set — reusing `window_tokens`' own "0 means
unknown" convention rather than inventing a second wire shape for the
condition. That was strictly worse than necessary: the post-fold history is
right there, and `estimatePromptTokensFromHistory` (above) is already the
signal `maybeAutoCompact` itself trusts to decide whether to compact at
all. Reporting "no known reading" to a gauge while simultaneously trusting
an estimate to trigger compaction was an unnecessary asymmetry. So
`Session.Compact` also computes `contextFoldEstimate` — the same
`estimatePromptTokensFromHistory` call, over the post-fold history, cached
once at the fold rather than recomputed on every read — and
`Session.ContextReading()` returns it (folded into `InputTokens`, since an
estimate does not split into input/cache-read/cache-write components) for
as long as `contextUnknown` holds. `used_tokens` therefore now carries
either an exact measurement or a size estimate, never distinguishing the
two on the wire: both answer the same question, "how full is context now,"
to the precision each has available, and no consumer needs to tell them
apart (see `server/openapi.yaml`'s `used_tokens` description).

This split is honestly asymmetric across the five projections, not
uniform. `contextJSONForSession`, `recordTurnEnd`, and
`publishHistoryCompacted` (`server/handlers.go`, `server/journal.go`) hold
a live `*engine.Session` and read `Session.ContextReading()` directly, so
they estimate. `recordTurnEnd` and `publishHistoryCompacted` matter most,
because `turn.end` and `history.compacted` are the live paths a console
gauge follows during a session while the other three projections only
answer on a read. `contextJSONForInfo` and `contextJSONForIndex` are cold
projections built from `SessionInfo`/`SessionIndex` — metadata sidecars
that carry `ContextUnknown` and `LastPromptTokens` but never the message
history a fold-time estimate needs — so they cannot estimate and keep
reporting plain 0, exactly as before. `ContextReading()` returns the usage
and the flag under one lock, so a fold landing between two separate reads
cannot pair a stale usage with a cleared flag, and a projection cannot
report the usage while forgetting the gate.

The estimate is deliberately cached AT THE FOLD, not recomputed on every
read: `estimatePromptTokensFromHistory` walks every message's parts, and a
session's history can hold hundreds of messages (the 631-message session in
its own doc comment is the extreme case on file) — cheap once per
compaction, but a GET /session poll runs far more often than a fold does,
and would otherwise pay that walk on every request for as long as
`contextUnknown` holds. Both the live fold (`Session.Compact`) and its
`LoadSession` replay counterpart (`recCompact`, `engine/store.go`) compute
`contextFoldEstimate` once, immediately after splicing the post-fold
history, so a reload reconstructs the identical value a live process would
have cached. Replay estimates over `message.ResolveOrphanToolCalls(s.history)`
rather than the accumulated `s.history` at that point in the tail scan,
without assigning the repaired copy back: the load-time orphan repair itself
runs only once, after the whole scan, so an orphaned tool_call retained past
a fold would otherwise be uncounted at cache time even though the same
repair later fixes `s.history` itself — a live fold never faces this, since
its in-memory history is always already repaired by construction (see
`message.ResolveOrphanToolCalls`'s own doc comment). A snapshot taken while
`contextUnknown` holds carries
`contextFoldEstimate` as its own field (`engine/snapshot.go`) rather than
recomputing it from the snapshot's `History` at restore — `History` there
can already include turn-in-progress messages appended after the fold,
which would disagree with the frozen value a full replay produces.

This design deliberately does NOT extend the estimate to the OTHER
zero-reading case: a session with no completed turn yet reports plain 0
too, unchanged. `contextUnknown` cannot collapse into "no measurement
recorded" (`LastPromptTokens == 0`) to cover both cases uniformly, because
the cold projections already need the two distinguished —
`contextJSONForInfo`/`contextJSONForIndex` gate on `!ContextUnknown &&
LastPromptTokens != 0` precisely so a fold-invalidated reading and a
genuinely fresh session read the same 0 for different, and separately
necessary, reasons. Estimating the pre-first-turn case would also have no
fold event to cache it at, reopening the same per-read cost this design
avoids everywhere else. It survives a reload: `LoadSession`'s `recCompact`
replay sets `contextUnknown` exactly where live `Compact` does, and the
durable sidecar index (`SessionIndex.ContextUnknown`, `engine/index.go`)
and the journal-scan fallback (`SessionInfo.ContextUnknown`,
`engine/store.go`) fold the same rule from the same records, so a cold read
never resurrects the stale number a live process would have suppressed.

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
  record type, and config fields are all purely additive. This no longer
  holds unconditionally after the model-derivation follow-up above: a
  deployment whose sessions run a model `modelmeta` recognizes now gets
  compaction armed where it previously silently wasn't — the intended fix
  for the disarmed-compaction gap above, not a regression, but worth calling out
  explicitly since it is the one behavior change on an upgrade with no
  config edit. An explicit `context_window_tokens: 0` is not distinguishable
  from "unset" (see `config.Config.ContextWindowTokens`'s doc comment) — a
  deployment that genuinely wants compaction to stay off for a recognized
  model has no way to say so today; filed as a follow-up, not blocking here.
