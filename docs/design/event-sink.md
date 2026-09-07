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

A configured sink receives every durable record — the same records the
journal keeps and `/event?from=` replays — in seq order, nothing projected
or filtered. Live-only record types (`text.delta`, `reasoning.delta`,
`tool.start`, `tool.end`) are never journaled in the first place (see
`server/journal.go`), so they are out by construction, not by a filter this
change adds.

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

## 5. Failure does not give up

`flushEventSink`'s delivery loop, on a `Deliver` error, logs a warning and
retries the SAME batch after `eventSinkRetryDelay` (2 seconds) — it does
not advance past the failure, drop the batch, or wait for a new record to
arrive before trying again. It keeps retrying, at that fixed interval,
until `Deliver` succeeds or the pump is retired (`sinkStop`, closed after
the prompt drain — see §7), at which point the goroutine exits without
another attempt.

The consequence: a receiver that is down, or answering errors, does not
lose any records. It delays them. A permanently unreachable receiver
leaves the pump retrying every `eventSinkRetryDelay` for the rest of the
process's life — this is intended, not a bug to fix later, because the
journal (§4) is already the buffer holding everything the pump has not
yet managed to deliver. There is nothing else for the pump to spool to,
and nothing is lost by continuing to retry against something the journal
already holds.

## 6. The receiver owns the cursor

`Deliver` returns `appliedThrough`, the seq the receiver has durably
applied, and `advanceSinkCursor` sets `s.sinkCursor` to exactly that value
(clamped to `[0, s.seq]`; never negative, never past what this server has
actually assigned). Harness keeps no cursor of its own beyond that one
in-memory integer, which is not itself durable across a restart — the
receiver's own answer is the only source of truth for "how far did this
replica get."

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
