# Event sink: outbound journal forwarding

Status: implemented.

## 1. The problem

A consumer that wants a replica of a box's durable journal has one option
today: hold `GET /event` open, per instance, for as long as it wants to stay
current. A restart, a redeploy, or a fresh consumer resuming from `from=0`
moves the box's entire durable record set over the wire to relearn a tiny
amount of new state. There is no push path: harness never initiates an
outbound delivery of its own journal.

## 2. The contract

By default a configured sink receives every durable record — the same
records the journal keeps and `/event?from=` replays — in seq order,
nothing projected or filtered. Live-only record types (`text.delta`,
`reasoning.delta`, `tool.start`, `tool.end`) are never journaled in the
first place (see `server/journal.go`), so they are out by construction, not
by a filter this change adds.

`event_sink.include_types` narrows that record set. Section 10 gives the
sparse-range semantics that the selector introduces.

## 3. The tap signals, it does not carry

`emitDurableLocked` calls `s.notifySinkLocked()` right after
`s.notifyWaitersLocked` (`server/journal.go`). `notifySinkLocked` sends a
SIGNAL on a buffered, size-1 channel — never the record itself, and a
send that finds the channel already full is dropped, not queued.

This is the opposite of `fanoutLocked`'s own drop policy for a slow SSE
subscriber, and deliberately so. Dropping a live event for a slow SSE
client is correct: that client can reconnect and ask `/event` to replay
from its own last-seen seq. Dropping a *record* bound for the pump would be
wrong: the pump has no independent record of what it missed, so a dropped
record would open a permanent hole in the replica. Sending only a wake
avoids that failure mode entirely — a coalesced or dropped wake cannot
lose anything, because the pump re-reads the journal from its own cursor
on every wake and picks up whatever accumulated since the last one.

## 4. The pump is a cursor over the journal

`runEventSink` is a goroutine, started by `server.New` only when
`Options.EventSink` is non-nil and `Options.SessionDir` is set. It waits on
the wake channel, then flushes: `flushEventSink` calls `nextEventBatch` to
slice the in-memory journal above `s.sinkCursor`, hands the resulting batch
to `Options.EventSink.Deliver`, and advances the cursor by whatever the
reply reports. The loop repeats until `nextEventBatch` reports nothing left
to send.

There is no separate outbound spool, retry queue, or on-disk staging area
for undelivered records. The journal itself is the buffer.

**This works because `s.journal` is append-only and never trimmed.** Every
durable record a session has ever produced stays resident in
`Server.journal`, in seq order, for the life of the process, and
`nextEventBatch` finds the first record above the cursor with a binary
search (`sort.Search`) that depends on that invariant — both on the
records still being there to search, and on the slice staying sorted by
seq. If the journal ever gains eviction (a size or age-based trim of
`s.journal` in memory), the pump can no longer assume the whole history is
resident, and `nextEventBatch` must fall back to reading `events.jsonl` at
an offset for whatever the eviction discarded. That is out of scope for
this change; nothing about the current implementation does it, and nothing
about the current design doc should be read as promising it will keep
working unmodified if eviction is added later.

## 5. A retryable failure does not give up

`flushEventSink`'s delivery loop, on a retryable `Deliver` error, logs a
warning and retries the SAME batch after `eventSinkRetryDelay` (2 seconds) —
it does not advance past the failure, drop the batch, or wait for a new
record to arrive before trying again. It keeps retrying, at that fixed
interval, until `Deliver` succeeds or the pump is retired (`sinkStop`, closed
after the prompt drain — see §7), at which point the goroutine exits without
another attempt.

The consequence: a receiver that is down, or answering a retryable error,
does not lose any records. It delays them. An unreachable receiver leaves
the pump retrying every `eventSinkRetryDelay` for the rest of the process's
life — this is intended, not a bug to fix later, because the journal (§4) is
already the buffer holding everything the pump has not yet managed to
deliver. There is nothing else for the pump to spool to, and nothing is lost
by continuing to retry against something the journal already holds.

Section 14 gives the one class of failure this does not cover: a receiver
that rejects the batch itself.

## 6. The receiver owns the cursor

