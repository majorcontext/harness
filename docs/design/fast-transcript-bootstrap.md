# Fast transcript bootstrap

Status: implemented.
Extends: `docs/design/journal-snapshotting.md` (Layer B), `docs/design/
live-event-tip-cursor.md`, `docs/design/transcript-tail-seqs.md`.
Related (other repo): `meetneptune/boxes`'s `docs/design/
transcript-backward-pagination.md` and `docs/design/
transcript-scroll-first-load.md`.

## 1. Problem, as measured and as cited

The console's session-bootstrap read is `GET /session/{id}/message?
stream_from=1`. A live cold read of it measured **9.5 s**
(`slow_harness_round_trip`, 1 s threshold). `handleMessages`
(`server/handlers.go:1212`) routes `?stream_from=1` to
`transcriptSyncedThrough` (`server/journal.go:1059`), which calls
`s.lookupSession(id)` (`server/handlers.go:3933`). For a session this
process does not currently hold resident, that calls
`s.opts.LoadSession(id)` → `engine.LoadSession` (`engine/store.go:1438`).

`LoadSession` unconditionally reads the **whole** journal file before it
looks at anything else:

```go
data, err := os.ReadFile(sessionPath(cfg.SessionDir, id))  // store.go:1445
...
head := countJournalRecords(data)                          // store.go:1461
...
startAfter := s.snapshotStartAfter(cfg.SessionDir, id, data, head) // :1469
```

`countJournalRecords` and the tail scan that follows both go through
`scanLogRaw`, which does `bytes.Split(data, []byte("\n"))` over the
**entire buffer** (`store.go:2154`) before the loop decides, line by line,
whether to decode it:

```go
err = scanLogRaw(data, func(raw []byte, line int, isLast bool) error {
    if int64(line) <= startAfter {
        s.recordsWritten = int64(line)
        return nil // covered by the snapshot; the header is already applied
    }
    ...
    return apply(rec, line, isLast)
})
```
(`store.go:1886-1904`)

**Journal snapshotting (Layer B, `docs/design/journal-snapshotting.md`,
status "implemented", `engine/snapshot.go`) already shipped and is already
live in the fleet.** Its own header comment states the original diagnosis
plainly:

> A session's durable state is one append-only JSONL journal (store.go),
> and LoadSession rebuilds a session by decoding every record in it,
> building the whole history slice, and repairing it. That is **O(journal
> size)** and grows for the life of the session: on a deployed box a
> single transcript read cost **8 s** ... (`engine/snapshot.go:1-9`)

Layer B bounds the one thing `s.replayedRecords` counts
(`store.go:1898`, `:1913`): the number of records **decoded** past the
snapshot anchor. It does not, and by construction cannot, bound the
`os.ReadFile` at `store.go:1445` or the whole-buffer `bytes.Split` inside
every `scanLogRaw` call at `store.go:1461` and `:1886` — those run before
`startAfter` is even known, and the tail scan still walks every line of
`data` from line 1, merely skipping the JSON `Unmarshal` for lines at or
below the anchor. The snapshot design's own motivating measurement — "not
CPU ... it is I/O + parse ... on the box's slow (gVisor) fs"
(`journal-snapshotting.md:24-26`) — is the I/O this file's own code still
pays in full, snapshot or no snapshot. **This is why the measured cost of
`stream_from=1` on a cold session has not gone away since Layer B
shipped**, and it is the fact this design is built on, not a suspicion:
verified by reading `engine/store.go:1438-1920` directly, not inferred
from the snapshot design doc's own claims about itself.

## 2. What already exists and already solves the adjacent problem

Harness already built, shipped, and documented (all "implemented" on
`origin/main`) the exact machinery an O(window) read needs — for a
**different** endpoint:

- **`engine.SessionIndex`** (`engine/index.go:43`), a slim sidecar fold
  (identity, role, timestamp, tool-call ids — never a message body,
  `index.go:130-145`) keyed on journal length and mtime
  (`LogSize`/`LogModTime`, `index.go:119-128`). `Session.writeRecord`
  updates it and flushes it to disk **synchronously, in the same critical
  section as the append itself**, on every record
  (`engine/store.go:1358` calling `flushIndexLocked`, defined at
  `:1377`). For any session written to since this index shipped, the
  on-disk sidecar is current the instant the session goes idle or is
  evicted — `ReadSessionIndex` (`index.go:624`) then costs one `stat` plus
  one small read (`index.go:614-621`).
- **`engine.ReadMessagePage`** (`engine/messagepage.go:145`) answers the
  newest K messages (or K before a given seq) by scanning the journal
  **backward from EOF** in 64 KiB blocks (`revChunkBytes`,
  `messagepage.go:95`, `scanLogBackward`, `:599`), touching only the tail
  — genuinely O(window), independent of journal length, for the common
  case where the window doesn't cross a compaction boundary
  (`tailPage`, `:273`). It falls back to `foldedPage` (`:451`) only when
  it does, which still decodes skeletons only, never full message bodies.
  This is exactly the mechanism `server/handlers.go`'s
  `handleMessagePage` (`:1356`) already calls for `?before_seq=&limit=`.
- **`live_from`** (`docs/design/live-event-tip-cursor.md`, "implemented")
  already establishes a race-free live-resume cursor — `tipAtStart :=
  s.currentSeq()`, sampled before any session state is read
  (`server/journal.go:1066`) — with a proof (§4 of that doc) that
  generalizes to *any* record excluded from the snapshot a caller is
  about to return, "a plain concurrent race, a compaction splice-timing
  sandwich, or a subagent turn boundary" alike (`live-event-tip-
  cursor.md:200-207`). This proof is exactly what a windowed read needs,
  and it needs no new argument — see §4.2.
- **`seqs`** (`docs/design/transcript-tail-seqs.md`, "implemented",
  already on `origin/main`) already puts each returned message's durable
  ordinal on the wire alongside `stream_from`/`live_from`
  (`transcriptJSON.Seqs`, `server/handlers.go:1287`), specifically so a
  client that only keeps a tail of what this endpoint returns can anchor
  its next backward page on a real seq. It is already exactly the field a
  windowed bootstrap needs to hand back.

None of these four pieces bounds `stream_from=1`'s own cost, because
nothing routes that query parameter to any of them. `handleMessages`
(`server/handlers.go:1218-1226`) actively **rejects** combining
`stream_from` with `before_seq` or `limit` today:

```go
paged := query.Has("before_seq") || query.Has("limit")
if paged && query.Has("stream_from") {
    writeErr(w, http.StatusBadRequest,
        "stream_from cannot be combined with before_seq or limit")
    return
}
```

Boxes' own `docs/design/transcript-backward-pagination.md` (§6, fork (c))
treats this as settled: "they never compose on the same request ... this
document does not decide" — but that document is scoped to *backward*
paging (scroll-up, after a cursor already exists). It never revisits
whether the **first** load, the one that establishes the cursor, could
also be windowed. That is the one open seam this design closes.

## 3. Recommended design

**Relax the rejection for exactly one combination — `stream_from` +
`limit`, never `before_seq`** — and, when a non-resident session receives
it, answer from `ReadSessionIndex`/`ReadMessagePage` (§2's already-shipped
tail read) instead of `LoadSession`. `before_seq` stays rejected alongside
`stream_from`: pairing a cursor-establishing read with an explicit
historical anchor is still two intentions on one request, and nothing
about this design needs to allow it.

### 3.1 Trigger

```go
hasBeforeSeq := query.Has("before_seq")
hasLimit := query.Has("limit")
hasStreamFrom := query.Has("stream_from")

