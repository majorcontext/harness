# Live-from-tip resume cursor

Status: implemented.

## 1. The problem, as measured

The boxes console bootstraps a session view with `GET
/session/{id}/message?stream_from=1` (the `Transcript` envelope:
`messages` plus `stream_from`, see `server/journal.go`'s
`transcriptSyncedThrough`), then opens `GET
/event?from=<stream_from>&session=<id>` to receive live updates from that
point on.

`stream_from` is `transcriptWatermarkLocked`'s answer: the highest durable
event-journal seq among this session's own `evtMessage` records whose
message ID is present in the `messages` snapshot just returned. It is
deliberately NOT the plain highest seq recorded for the session
(`sessionSeqLocked`) — see that function's own doc comment for why a naive
max reopens a duplicate-delivery race.

The event-journal's seq space is box-global: one counter (`Server.seq`),
shared by every session and every durable event type (`evtMessage`,
`evtSessionStatus`, `evtTurnEnd`, `evtModel`, ...). A session that has run a
lot of nested activity under one id — a claude-code/Codex session whose
subagent turns land in the same session's journal (harness#217, #223: the
cross-family `task` tool and list-only model exposed over the MCP shim) —
accumulates many durable records whose seq sits above its own message
watermark, simply because `transcriptWatermarkLocked` only ever looks at
`evtMessage` records that ended up in `messages`; every other durable
record type, and every `evtMessage` record NOT in `messages` (excluded for
any reason: a splice-timing edge case, or, at scale, simply because the
message boundary itself), is invisible to it.

Measured on one real production session: opening `GET
/event?from=<stream_from>&session=<id>` replayed ~13.5 MB / ~1800 message
events — a full backlog the console immediately discards, since it only
wants what changed after the bootstrap read. The console renders this as a
visible two-stage "sparse transcript, then a flood" instead of loading
cleanly once.

## 2. Chosen design

Add one field to the existing `Transcript` envelope
(`server/handlers.go`'s `transcriptJSON`, the `?stream_from=1` response
shape) and to `transcriptSyncedThrough`'s return values: `live_from`, the
box-global event-journal tip the console should pass as `/event`'s `from`
parameter for a LIVE-ONLY resume — one with no backlog, at the cost of
giving up `stream_from`'s own narrower self-heal guarantee (see §4).
`stream_from` itself is untouched: same computation, same field, same
meaning, still returned, still the value a caller that wants the
`messages`-consistent, self-healing resume point should use.

```go
// server/journal.go
func (s *Server) transcriptSyncedThrough(id string) (
    history []message.Message, seq int64, liveFrom int64, ok bool)
```

```go
// server/handlers.go
type transcriptJSON struct {
    Messages   []json.RawMessage `json:"messages"`
    StreamFrom int64             `json:"stream_from"`
    LiveFrom   int64             `json:"live_from"`
}
```

`liveFrom` is computed as `max(seq, tipAtStart)`, where:

- `seq` is the existing message watermark, unchanged.
- `tipAtStart` is `s.seq` sampled (via the existing `currentSeq` helper)
  as the FIRST thing `transcriptSyncedThrough` does, strictly before it
  calls `sess.History()`.

Both `seq` and `tipAtStart` are individually proven, in §4, never to sit at
or above the seq of a durable record this call must keep replayable. Their
max is the tightest cursor that still honors both proofs. In practice
`live_from` sits at or above `stream_from` from the moment a session is
created, not just once a lot of activity accumulates: `createSession`
itself durably journals one `evtSessionCreated` record before any message
exists, so `tipAtStart` can already be 1 while `stream_from` (which has
nothing to count yet) is still 0. Once a turn completes, its own trailing
`evtSessionStatus`/`evtTurnEnd` records are journaled ABOVE its last
message's seq too, and `tipAtStart` (sampled after the turn finished)
counts those while `seq` never does — so even an ordinary single-turn
session with no unusual backlog already shows `live_from` strictly above
`stream_from`. That is not a special case to route around; it is exactly
the mechanism §1 exists to fix, just visible at a smaller scale than the
measured 1800-event backlog.

This is additive only: no existing field, return value, endpoint
behavior, or wire shape changes. A caller that has never heard of
`live_from` gets byte-for-byte what it always got, plus one new integer in
a response it already opted into via `?stream_from=1`.

### Why not change `/event`'s own semantics

`/event`'s `from` parameter keeps meaning exactly what it means today:
replay every durable record for the (optionally session-filtered) stream
with `seq > from`, then continue live. No new query parameter, no new
replay mode, no branch on which KIND of cursor `from` is — the endpoint
cannot tell `stream_from` and `live_from` apart, because there is nothing
to tell apart: both are the same integer type, used the same way, at the
same parameter. This is what makes the change safe for every current
consumer of `/event?from=` (enumerated in §3): none of them changes
behavior, because none of them passes anything different than before. A
NEW caller (the boxes console's bootstrap path, in a follow-up change
outside this repository) simply starts choosing which of the two numbers
in the `Transcript` envelope to hand to the same, unmodified endpoint.