`Deliver` returns `appliedThrough`, the seq the receiver has durably
applied, and `advanceSinkCursor` sets `s.sinkCursor` to exactly that value
(clamped to `[0, s.seq]`; never negative, never past what this server has
actually assigned). Harness keeps no cursor of its own beyond that one
in-memory integer, which is not itself durable across a restart — the
receiver's own answer is the only source of truth for "how far did this
replica get."

**The receiver acknowledges through `to_seq`, not through the seq of the
last record in `records`.** The two are the same integer only while the
pump is unfiltered. Section 10 explains why a selector separates them.

A rewind is nothing special: if the receiver answers a seq lower than what
it previously reported (a rollback, a lost write, a fresh receiver that
lost its own state), the cursor moves backward and the very next batch
re-ships everything from that point forward. A full re-bootstrap is the
same mechanism at its extreme: a receiver that has nothing yet answers 0,
and the pump starts shipping from the very first durable record.

## 7. What this does not promise

There is no at-least-once delivery claim across every failure mode a
deployment might hit:

- A wiped journal (the process loses `s.journal` and `events.jsonl` both,
  e.g. disk loss) loses whatever had not yet been applied by the receiver.
- A receiver that silently skips a record it cannot parse (a "poison"
  record) but still answers a cursor past it has told harness it applied
  something it did not. Harness has no way to detect this; it trusts the
  receiver's own `appliedThrough`.
- A process deleted before its tail ships — journal and all — loses that
  tail. The pump's final flush covers an orderly shutdown, not a deletion
  that never lets the process run that path.

An orderly shutdown IS covered, but only because of where the pump is
retired. `Drain` closes `s.closing` first and only then waits for in-flight
prompts, and those prompts journal their trailing records during that wait —
a final assistant message, a `session.aborted` per cancelled prompt, the
`session.status(idle)` transitions. So the pump watches its own `sinkStop`
channel, which `Drain` closes in a deferred call AFTER that wait, rather
than watching `s.closing`. A pump retired at the start of the drain would
exit before those records existed and lose every one of them, while Drain's
own `sinkDone` wait returned instantly having guarded nothing.
`TestEventSinkShipsRecordsJournaledDuringDrain` pins this.

`stopEventSink` also cancels the ordinary pump context. A transport that is
blocked in `Deliver` can then return instead of outliving the shutdown budget.
After that cancellation, the pump makes one final catch-up pass under the
`Drain` context. A failed final delivery does not retry because shutdown has
already begun. `Close` without `Drain` supplies an already-canceled final
context: it retires the pump but makes no graceful-delivery promise.
`TestDrainCancelsBlockedDeliveryBeforeFinalFlush` pins both cancellation and
the final attempt.

A restarted process ships its restored journal without waiting for a new
record. `loadJournal` appends the journal straight to `s.journal`, never
through `emitDurableLocked`, so nothing wakes the pump for records this
process did not itself emit; `runEventSink` therefore flushes once before
entering its wait loop. Without that, a box that restarts and goes idle
replicates nothing at all — which would defeat the whole point of reading a
transcript without waking the box.
`TestEventSinkShipsARestoredJournalWithNoNewRecord` pins this.

## 8. Layering

The transport is an `Options.EventSink` callback (`server.EventSink`,
`server/eventsink.go`), implemented in `cmd/harness` (`httpEventSink`,
`cmd/harness/eventsink.go`) rather than in `server/` itself. `server/`
holds the pump, the cursor, and the tap, but no outbound HTTP client —
consistent with every other `Options` hook this package already uses to
keep `cmd/harness`-only dependencies out of `server/`. This is also why the
wire shape (`sinkBody`, with its `generation` field) lives in
`cmd/harness`, not in `server.EventBatch`: `EventBatch` is what the server
hands to ANY `EventSink` implementation, and the server has no reason to
carry a value it never reads.

## 9. The generation is opaque

`config.EventSinkSpec.Generation` is a label naming which journal a batch
of seqs belongs to. Harness stamps it on every request and never
interprets it — the deployment that configures the sink mints its own
value and gives it whatever meaning it needs (for example, disambiguating
one box's journal from another box that reused the same session
directory, or from the same box across a disk replacement). Nothing in
this repository parses, validates, or branches on its contents.