if hasBeforeSeq && hasStreamFrom {
    writeErr(w, http.StatusBadRequest, "stream_from cannot be combined with before_seq")
    return
}
if hasBeforeSeq || (hasLimit && !hasStreamFrom) {
    s.handleMessagePage(w, query, id)
    return
}
if hasStreamFrom {
    limit, ok := intParam(w, query, "limit") // same helper, same rules, handlers.go:1471
    if !ok {
        return
    }
    s.handleTranscriptBootstrap(w, id, limit)
    return
}
```

`limit` absent (the only shape every existing caller sends today) is
byte-for-byte unaffected: `handleTranscriptBootstrap` with `limit == 0`
calls `transcriptSyncedThrough` exactly as `handleMessages` does today.
This is a pure addition to the query grammar, not a redefinition of
anything a current client sends.

### 3.2 The fused window+cursor read

```go
func (s *Server) handleTranscriptBootstrap(w http.ResponseWriter, id string, limit int) {
    if limit > 0 {
        if resp, ok := s.coldWindowedBootstrap(id, limit); ok {
            writeJSON(w, http.StatusOK, resp)
            return
        }
        // Resident, or no usable index/page, or lost the residency race
        // below: fall through to the always-correct path.
    }
    msgs, seq, liveFrom, seqs, ok := s.transcriptSyncedThrough(id) // unchanged
    if !ok {
        writeErr(w, http.StatusNotFound, "no such session")
        return
    }
    writeJSON(w, http.StatusOK, transcriptJSON{
        Messages: marshalMessages(msgs), StreamFrom: seq, LiveFrom: liveFrom, Seqs: seqs,
    })
}

