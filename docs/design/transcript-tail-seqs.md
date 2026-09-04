# Transcript tail seqs

Status: implemented.

## 1. The problem, as reproduced

The boxes console's initial pane load is a byte-budget tail: it reads a
session's whole message history through `GET
/session/{id}/message?stream_from=1` (the `Transcript` envelope — see
`server/journal.go`'s `transcriptSyncedThrough`), then trims it client-side
to the most recent messages that fit a byte budget
(`meetneptune/boxes`'s `internal/api/transcript_truncate.go`,
`budgetTranscript`). Harness never budgets this read itself; it always
answers the whole history.

Scrolling up in that pane asks for older messages via `GET
/session/{id}/message?before_seq=N&limit=K` (`server/handlers.go`'s
`handleMessagePage`). A real anchor needs `N` to be the durable journal seq
of the oldest message the pane already shows — but `message.Message`
(`message/message.go`) carries no seq of its own, and the byte-budget tail
that produced the pane's oldest message came from the *other* endpoint,
which answered a `stream_from`/`live_from` cursor, never a per-message one.
So the FIRST "load older" request from a freshly opened pane had no real
seq to anchor on, and asked for harness's own "the newest page" convention
(`before_seq=0`) instead — re-fetching the exact page already on screen.
Only the SECOND "load older" request, which pages from a real
`first_seq` a genuine page response had already supplied, reached further
back.

## 2. The fix

`transcriptJSON` (`server/handlers.go`) gains a fourth, additive field:

```json
{"messages": [...], "stream_from": 123, "live_from": 130, "seqs": [41, 42, 43, 44]}
```

`seqs` is parallel to `messages`: each entry's DURABLE MESSAGE ORDINAL, in
the same order — the SAME per-session numbering `before_seq`/`limit`
itself is defined in terms of (`engine/messagepage.go`'s own doc comment:
"a message's 1-based ordinal in the session's durable message sequence
... with each compact record's fold applied"). `0` for an entry with no
durable ordinal of its own (a `message.IsSyntheticOrphanID` load-time
repair — see `messageDurableOrdinals`' own doc comment, `journal.go`).

This is deliberately NOT the box-global event-journal seq
`stream_from`/`live_from` report (`s.seq`, `emitDurableLocked`) — an
earlier revision of this change sampled that value instead, and it is
WRONG for this purpose even though it is also monotonic and also
per-message: that seq space is shared by every session and every durable
event type this session's id has ever journaled under (`evtSessionCreated`,
`evtSessionStatus` on each turn's busy/idle transition, `evtModel`, ...),
so it runs ahead of the per-session message ordinal by an amount that
grows with every turn and every child session's own interleaved activity.
A client that sent that inflated value back as `before_seq` almost always
named a point PAST the session's own message total, which
`MessagePageWindow` clamps back down to the newest page — silently
re-fetching the tail, the exact bug this field exists to fix, just moved
one seq-space over. `messageDurableOrdinals` computes the right space
instead: a 1-based count over `history`'s own entries (skipping a
synthetic one), which already matches `engine/messagepage.go`'s own
fold-adjusted count because `Session.Compact`'s `spliceCompact`
(`engine/compact.go`) already splices a compaction summary into
`s.history` in place of the range it replaced — the identical fold, not a
second implementation of it.

A caller that budgets `messages` down to a shorter tail can now look up the
ordinal of whichever message survived as the OLDEST kept one, by its `id`,
and use that as `before_seq` on its first "load older" request — a real
anchor, with no wasted overlapping fetch. `stream_from`, `live_from`, and
`/event` are unchanged; a client that reads only those is unaffected, and
`seqs` is absent (`omitempty`) for nothing here changing shape on an old
client's own request.

## 3. What this does NOT do

It does not make harness budget the byte-budget tail itself, and it does
not change the `before_seq`/`limit` page endpoint's own envelope
(`first_seq`/`last_seq`/`total`/`has_more`) at all — that mechanism
(`docs/design/transcript-backward-pagination.md`, in `meetneptune/boxes`)
already answers a real anchor for every page AFTER the first one. This
closes the one gap before it: the FIRST page, computed from a tail harness
never bounded in the first place.

See `meetneptune/boxes`'s own `docs/design/transcript-scroll-first-load.md`
for the client-side half: how the byte-budget trim picks the kept tail's
first message and turns its `seqs` entry into a real `before_seq`.