## 10. The selector makes a scanned range sparse

`event_sink.include_types` is a list of durable event types. An absent or
an empty list keeps the pump unfiltered. `eventSinkTypeSet`
(`server/eventsink.go`) turns that list into a nil set, and a nil set is
what tells `nextEventBatch` to stay on the dense path.

An unfiltered request keeps the envelope a receiver saw before the selector
existed. The wire field is `filtered` with `omitempty` (`sinkBody`,
`cmd/harness/eventsink.go`), so an unfiltered request omits the key. It
does not send `"filtered":false`. A receiver that predates the selector
needs no update for the selector.

This is a statement about the envelope, not about the bytes of a record.
Section 12 adds `recorded_at` to every newly emitted durable record, so an
unfiltered request is no longer byte-identical to a pre-selector one. Both
changes are additive: a receiver that ignores an unknown key reads either
request unchanged.

A non-empty list turns the pump filtered. Then:

- `from_seq` and `to_seq` bound the range the pump SCANNED, not the range
  the request carries. `records` holds only the scanned records whose
  `type` is in the list. It is sparse, and it can be empty.
- `filtered` is `true` on every request that a filtered pump sends, even on
  one whose selector matched every scanned record. The flag reports the
  pump's mode. Its meaning does not change from request to request, so the
  receiver can trust `to_seq` without inspecting `records`.
- An empty `records` encodes as `[]`, never as `null`.
- The receiver must acknowledge through `to_seq`. A receiver that answers
  the seq of the last record in `records` re-receives the whole unselected
  tail on every request. A receiver that answers `0` for an empty `records`
  rewinds the cursor to the start of the journal (§6).

An empty `records` is a checkpoint, not an error. This is a complete
request and a complete reply:

```json
{"generation":"jrnl_test","from_seq":8,"to_seq":12,"filtered":true,"records":[]}
```

```json
{"applied_through":12}
```

The checkpoint is how the cursor crosses a long run of unselected records.
Without it, a session that produces nothing the selector wants would hold
the cursor at the last selected record for the life of the process.

A filtered pump therefore costs requests that an unfiltered pump does not.
A busy session whose records are all unselected sends one empty checkpoint
per flush window, and each one carries only a cursor. This is the accepted
trade for the smaller record volume.

## 11. Type matching is exact

`eventSinkTypeSet` builds a map and `nextEventBatch` looks up the record's
`type` in it. The match is exact, case-sensitive string equality. There is
no prefix rule, no glob, and no namespace rule: `turn` does not select
`turn.end`, and `Turn.End` selects nothing.

**A misspelled type selects nothing, and harness reports no error.** The
`config` package validates the structure of the list only. It rejects an
empty string, leading or trailing whitespace, and a duplicate. It does not
validate a name, because the journal owns the type set and this repository
holds no closed enumeration of it to check against.

The failure is therefore silent. A selector whose entries are all
misspelled produces a stream of empty checkpoints that advance the cursor
and deliver no record at all. Copy each type from a live `/event` stream,
or from the `Publish` cases in `server/journal.go`, rather than typing it
from memory.

## 12. Every new durable record carries its own instant

A receiver that replays a journal needs the age of each record. `seq` orders
records but dates none of them, and the delivery time is the wrong clock: a
box that restarts and ships its whole restored journal delivers a month-old
record and a fresh one in the same request. Boxes expires a replayed record
by age, so the record has to carry that age itself.

`emitDurableLocked` (`server/journal.go`) sets `Event.RecordedAt` from
`Server.now`, converted to UTC, right after it assigns the seq — ahead of
`writeJournalLocked` and ahead of any `nextEventBatch` copy. One record
therefore carries one identical instant wherever it appears: in its journal
line, on the SSE stream, and in a sink batch. A stamp added at delivery time
instead would date the record from the pump, and a stamp added at load time
would date it from the restart.

The stamp is an emission time, not a persistence receipt. It is assigned
immediately before the append is attempted, and `writeJournalLocked` reports
a failed append through `s.lastErr` and `Options.OnError` without ever making
it fatal. A record whose journal line never landed therefore still carries
its stamp, still fans out to a subscriber, and still ships to the sink. A
consumer reads the instant the server assigned the record, never proof that
the line reached the disk.