### Why not cap the message watermark itself, or filter `/event` by type

Redefining `stream_from` to already equal a broader tip would change its
value for every existing caller, breaking requirement #1 (an additive
change, not a redefinition) and, worse, would break `stream_from`'s own
self-heal guarantee for a message that is legitimately excluded from
`messages` at read time only because of a race (§4's `tipAtStart` proof
exists specifically because `stream_from` must keep NOT doing this).
Filtering `/event` to only ever replay `evtMessage` records, or to only
replay records "new since a session last observed the journal," would
change `/event`'s answer for the mirror/console-read-path consumer (§3),
which depends on a full, unfiltered replay from an arbitrary earlier seq
to rebuild an authoritative transcript.

## 3. Consumer-impact enumeration

Every current caller of `stream_from` (the field) and of `/event?from=`
(the endpoint), found by a repository-wide search of `server/`, the only
directory that reads or writes either:

| Consumer | File | Effect of this change |
|---|---|---|
| `handleMessages`' `?stream_from=1` branch | `server/handlers.go` | Gains one field (`live_from`) on its response. Every other branch (bare array, `MessagePage`) is untouched — this change touches no code path that a `stream_from`-unaware caller exercises. |
| `transcriptSyncedThrough`, `transcriptWatermarkLocked` | `server/journal.go` | `transcriptSyncedThrough` gains a return value and one extra `currentSeq()` read before its existing unlocked `sess.History()` read. `transcriptWatermarkLocked` — the function that computes `stream_from` itself — is not modified at all. |
| `handleEvent` (`GET /event`) | `server/sse.go` | Not modified. Still replays `seq > from` for the (optional) session filter, then streams live. It has no notion of `live_from` and needs none — see §2. |
| `server/openapi.yaml`'s `Transcript` schema and `/event` operation | `server/openapi.yaml` | `Transcript` gains a documented `live_from` property, marked required alongside the existing `stream_from`. The `/event` operation's own description is unchanged: its contract does not vary by which cursor a caller happens to pass. |
| Existing tests (`server/transcript_sync_test.go`) | `server/transcript_sync_test.go` | `transcriptResponse` (the test's own decode-shape) gains a `LiveFrom` field to stay a faithful mirror of the wire shape; every existing assertion is about `stream_from`/`messages` and is unaffected. |
| Console bootstrap path (`meetneptune/boxes`, out of this repository) | N/A | Out of scope for this change (harness-only, per this task). It keeps reading `stream_from` exactly as it does today until a follow-up change there opts into `live_from`. |
| Console-read-path mirror consumer (`meetneptune/boxes`'s `docs/console-read-path.md`, out of this repository) | N/A | That consumer resumes `/event?from=` from an EARLIER seq than any bootstrap watermark, specifically to replay a full authoritative window — see `server/sse.go`'s own doc comment ("replay covers `(from, max]`"). Nothing about `/event`'s replay semantics changed, so this keeps working exactly as it does today. |

No other file in the repository (outside `.claude/worktrees/*`, stale
branch snapshots not part of `main`) references `stream_from`,
`StreamFrom`, or reads `/event`'s `from` parameter.

## 4. Race-close argument

Claim: `live_from = max(seq, tipAtStart)` never sits at or above the seq
of a durable record for session `id` that a client, having received this
response, still needs to receive via `/event?from=live_from&session=id` —
and never sits below the seq of any record already fully represented by
this response, so resuming from it replays nothing the client already has.

`seq` (the existing message watermark) already carries this proof for
every message in `messages`: `transcriptWatermarkLocked`'s own doc comment
establishes that it is at least the seq of every in-`messages` message,
and strictly below the seq of any message journaled after this call
returns (nothing can be appended to this session's journal between "the
snapshot is fully durable" and "`seq` was read," because `emitDurableLocked`
never runs without `s.mu`, which this call holds for exactly that span).
`live_from >= seq`, so `live_from` inherits the "not below `messages`"
half of that proof, and the "resume misses nothing appended after this
call returns" half too (`s.seq` only grows under `s.mu`, and `live_from`
is read inside the same critical section as `seq`, at or after it).

The new half of the claim — `live_from` does not swallow a record that
races into the gap between this call's UNLOCKED `sess.History()` read and
its `s.mu.Lock()` (the exact race `TestTranscriptStreamFrom_
ConcurrentJournalDuringSnapshot` forces deterministically via the
`transcriptSyncRace` seam, and the race `transcriptWatermarkLocked`'s own
doc comment proves `seq` alone avoids) — is what `tipAtStart` is for.

`tipAtStart` is `s.seq`, read under `s.mu` and released, as the FIRST
statement in `transcriptSyncedThrough`, strictly before `sess.History()`
is even called. Take any durable record R for session `id` that is
excluded from the `history` this call returns. By definition of
"excluded," R's message was appended to the engine session's own history
strictly AFTER this call's `sess.History()` read returned its snapshot
(`sess.History()` returns every message present as of the instant it
runs; R is absent, so it was not yet present then). Every record this
codebase journals is journaled as a REACTION to observing its message
already present in some caller's `sess.History()` snapshot — journaling
never precedes the append it journals. So R's own journaling (`s.seq++`
under `s.mu`) happens no earlier than the append that made it visible,
which happens no earlier than (strictly after) this call's own
`sess.History()` read, which happens no earlier than (strictly after,
same goroutine, same program order) the critical section that already
read and released `tipAtStart`. Mutex serialization turns that
"happens no earlier than a released critical section" into "R's
`s.seq++` runs in a LATER critical section than `tipAtStart`'s own,"
and `s.seq` only grows — so R's assigned seq is strictly greater than
`tipAtStart`. `live_from >= tipAtStart` never on its own forces
`live_from` past R's seq purely from `tipAtStart`'s contribution; whether
the OTHER contributor, `seq`, could independently exceed R's seq is
exactly the case `transcriptWatermarkLocked`'s own proof already rules
out (R is excluded from `messages`, so `seq` does not count it either).
So neither term of the `max` can put `live_from` at or above R's seq: R
stays strictly above `live_from`, and `/event?from=live_from` still
replays it.

