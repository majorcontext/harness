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
{"messages": [...], "stream_from": 123, "live_from": 130, "seqs": [118, 119, 121, 123]}
```

`seqs` is parallel to `messages`: one durable journal seq per entry, in the
same order, `0` for an entry with no durable seq of its own (a
`message.IsSyntheticOrphanID` load-time repair — see
`transcriptSyncedThrough`'s own doc comment for why that one case is
deliberate, not a gap). It is sampled from `s.journal` in the SAME locked
section as `stream_from`/`live_from`, after `transcriptSyncedThrough`'s own
journaling loop has run, by `messageSeqsLocked` — so a message this very
call durably journals for the first time already has an entry for the scan
to find.

A caller that budgets `messages` down to a shorter tail can now look up the
seq of whichever message survived as the OLDEST kept one, by its `id`, and
use that as `before_seq` on its first "load older" request — a real anchor,
with no wasted overlapping fetch. `stream_from`, `live_from`, and `/event`
are unchanged; a client that reads only those is unaffected, and `seqs` is
absent (`omitempty`) for nothing here changing shape on an old client's
own request.

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