The stamp lands in the durable primitive only. A live-only event goes through
`publishLive`, which never reaches `emitDurableLocked`, so `text.delta` and
its peers carry no `recorded_at` — the same construction that keeps them out
of the journal in the first place (§2). `emitDurableLocked` also leaves a
non-zero `RecordedAt` alone, so a re-emitted record keeps its original age.

The wire field is `recorded_at` with `omitzero`, not `omitempty`:
`encoding/json` drops nothing for an `omitempty` struct field, so `omitempty`
would ship an explicit `"0001-01-01T00:00:00Z"` on every record that has no
stamp. `omitzero` omits the key, which is the shape the rest of `Event`
already uses for an optional field.

## 13. A record written before the stamp existed stays undated

`loadJournal` appends what it parsed. A journal line written before
`recorded_at` existed has no such key, decodes to the zero `time.Time`, and
keeps it — `loadJournal` must never backfill the field. A backfill would date
every historical record from the restart, so a month-old transcript would
reach Boxes looking brand new and would never expire.

The zero value is what Boxes reads as expired, which is the intended outcome
for a record whose real age is unknown. `omitzero` (§12) also keeps the key
off the wire for such a record, so a receiver can tell "undated" from
"dated at the epoch" without a special case.

`TestDurableEventStampsRecordedAtFromTheInjectedClock`,
`TestLiveEventCarriesNoRecordedAt`, and
`TestLegacyEventKeepsAZeroRecordedAtOnReload` pin these three rules.

## 14. A permanent rejection retires the pump

A retry is a bet that the same bytes can succeed later (§5). Some receiver
answers say they cannot. A receiver that rejects the batch itself — a body
it cannot parse, a credential it refuses, a route that holds no receiver —
answers the identical rejection to the identical retry, every two seconds,
for the life of the process. That loop delivers nothing, and it logs a
warning on every pass, which buries every other line an operator reads.

`server.ErrEventSinkPermanent` (`server/eventsink.go`) is the sentinel for
that class. A transport wraps it; `flushEventSink` detects it with
`errors.Is`, logs one bounded warning, and returns false, which retires
`runEventSink`. The pump goroutine exits and `sinkDone` closes.

**Harness does not stop.** The sentinel retires the replica, nothing else:
sessions run, records still journal and still reach `/event`, and `Drain`
still returns (it waits on a `sinkDone` that is already closed). The
deployment loses forwarding, not the box.
`TestEventSinkPermanentRejectionStopsThePumpWithoutStoppingHarness` pins the
stop, the single warning, and the still-healthy server.
`TestEventSinkRetryableFailureIsNotPermanent` pins the two-second retry that
a retryable error still gets.

`httpEventSink` classifies by status alone (`eventSinkPermanentStatus`,
`cmd/harness/eventsink.go`):

| Status | Class | Why |
|---|---|---|
| 400, 422 | permanent | The receiver cannot parse or accept this body. |
| 401, 403 | permanent | The credential is refused, not throttled. |
| 404, 410 | permanent | The URL names no receiver. |
| 409 | permanent | The batch contradicts what the receiver applied. |
| 408, 425, 429 | retryable | The receiver asks for the same batch later. |
| 5xx | retryable | A receiver a restart or a failover fixes. |
| any other status | retryable | The set is fixed, not "every 4xx". |
| transport failure | retryable | A dial or a timeout carries no verdict. |

The status is the whole classifier. A permanent status with no body is still
permanent, and a retryable status that carries a diagnostic is still
retryable. The diagnostic itself is unchanged: `eventSinkDiagnosticCode`
still extracts only the bounded `code` field, so the pump logs the status
and that machine code, never the receiver's free text and never the
configured URL. `TestHTTPEventSinkClassifiesPermanentReceiverRejections`,
`TestHTTPEventSinkPermanentRejectionKeepsABoundedDiagnostic`, and
`TestHTTPEventSinkTransportFailureIsNotPermanent` pin the table above.

A permanent rejection is a configuration report, not a data loss. The
journal keeps every record, so fixing the receiver and restarting harness
resumes forwarding from whatever cursor the receiver answers next (§6).