This argument does not depend on WHY R is excluded from `history` —
a plain concurrent race, a compaction splice-timing sandwich, or a
subagent turn boundary all fit the same "appended after this call's
`sess.History()` read" shape (the compaction case is the one existing
tests already probe explicitly; see `TestTranscriptWatermarkLocked_
CompactionSummarySandwich`). It generalizes cleanly, needing no
per-cause special-casing beyond what `transcriptWatermarkLocked` already
carries for `seq`.

`TestTranscriptLiveFrom_NoGapConcurrentRace` (`server/transcript_live_from_
test.go`) pins this exact argument: it forces a real message into the
documented unlocked-read gap via the same seam the existing
`ConcurrentJournalDuringSnapshot` test uses, asserts `live_from` sits
strictly below the raced message's seq, and then opens a real SSE
connection at `from=live_from` to prove the raced message is actually
delivered — not just that the inequality holds on paper.

### The one case this design does not cover

A durable record R excluded from `messages` for the SAME race, if it
races into the gap between `tipAtStart`'s own read and `sess.History()`'s
read (a narrower sub-window of the same gap), is still correctly kept
above `live_from` by the proof above — so there is no uncovered case for
`live_from`'s own contract. What `live_from` does deliberately give up,
relative to `stream_from`, is `stream_from`'s SELF-HEAL promise for a
message that ends up correctly excluded from `messages` for a reason
`transcriptWatermarkLocked`'s cap logic does not track (e.g., an
in-flight compaction sandwich, or a large volume of subagent-turn
`evtMessage` records that predate this call, all fully captured by
`tipAtStart`'s proof above and hence deliberately BELOW `live_from` by
design — that is the whole point). A console using `live_from` as its
live-resume cursor is choosing "no backlog flood" over "SSE alone will
eventually redeliver everything," consistent with this task's own stated
architecture: the console's REST transcript read plus backward pagination
remains the sole source of truth for history, and `live_from`'s stream
exists only to report what changed after the bootstrap read, never to
reconstruct history. A caller that still wants `stream_from`'s narrower
self-heal guarantee keeps it, unchanged, in the same response.

## 5. Testing

`server/transcript_live_from_test.go`:

- `TestTranscriptLiveFrom_AtLeastMessageWatermark`: contract sanity —
  `live_from >= stream_from` always, including immediately after session
  creation (before any message exists, `stream_from` is 0 while
  `live_from` can already be 1, per §2), and `live_from` is strictly
  greater once a single ordinary turn has completed, since its own
  trailing status/turn-end records sit above the message watermark too.
- `TestTranscriptLiveFrom_SkipsStaleBacklogButOldWatermarkDoesNot`: the
  differential test. Fabricates a stand-in for the measured production
  backlog — durable `evtMessage` records for one session, excluded from
  its own `messages`, journaled before the bootstrap read — and proves (a)
  `live_from > stream_from`, (b) resuming from the OLD `stream_from`
  replays every one of them (red-verifying today's bug), (c) resuming
  from the NEW `live_from` replays none.
- `TestTranscriptLiveFrom_RealSessionNoBacklogAfterBootstrap`: the same
  property against a real, non-fabricated event stream — two real turns
  before the bootstrap read, one real turn after — proving the live
  stream at `live_from` carries only the after-read turn.
- `TestTranscriptLiveFrom_NoGapConcurrentRace`: the race-close proof made
  concrete, described above.
- `TestEventReplayFromEarlierSeq_UnaffectedByLiveFrom`: the mirror/replay
  regression pin — `/event?from=<an earlier seq>` still replays every
  durable record above it, unfiltered, exactly as before, regardless of
  where `live_from` for the same session happens to sit.