// coldWindowedBootstrap answers a windowed bootstrap for a session this
// process does not hold resident, reading only the journal's tail. ok is
// false for every case the caller should instead answer from
// transcriptSyncedThrough: resident (already cheap in memory, and the
// only path proven correct against s.durableDebt's deferred-write shape),
// no readable index/page (first-ever read of a pre-index session, or a
// genuine I/O error), or a residency transition raced this read.
func (s *Server) coldWindowedBootstrap(id string, limit int) (transcriptJSON, bool) {
    if s.liveSessionObject(id) != nil { // server/live.go:156, O(1)
        return transcriptJSON{}, false
    }
    tipAtStart := s.currentSeq() // sampled first — see §4.1
    page, err := engine.ReadMessagePage(s.opts.SessionDir, id, 0, limit) // beforeSeq<=0: newest page
    if err != nil {
        return transcriptJSON{}, false
    }
    if s.liveSessionObject(id) != nil { // re-check: closes the residency race, see §4.3
        return transcriptJSON{}, false
    }
    seq, liveFrom := s.transcriptCursorLocked(id, page.Messages, tipAtStart) // shared with transcriptSyncedThrough, see §3.3
    seqs := make([]int64, len(page.Messages))
    for i := range page.Messages {
        seqs[i] = int64(page.FirstSeq + i)
    }
    return transcriptJSON{
        Messages: marshalMessages(page.Messages), StreamFrom: seq, LiveFrom: liveFrom, Seqs: seqs,
    }, true
}
```

### 3.3 The one seam: factor the lock section, not the read

`transcriptSyncedThrough`'s body (`server/journal.go:1082-1111`) is
already, in substance, "given a message slice and a `tipAtStart`, mark
each message seen, journal any not yet seen, compute the watermark, take
the max with `tipAtStart`." Nothing in that section reads `sess` again —
it was already written to close exactly the race a second read would
reopen (see the function's own doc comment, `journal.go:997-1005`).
Factor it out unchanged in behavior:

```go
// transcriptCursorLocked is transcriptSyncedThrough's own lock section
// (server/journal.go:1082-1111), unchanged, taking history and tipAtStart
// as parameters instead of computing them from a *engine.Session. Both
// callers — transcriptSyncedThrough (history = sess.History(), full) and
// coldWindowedBootstrap (history = a tail page, bounded) — satisfy the
// same precondition: history is a snapshot no later than tipAtStart's own
// released critical section, and neither reads it again afterward.
func (s *Server) transcriptCursorLocked(id string, history []message.Message, tipAtStart int64) (seq, liveFrom int64) {
    s.mu.Lock()
    defer s.mu.Unlock()
    for i := range history {
        m := history[i]
        if message.IsSyntheticOrphanID(m.ID) || s.isSeenLocked(id, m.ID) {
            continue
        }
        s.markSeenLocked(id, m.ID)
        s.emitDurableLocked(&Event{Type: evtMessage, SessionID: id, Message: &m})
    }
    seq = s.transcriptWatermarkLocked(id, history) // journal.go:1253, unmodified
    liveFrom = tipAtStart
    if seq > liveFrom {
        liveFrom = seq
    }
    return seq, liveFrom
}
```

`transcriptSyncedThrough` becomes a thin wrapper: sample `tipAtStart`,
read `sess.History()`/`PersistErr()`, run the test-only race seam, call
`transcriptCursorLocked`, report the persist error. No behavior changes
for the resident path — this is a refactor, not a rewrite, of the one
function the whole design already trusted.

This is the "fuse the windowed page read with the cursor establishment"
the brief asks for: the fusion is that **one function now serves both a
full in-memory history and a bounded on-disk window**, because the lock
section never actually depended on which one produced its input.

## 4. Cursor semantics and the edge cases

### 4.1 The consistency triple

A caller of the windowed bootstrap gets `(Messages, StreamFrom, LiveFrom,
Seqs)` with the same guarantee `live_from` already carries for the
unwindowed read (`live-event-tip-cursor.md` §4), restated for a window:

- `Seqs[i]` is `Messages[i]`'s real durable ordinal
  (`page.FirstSeq + i`), the same numbering `before_seq`/`limit` already
  uses (`engine/messagepage.go`'s own package comment, line 11-22) — so a
  client can immediately request `before_seq: Seqs[0]` to page backward
  with no wasted overlapping fetch, exactly as `transcript-tail-seqs.md`
  already documents for the full-history case.
- `StreamFrom` is the highest event-journal seq among the window's own
  messages, now durably journaled by `transcriptCursorLocked`'s loop —
  the same watermark computation `transcriptSyncedThrough` already used,
  fed a smaller slice.
- `LiveFrom` is `max(StreamFrom, tipAtStart)`, `tipAtStart` sampled before
  the disk is touched at all (`coldWindowedBootstrap`'s first line). The
  race-close argument in `live-event-tip-cursor.md` §4 is stated over "any
  durable record R excluded from `history`" and its proof depends only on
  R's message having been appended to the session strictly after the
  `history` read this call used — it never depends on `history` being the
  *whole* session, only on it being a snapshot no later than
  `tipAtStart`'s own released critical section. A tail page bounded by
  `ix.LogSize` (`readMessagePageWithIndex`, `messagepage.go:213`) is
  exactly such a snapshot: every message in it was durably on disk at or
  before the moment `ReadSessionIndex` observed that length, which is
  after `tipAtStart` was sampled and released. The proof carries over with
  no new argument.

### 4.2 A message outside the window

Every message strictly older than `Messages[0]` is, by construction,
never journaled by this call — `transcriptCursorLocked` never sees it,
so it is never marked seen. This is a deliberate change in what
`stream_from`'s self-heal guarantee covers: today, a full read pre-warms
`s.seen` for the entire history, so `stream_from`'s self-heal spans
everything. A windowed read only pre-warms the window. This is not a new
kind of gap — it is exactly the tradeoff `live_from` already made and
documented (`live-event-tip-cursor.md`, "What `live_from` does
deliberately give up ... choosing 'no backlog flood' over 'SSE alone will
eventually redeliver everything'"), extended from "the box-global tip" to
"the returned window." A client relying on it for anything older than the
window is using the wrong mechanism: it pages backward instead
(`before_seq`/`limit`), which never touches the live cursor at all.

### 4.3 The residency-transition race

Between `coldWindowedBootstrap`'s first `liveSessionObject` check and its
second one, a concurrent `claimForPrompt` could load the session and start
a turn. The second check catches this and returns `ok = false`; the caller
falls through to `transcriptSyncedThrough`, which is now cheap (the
session is resident — `s.lookupSession`'s `liveSessionObject` branch
returns immediately, no `LoadSession` call). No new synchronization
primitive is needed: the check-read-check-again shape is the same one
`engine.ReadMessagePage` itself already uses for its own race
(`readMessagePageWithIndex`'s `fi.Size() < ix.LogSize` check,
`messagepage.go:209`, and `pageError`'s second check, `:253`) — "any
doubt, fall back to the answer already proven correct" is this
codebase's standing rule (`engine/snapshot.go`'s own rule 5, "a snapshot
bug degrades to slow, never wrong").

A narrower sub-race — residency begins strictly between the second check
and `ReadMessagePage`'s own read — is closed the same way
`ReadMessagePage` closes concurrent writer overlap generally: the read is
bounded by `ix.LogSize`, a length observed before any conflicting write,
so it either sees a consistent prefix or reports `ErrStaleMessagePage` and
retries once (`ReadMessagePage`, `messagepage.go:145-154`); it is never
handed bytes a concurrent append is still writing.

### 4.4 A compaction sandwich

`transcriptWatermarkLocked`'s own "two-emit sandwich window" case
(`server/journal.go:1229-1251`) requires a compaction actively landing
*during* the read — which requires an active turn, which requires
residency. A truly cold session (no resident `*engine.Session`) cannot be
mid-compaction. The only way this design could see a compaction sandwich
is the same residency-transition race §4.3 already closes: the moment a
concurrent prompt makes the session resident, the fast path bails out to
`transcriptSyncedThrough`, which already carries the exact proof and test
(`TestTranscriptWatermarkLocked_CompactionSummarySandwich`) for that case.
This design adds no new compaction handling because it never runs the
watermark computation over a compacting session at all.

### 4.4a An already-landed compaction (not a sandwich — a correctness bug
found by review, fixed)

§4.4 above rules out an ACTIVE compaction racing this design's read. It
does not, on its own, rule out an ALREADY-LANDED one: a compaction that
completed before this call started, whose summary message simply sits at
an ordinal *older than the returned window*. An Opus review of the first
version of this change found that `transcriptWatermarkLocked`, called
unchanged with the tail window as `history`, could not tell that case
apart from the live sandwich it exists to protect — both look identical
to its loop: "a compaction summary's `evtMessage` record is in `s.journal`
but absent from `history`." For a windowed read, that condition is true
for EVERY compaction summary older than the window, always, not merely
during a race — `s.journal` is this **process's** own cumulative event
log, populated by any earlier full `stream_from=1` read of this session
or by `Server.reconcile`'s startup replay of whatever already sits in
`SessionDir`, and neither has anything to do with whether a compaction is
happening *now*. The old code applied the sandwich's cap anyway, dragging
`stream_from` down toward `summaryCeiling - 1` — in the worst case,
toward the very start of the session — silently reopening exactly the
backlog flood `live_from` exists to prevent (§1, §4.1), scaling with
session length and worst on the long, already-compacted, cold sessions
this design targets. `live_from` itself stayed correct throughout (it is
`max(seq, tipAtStart)`, and `tipAtStart` never depends on this
computation), so the failure was an over-delivery on `stream_from`
resume, not a gap — but it directly broke this section's own "same
watermark computation, fed a smaller slice" claim (§4.1) and
`transcriptJSON`'s only-additive-narrowing contract (§5).

**The fix.** `transcriptWatermarkLocked` takes a `windowed bool` parameter
(threaded through `transcriptCursorLocked`, `false` from
`transcriptSyncedThrough`, `true` from `coldWindowedBootstrap`). When
`windowed` is true, the function skips `summaryCeiling` and
`pendingCeilings` entirely — no cap fires, ever — and answers with
`highest` alone: the greatest journaled seq among messages the window
actually returned. This is sound, not merely convenient, because
`coldWindowedBootstrap` only reaches this call after re-confirming
non-residency (§4.3's own second `liveSessionObject` check): a cold
session cannot be mid-compaction (§4.4's own argument), so no summary
this call could ever see is excluded by a race. And because the window is
always a "newest page" (`beforeSeq<=0`, so `page.LastSeq` equals the
session's current total), the single highest-seq message across the
**whole** session — the true tip `stream_from` must report — is *always*
inside the window; `highest` alone already equals what a full read would
compute, with nothing left for a ceiling to legitimately lower. The
unwindowed path (`windowed == false`) is untouched: every existing
sandwich, race, and pending-ceiling test still exercises the same code,
unconditionally.

See `TestColdWindowedBootstrap_StreamFromParityAfterSeededJournal` and
`TestColdWindowedBootstrap_ParityWithFullRead_CompactedPartialWindow`
(`server/transcript_bootstrap_window_test.go`) for the regression tests:
both seed `s.journal` with an already-landed compaction summary (one via
an explicit prior full read, the other via `Server.reconcile`'s startup
replay of a pre-existing `SessionDir`), then assert the windowed path's
`stream_from`/`live_from` exactly match the full path's — red-verified
against the pre-fix code, which collapsed `stream_from` toward the
session's start in both cases.

### 4.5 Resident sessions

Unaffected. `coldWindowedBootstrap` bails out immediately for a resident
session and defers entirely to `transcriptSyncedThrough`, unmodified.
Windowing an in-memory `sess.History()` (a trivial tail slice before
marshaling) is a real, separable optimization for wire size on a
long-lived resident session, but it is not the fix this design is for —
a resident session never pays the `LoadSession` cost §1 measures, since
`sess.History()` is already an in-memory read. Left as a clearly-optional
follow-up, not bundled here.

### 4.6 Managed child sessions

No special-casing. A child (subagent/task) session is stored, indexed,
and loaded through the exact same `SessionDir`, `SessionIndex`, and
`ReadMessagePage` machinery as a root session — `SessionIndex` itself
carries the child's own lineage fields (`TaskParentID`, `TaskAgentType`,
`TaskDepth`, `index.go:61-69`) precisely because it is folded the same
way. `handleMessagePage` already serves child sessions with no branch on
lineage; this design adds none either.

### 4.7 Stale or absent index

`ReadSessionIndex` (`index.go:624`) already handles this: no sidecar, a
version mismatch, or a journal that grew or shrank since the sidecar was
written all trigger one slim refold (skeleton fields only, no message
bodies) and a write-back, so every read after the first is the fast path
(`index.go:611-621`). The refold itself still does one `os.ReadFile` of
the whole journal (`index.go:671`) — no faster than `LoadSession`'s own
read for that one call — but it is a one-time cost per session per
sidecar loss, not a recurring one, and it is strictly cheaper in CPU (no
message-body decode, ever) even on that first call. Given
`flushIndexLocked` runs on every write since this index shipped
(`store.go:1358`), the sidecar already exists and is current for
essentially every session in the fleet today; this is the same fallback
shape `ReadMessagePage` already relies on for `before_seq`/`limit`, not a
new one this design invents.

### 4.8 Empty or very small sessions

`MessagePageWindow` (`messagepage.go:112`) already returns an empty page
(`hi < lo`) for a session with no durable messages; `page.Messages` is
`nil`, `transcriptCursorLocked` runs its loop zero times, and `seq` falls
back to 0 exactly as `transcriptWatermarkLocked`'s own doc comment already
specifies for that case (`journal.go:1176-1179`). No new zero-length
handling is needed anywhere in this design.

## 5. The CP↔harness contract change

**The wire response shape does not change at all.** `transcriptJSON`
(`Messages`/`StreamFrom`/`LiveFrom`/`Seqs`) already carries everything a
windowed bootstrap needs; `Seqs` in particular already exists for exactly
this purpose (§2). The only contract change is a **query-grammar
relaxation**: `?stream_from=1&limit=N` becomes a legal, meaningful
request instead of a 400. No existing caller is affected — the
combination was rejected outright before, so nothing today depends on it
erroring.

Boxes' own side (`internal/api/journal.go`'s `journalBounds`) already
special-cases this exact combination defensively:
`transcriptQuery()` (`journal.go:133-144`) sends `bounds.query()` alone,
never `stream_from`, whenever `paging()` is true (`Limit > 0 ||
BeforeSeq > 0`) — a comment there cites this exact rejection by name.
The follow-up change on that side (out of scope here, described for
completeness) is to let an **unpaged, byte-budgeted** bootstrap read also
carry a modest message-count `Limit` (matching `console_bootstrap.go`'s
existing `maxBytes` intent, not replacing it — recommend
message-count over byte-bounded, matching boxes' own already-decided
precedent in `transcript-backward-pagination.md` §6(b)) and send it
alongside `stream_from=1`. `budgetTranscript`'s byte trim keeps running
client-side afterward as the precise safety net, now over a window that
is already close to the target size instead of the session's entire
history.

## 6. Expected performance

The dominant cost `LoadSession` pays for a cold session is `os.ReadFile`
of the whole journal plus a whole-buffer `bytes.Split` twice over
(`countJournalRecords`, then the tail `scanLogRaw`) — O(journal bytes),
independent of the snapshot anchor (§1). `ReadMessagePage`'s tail path
touches only `revChunkBytes`-sized blocks (64 KiB, `messagepage.go:95`)
working backward from EOF until the window is filled — O(window bytes),
independent of journal length, for the common (non-compacted-boundary)
case. For the sessions that motivated this measurement — the fleet's
longest production session was 1.4 MB (`messagepage.go:6-9`'s own cited
figure) — a 100-message window (`DefaultMessagePageLimit`,
`messagepage.go:59`) reads a small, bounded fraction of that regardless of
how long the session grows afterward. The mechanism is already proven at
this scale: it is the identical read `handleMessagePage` already answers
for `before_seq`/`limit`, and boxes' own pagination design already
measured it as "cheap by construction"
(`transcript-backward-pagination.md` §0.2). This design does not
introduce a new fast path to validate; it routes a second entry point to
one already carrying production traffic.

## 7. Alternatives considered

**(b) Make `LoadSession` itself fast enough for every caller** (i.e., make
the full-replay path index/snapshot-backed all the way down, so the
existing `stream_from=1` branch stays untouched but becomes cheap on its
own). Rejected. Two independent reasons, not one:

1. It is already the design Layer B tried, and §1 shows exactly why it
   falls short: the snapshot anchor bounds *decode*, not the unconditional
   `os.ReadFile` and whole-buffer line split that run before the anchor is
   even consulted. Closing that gap for `LoadSession` specifically would
   mean seeking to a byte offset instead of reading the whole file — which
   is precisely what `SessionIndex`/`ReadMessagePage` already do, for a
   reason `LoadSession` cannot share: `LoadSession` must produce a fully
   mutable, resident `*engine.Session` capable of accepting the next turn,
   which needs the complete fold state (compaction history, tool-result
   cache, prompt queue, goal state — every field `sessionSnapshot`
   enumerates, `engine/snapshot.go:112-193`), not a bounded tail. A
   read-only bootstrap needs none of that.
2. Building a second, offset-seeking read specifically for `LoadSession`
   would duplicate `tailPage`/`foldedPage`'s own fold — exactly what their
   doc comments already forbid ("no second, subtly different
   implementation of a fold this repository forbids," `store.go:1874-1878`
   and `messagepage.go:447-450`). The existing index-tail mechanism is the
   one true fold; reusing it, as this design does, is the smaller and
   safer change.

**(c) A control-plane–side two-call bootstrap**: page the tail
(`before_seq`/`limit`, already fast) as one call, then a second call to
arm the live cursor. Rejected: this reopens exactly the race
`transcriptSyncedThrough`'s own doc comment names as the reason
`stream_from=1` exists at all — "the tail-load versus live-stream race the
console's duplicate-render bug traces to" (`journal.go:989-994`). A
message appended between the two round trips lands in neither the page
(already returned) nor whatever the second call's cursor covers, unless
the server holds state across the two requests to close the gap — at
which point it has reinvented this design's single locked read, across an
HTTP boundary instead of inside one function, for no benefit.

## 8. Does journal snapshotting still matter here?

Yes, but for a different reader. §1 already shows Layer B does not bound
this design's target read — the index-tail path bypasses `LoadSession`
entirely for the common case. Layer B remains the right mechanism for the
read this design explicitly leaves alone: `claimForPrompt`'s
`LoadSession` call when a cold session's first prompt arrives, and the
`transcriptSyncedThrough` fallback this design's own residency-race and
stale-index cases still take. Both of those genuinely need the full
resident `Session`, and Layer B is what bounds their decode cost once
they run. The two mechanisms are complementary, not overlapping: the
index bounds what a *read* has to touch; the snapshot bounds what a
*replay* has to decode when a full one is unavoidable. Nothing here makes
Layer B redundant, and nothing in Layer B makes this design unnecessary —
each already covers the case the other does not.

## 9. Why this is elegant and fast

The fix is nearly free of new mechanism. Every load-bearing piece —
`SessionIndex`, `ReadMessagePage`, the `tipAtStart` race-close proof,
the `Seqs` field — already ships on `main`, already serves production
traffic on a sibling endpoint, and already has its own tests. The one new
piece of logic is `coldWindowedBootstrap`'s residency check and its
recheck (four lines around an existing, unmodified read), and
`transcriptCursorLocked` is a pure extraction — same code, same lock
section, a parameter instead of a hard-coded `sess.History()` call. No
wire schema changes. No new endpoint. No new cursor type. The single seam
the brief asked for is real: one function now answers the cursor question
for either a resident session's full history or a cold session's tail
window, because the question was never about *how* `history` was
produced, only about *when* it was produced relative to `tipAtStart`.
