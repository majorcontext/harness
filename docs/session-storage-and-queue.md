# Session storage, reads, queue, and processes

This document describes session persistence, paging, prompt queues, and
managed-process invariants. Read the matching section before changing those
paths.

## Session metadata index

`GET /session` and `GET /session/{id}` do not replay a session journal. Each
session log has a sidecar `<id>.index.json` holding one
`engine.SessionIndex`. The index carries every wire `Session` field with a
durable source: timestamps, model, effort, workdir, parent session, task
lineage, message count, usage, durable goal state, queue depth, and
compaction counters.

Before the index, a read of a non-live session called `LoadSession`. That
call decodes every message body and rebuilds the whole history. The handler
then reported a dozen scalars and dropped the rest. The list endpoint paid
that cost once per non-live session (workstream 1 in the
`console-read-path.md` design from the meetneptune/boxes repository).

The index is a fold of the journal (`engine/index.go`). Three rules keep it
honest.

**It folds records, not memory.** `Session.writeRecord` folds every record
it appends. `ensureLog` folds the header records it writes directly, which
is the one write that does not pass `writeRecord`. Memory is never the
source: `EnqueuePromptDurable` writes its record before it mutates the
queue, so a fold of memory at that instant would disagree with the log it
claims to summarize.

**It is a cache, never an authority.** `ReadSessionIndex` serves a stored
index only when three checks pass: a checksum over the stored bytes, the
journal's byte length, and the journal's modification time. Anything else
refolds from byte 0. There is no repair path, so no repair path can be
wrong. The checksum catches a torn read — both writers replace the sidecar
in place, and a reader in another process can otherwise mix old bytes with
new ones that parse. Length and modification time are a staleness key, not a
proof: they rest on the journal's own contract of one writer and append-only
writes.

**It reports what a full load reports.** `Messages` and `LastActivityAt` run
through `message.ResolveOrphanToolCalls`, over a skeleton of ids, roles, and
tool-call ids. A crash between a tool call and its result therefore counts
the same on both paths. `DurableMessages` counts the records alone, for a
reader that must map a message to a byte offset; paginated message reads are
numbered against it.

Some journals cannot be answered by a fold at all. A legacy header records
no workdir, and a crash can tear away the initial model record. `LoadSession`
answers those from the loading `Config`; a fold has no `Config`.
`SessionIndex.Complete` reports the difference, and
`Server.coldSessionJSON` uses the authoritative load path for such a
session.

Three folds have real state machines. Each has one implementation, shared by
`LoadSession` and the index: `applyCompactRecord` (`compact.go`),
`applyGoalRecord` (`store.go`), and `promptQueueFold` (`queue.go`).
`engine/index_test.go`'s oracle test pins every index field against a full
`LoadSession`.

A refold is far cheaper than a full `LoadSession`, and a current index is
cheaper again. Write-through costs one marshal and one rewrite per record.
The sidecar never gets an `fsync`: losing it in a crash costs one refold.

`server.Options.Plugins` supplies a session's plugins to a cold read.
Plugins are process configuration, not durable session state, and a cold
read has no `Session` to ask.

## Journal snapshotting

`LoadSession` does not always replay a whole journal. Beside the log sits
`<id>.snap`, a **seq-anchored checkpoint**: the fold-produced state as of
journal line N, plus a CRC-32 and a format version. Recovery loads the
snapshot, applies the session HEADER record (line 1, always — it is what
carries `created_at`, workdir, and task lineage, whose restore rules turn on
absent-versus-present and are not worth reproducing twice), and then replays
only lines `> N`. The tail scan reads through `scanLogRaw`, so a covered
record is never DECODED: skipping the decode of a message record's whole
part tree is where the saving is. Design:
docs/design/journal-snapshotting.md.

The snapshot schema is EXPLICIT (`sessionSnapshot`, `engine/snapshot.go`),
never `json.Marshal` of a `*Session`: every field but `ID` is unexported,
and the config carries live callbacks, a `SessionManager` pointer, and open
file handles that must never be serialized. The rule for what belongs in it
is **exactly what the folds reconstruct** — a fold added without a matching
snapshot field is silently dropped, which is what
`TestSnapshotCarriesEveryFoldedField` and the replay-equivalence tests pin.
Two exclusions are deliberate: header-derived state (replayed instead), and
`turn`/`lastSystem`, which have NO durable source at all — capturing them
would make a snapshot-loaded session disagree with a full replay of the same
journal and make an observable field depend on whether a snapshot happened
to exist.

The invariant is `state(snapshot@N) + replay(N+1..head) ≡
full-replay(0..head)`. Every doubt falls back to a full replay: no snapshot,
a torn one, a checksum mismatch, a wrong version, another session's id, a
`seq` ahead of the journal head, or a journal whose first record is not a
session header. **Slower, never wrong.** The journal is never truncated;
snapshots are derived and can be deleted at any time.

**The trigger is at the APPEND BOUNDARY, never inside `writeRecord`.** A
snapshot pairs a memory image with a journal position, and inside
`writeRecord` the two do not yet agree: some callers persist their record
BEFORE applying their own memory mutation (`EnqueuePromptDurable`,
deliberately), so a capture there would anchor past a record whose effect
memory has not applied — and the reload would skip that record and lose the
effect forever. `appendWithUsage` (after `persistMessage`, still under
`s.mu`) and the on-idle trigger (`runAgenticLoop`'s defer, and
`ReleaseFiles` on eviction) are the two boundaries where the caller has
completed both halves.

The OPPOSITE direction is guarded by `snapshotSafeLocked`:
`SessionManager` splits some mutations into an in-memory half and a
DEFERRED durable half (`appendMemoryOnly`/`persistAppendedMessage`,
`enqueueTaskNotificationMemoryOnly*`/`persistQueuedTaskNotification`,
`queueRecordDeferredLocked`), so memory can be AHEAD of the journal. A
snapshot taken in that window carries the mutation AND leaves its record in
the tail, and the reload applies it twice — a duplicated message, or a
child-completion notification the parent renders to the model twice.
`Session.durableDebt` counts the outstanding halves; a capture is refused
while any is open, which merely postpones the snapshot to the next
boundary. The debt clamps at zero: a leaked increment stops this session
snapshotting (degrading to today's full replay), where a negative count
would ARM a capture in exactly the unsafe window.

`Config.SnapshotEveryRecords` is the cadence. Zero — the engine zero value —
disables snapshot WRITING, so a bare embedder-built `engine.Config` keeps
the pre-snapshot behavior; the config/CLI layer supplies the product default
of 64 (`snapshot_every_records`), the same unset-versus-explicit-zero split
`prompt_retries` uses. READING a snapshot is never gated on it: recovery is
a property of the files on disk, not of the loading Config. A snapshot write
is background, coalesced (one in flight per session), and atomic (temp →
fsync → rename), and its failure lands in `lastSnapshotErr`, never
`lastPersistErr` — a snapshot is derived acceleration, not a durability
promise.

A load that takes the snapshot path marks the metadata-index fold BROKEN
rather than building a partial one: the index summarizes EVERY record, and
this load deliberately did not see most of them. The index is a cache with
no repair path, so a reader that finds none refolds and `ensureLog` re-seeds
the fold from the journal on this session's next write. Snapshotting the
index fold itself is the obvious follow-up; nothing may guess at it.

## Paginated message reads

`GET /session/{id}/message?before_seq=N&limit=K` answers one bounded page of
a session's messages, read from the journal's tail. The unparameterized call
is unchanged, byte for byte: no `before_seq` and no `limit` still returns the
bare array of the whole history every existing caller expects. A request that
names either parameter gets a `MessagePage` envelope instead — `messages`,
`first_seq`, `last_seq`, `total`, `has_more` — because a client that pages
needs the page's position and a client that does not must not have to learn a
new shape. A console loads the tail and pages older messages in on scroll
(workstream 2 and directive 1 in the `console-read-path.md` design from the
meetneptune/boxes repository). Before this, every console open transferred
the whole transcript.

**Seq is an ordinal over the DURABLE message sequence**: message records in
log order, with each compact record's fold applied (the folded range replaced
by that record's summary). `SessionIndex.DurableMessages` counts that same
sequence, so the newest message's seq equals it. `SessionIndex.Messages` can
be larger — it also counts the synthetic tool results
`message.ResolveOrphanToolCalls` derives — and a derived message has no
record, so it has no seq and no page carries it. This definition is what
makes a bounded read possible: an ordinal over durable records can be counted
backwards from the end of a file, while an ordinal over a materialized
history cannot be known without materializing it.

`engine.ReadMessagePage` (`engine/messagepage.go`) serves a page two ways,
and both are numbered by the same index:

- The **tail walk** reads `revChunkBytes` blocks backwards from
  `SessionIndex.LogSize` and numbers message records down from the total. It
  touches only the tail however long the journal is. It gives up the moment
  it meets a compact record, because the messages a fold KEPT sit in the log
  between the folded range and the compact record itself — undoing that
  backwards would be a second, subtly different implementation of a fold.
- The **fold path** then reuses `indexFold` — the same forward fold and the
  same compact-range occurrence selection — to learn which messages occupy
  the requested seqs. `indexFold` carries each surviving message's journal
  record ordinal beside its skeleton; `foldedPage` reads back by that ordinal,
  never by message ID alone. This matters for hand-written or externally
  assembled journals that repeat an ID: one occurrence can be compacted away
  while another survives, and the surviving sequence entry must decode the
  surviving record. The path costs one slim pass (ids, roles, and ordinals,
  never message bodies), which is still three orders of magnitude below
  materializing the history.

The scan is bounded by `SessionIndex.LogSize`, never the file's current size,
so a turn appending records while a page is read cannot renumber that page
under it.

Two properties are deliberate. A page carries durable messages **verbatim**:
it never runs `message.ResolveOrphanToolCalls`. That repair exists to keep a
provider REQUEST valid, this endpoint builds no request, and fabricating a
tool failure in a read view has real production history (see `Server.lookup`'s
doc comment: a healthy child's in-flight tool call rendered as failed in the
console for as long as it kept running). And compaction **renumbers** — a fold
replaces N messages with one summary, so every later seq shifts down by N-1.
A client paging across a compaction can see one page overlap another; message
ids are stable and are the way to de-duplicate.

A page is read from the durable records even when the session is resident, so
one seq definition covers both cases: a resident history can carry messages
the log does not (load-time repairs, recovery's memory-only closers), and
numbering those would give one message two different seqs depending on
residency. A session with no journal at all — created through the API and
never prompted — falls back to its resident history, which for such a session
IS the durable sequence.

## Prompt queue

`POST /session/{id}/prompt_async` against a session already busy (another
prompt, or a running goal loop) no longer 409s — it queues. The prompt is
enqueued durably (`engine.Session.EnqueuePrompt`, persisting a `prompt.queued`
record and assigning a session-monotonic ID) synchronously, before any
response is written — the same enqueue-durable-before-202 shape `RegisterGoal`
already uses for goals, closing the accept-vs-lose race structurally. The
response is 202 either way: `status: "started"` when a turn is now running for
this request's own prompt (an idle claim against an EMPTY queue, or a
freed-slot retry that happens to win and dispatch this same prompt), or
`status: "queued"` (carrying the current depth) when it is durably waiting —
including the idle-claim case where the queue is already non-empty (a
restart refold, or any other drain gap that ever left a prompt stranded):
`handlePrompt` enqueues the incoming text behind whatever is already waiting,
then dispatches the queue's HEAD — not necessarily this request's own text —
into the run slot it just claimed, so a fresh arrival can never cut the
line ahead of prompts already queued. The workdir-held-by-another-session 409
is unchanged — only same-session busy gets queue semantics.

The queue drains FIFO, by queue ID, at every run-slot release, with no
exceptions: `runPrompt`'s, `runGoal`'s, and `handleCompact`'s tails all call
`maybeDispatchQueued`, which claims the freed slot, dequeues the head
(`reason: "delivered"`), and spawns it as a normal prompt turn — whose own
tail repeats the check, so the whole queue drains one turn at a time before
anything else gets a look. `handlePrompt`'s own claim-success path (previous
paragraph) is the one non-tail drain site: an admission-time head-dispatch
for the idle-with-non-empty-queue case, closing the gap a tail-only drain
would otherwise leave open between "session goes idle with a queue still
non-empty" and "the next prompt/goal/compact activity happens to touch it."
This is also where
**queue beats goal auto-arm**: `runPrompt`'s and `handleCompact`'s tails call
`maybeDispatchQueued` *before* `maybeAutoArmGoal` (see above), so a prompt
sitting in the queue when a turn or a compact call ends is dispatched first —
direct user input outranks the background objective — and the goal only
auto-arms once the queue is empty.

**Delivery granularity is per tool-call boundary, not per turn.** Inside
`Session.Prompt`'s agentic loop (`engine/engine.go`), the instant a
tool-result message is appended — after the model made one or more tool
calls and before the next provider request in that SAME turn — the loop
drains the ENTIRE queue, FIFO, in one locked op (`DequeueAllPrompts
("injected")`) and appends the drained batch as a single, durable user
message: the same labeled "OPERATOR MESSAGES" block template
(`operatorMessagesBlock`, `engine/queue.go`, shared by every drain site so a
batch renders identically apart from one parameterized word — this
call site passes `operatorContextTask`, so its header says "continue the
task", never "continue the goal", even when this drain happens to fire
inside a goal loop's worker turn; only goal.go's own turn-boundary drain
below passes `operatorContextGoal`). This only ever
APPENDS — never rewrites an earlier message — so a provider's prompt-cache
prefix stays intact, the same principle the managed-processes ephemeral
status block below relies on, except this message is a REAL, durable
delivery, not a disposable status line. A turn that ends WITHOUT any tool
call never reaches this drain point at all (the model's own end-of-turn
return precedes it), so that path — and anything still queued when it
happens — is left entirely to the mechanisms below. Because `PursueGoal`'s
worker turns run through this exact same `Prompt` loop
(`promptTurnWithRetry`), goal loops inherit tool-call-boundary injection
automatically, with no separate wiring: a prompt queued while a goal's
worker turn is mid-tool-call is delivered inside that SAME worker turn —
matching Claude Code's mid-turn steering granularity — rather than waiting
for the goal's own turn boundary described next.

`PursueGoal` keeps a second, complementary drain at its own turn boundary:
at the top of every turn (the same `snapshotGoal` boundary #77's
condition-update snapshot uses, and before that turn's own tool-call-boundary
drain above has any chance to run) it drains the *entire* queue, FIFO —
catching anything still queued from a turn that made no tool calls at all, or
that arrived in the gap between one turn ending and the next one's snapshot —
and prepends it to that turn's directive as the same labeled "OPERATOR
MESSAGES" block (`operatorMessagesBlock`, `operatorContextGoal` — so its
header says "continue the goal"), ahead of — never replacing — the
ordinary condition/guidance text. The evaluator's condition string is
unchanged by this — it is built from the condition alone, never from the
block or the turn's rendered directive — so goal injection judges only the
goal there; the evaluator's separate transcript field does render the full
history, so it does see the block once the worker turn that received it has
run. Every drained prompt journals its own `prompt.dequeued(injected)` record
before the turn's directive is even built, so it counts as delivered at that
point even if the turn's outcome later turns out stale and gets discarded —
an injected prompt is never re-queued, at either drain site. This means an
abort (`POST /abort`) or a goal clear (`DELETE /session/{id}/goal`) racing a
goal turn boundary consumes an entire just-injected batch at once: every
prompt the boundary drained is already journaled `dequeued(injected)` before
the worker turn even starts, so a turn that gets cancelled or whose outcome is
later discarded as stale still loses all of them together — several operator
messages, not just one — the same exposure class an ordinary in-flight prompt
already has, just multiplied across the whole drained batch. The two drain
sites can never double-deliver the same prompt: `DequeueAllPrompts` is one
atomic, locked pop of the whole queue, so whichever site runs first against a
given prompt is the only one that ever sees it.

Every enqueue/dequeue is a durable record — `prompt.queued` and
`prompt.dequeued`, the latter carrying a `reason` of `"delivered"` (idle
drain), `"injected"` (tool-call-boundary or goal-turn-boundary injection —
both drain sites share the reason, see above), or `"cleared"` (see below) —
journaled and emitted (`EventPromptQueued`/`EventPromptDequeued`) under
`s.mu` in the same critical section, mirroring `RegisterGoal`/`ClearGoal`
exactly. Dequeue always journals *before* the text enters any turn, so a crash
between that journal write and the dispatched turn's completion cannot
double-deliver — the prompt is simply gone from the queue on replay, the same
exposure any in-flight prompt already has today. **Boot never auto-dispatches
a resumed queue**: `LoadSession` folds `prompt.queued`/`prompt.dequeued`
records back into the exact undelivered set, `GET /session`'s `queued` count
reflects it immediately, and it sits there until the next natural drain
trigger (an idle prompt, the next tool-call boundary inside a running turn, or
a goal loop's next turn boundary) — the same settled boot rule goals follow.
`DELETE /session/{id}/queue` is the one explicit clear surface: it journals
`prompt.dequeued(cleared)` for every pending item then 204, idempotent on an
empty queue, and never touches a currently running turn — `POST /abort` is
unrelated and does not touch the queue either way (it only cancels the
in-flight turn's context).

A queued prompt carries **text and image attachments**: `QueuedPrompt{ID,
Text, Blobs}`, persisted together on the `prompt.queued` record
(`promptRecord.Blobs`), so an image sent while a turn was running still
reaches the model when the queue drains — across a process restart included.
The bytes ride only on `prompt.queued`, never on `prompt.dequeued`, which
names its entry by ID. Every drain site delivers both halves: idle dispatch
and the tool-call boundary append the blobs as `Blob` parts of the user
message they build, and the goal loop's turn-boundary injection passes them
into its worker turn. The rendered `OPERATOR MESSAGES` block is text, so each
prompt that carries attachments announces them (`[N image attachment(s)
attached below]`) — the marker names which numbered message the picture
belongs to.

One v1 limit is still deliberate, not a gap: **a per-request `model` override
is silently dropped when the prompt is queued** — there is no slot in
`QueuedPrompt` to carry it through to a future drain, so a caller that needs a
model swap to take effect must re-issue the request once it is confirmed
`started`.

`POST /session/{id}/enqueue` (docs/plans/2026-07-21-durable-enqueue.md) is
`prompt_async`'s durable, idempotent sibling for a caller whose own upstream
ack rides on this call succeeding — an inbox poller or coordinator relay,
not an interactive client. `Session.EnqueuePromptDurable` extends
`EnqueuePrompt` with three properties the plain path deliberately lacks:
write-ahead durability (the `prompt.queued` record is written and, in the
default `session_sync: "fsync"` mode, *fsynced* before any in-memory
mutation or response, so a 2xx is an honest attestation rather than a
best-effort ack — a write/fsync failure returns 500 "enqueue not durable"
instead of the swallowed `lastPersistErr` every other persist path uses), a
caller-issued session-monotonic `seq` deduplicated against a durable
high-water mark (`Session.EnqueueSeq()`, journaled on the record and
rebuilt by `LoadSession` — a seq at or below the mark is a clean 200
`duplicate` no-op, so retries are always safe, including across a process
restart), and torn-write healing (a burned-but-failed queue ID is never
reused, and replay folds same-seq records last-writer-wins). Delivery is the
exact same FIFO/tool-boundary/goal-boundary machinery described above — this
is a new *acceptance* contract, not a new delivery path: durable means
accepted into the queue, and delivery-out is still the queue's normal
at-most-once-per-dequeue machinery, so a crash between dequeue and turn
completion loses that delivery once rather than redelivering it, exactly
like any in-flight prompt (`maybeDispatchQueued`'s "No-double-delivery
equivalence", invariant 7, in server/handlers.go). `GET
/session/{id}/queue` is the paired reconciliation read: the watermark plus
the pending queue (FIFO, `seq` present only on durable-enqueue entries), for
an upstream recovering from its own crash to check what's already inside the
durability domain instead of re-sending blind. `prompt_async` remains the
right choice for an interactive client that has no upstream ack to protect —
it is not going away, and `POST /session/{id}/enqueue` adds one limit of its
own beyond what queued prompts have: **text parts only**. Its callers are
machine relays, not the interactive composer that uploads images, so the
durable-accept contract stays a string contract.

The `fsync` in "write-ahead durability" above is itself mode-selectable:
config's `session_sync` ("fsync", the default, or "volume") gates both this
durable-enqueue fsync and the one-time session-create directory fsync
(`ensureLog`'s fresh-file `syncDir` call, store.go) — nothing else changes.
"volume" is for a session store on a continuously-synced network volume
whose own commit layer is the documented durability boundary: fsync adds no
durability there, and some FUSE/9p transports deadlock permanently on it
(`fsync(dirfd)` especially — a wedge that hangs every later file op on the
mount, not just the one call). In that mode the write(2) landing out of
`EnqueuePromptDurable`/`ensureLog` is itself the attestation; the write
ordering, torn-write healing, and replay/fold logic above are byte-for-byte
identical in both modes — a volume can still lose an unsynced tail on abrupt
death exactly like a torn fsync can, and the same last-writer-wins fold
repairs both. See docs/deploy-modal.md for the recommended setting on Modal
Volume v2 deployments.

## Managed processes

`config.Config.Processes` (`processes` in JSON) declares named long-lived
dev/support processes (`pnpm dev`, a local DB) that a `process` session
tool can start/stop/restart/inspect without an agent reinventing PID
tracking. `*process.Manager` (package `process`, not `engine`) is a
box-scoped singleton — built once per harness process and shared across
every session, exactly like `engine.MCPManager` — with a
starting/ready/running/exited/stopped state machine, unix process-GROUP
kill on stop (mirroring `engine/bash_unix.go`'s Setpgid/kill-pgroup/
WaitDelay pattern), and asynchronous death detection (a waiter goroutine
flips state to `exited` with no client asking). Logs land at
`<workDir>/.harness/proc/<name>.log`.

The tool can also `declare`/`undeclare` NEW process definitions at
runtime (server-lifetime only, never written to `.harness.json`) — see
`docs/design/managed-processes.md` for the full validation and origin
(`config` vs `runtime`) rules. `harness serve` always builds a
`*process.Manager`, even with zero configured processes, so the tool is
present on every served box; `harness run` keeps the zero-cost-when-
unconfigured rule.

Once at least one declared process has EVER been started (this server
process's lifetime), request assembly appends an ephemeral `[processes:
...]` status block to the newest user message ONLY — as a
`message.EngineContext` part (see the "Ambient engine context" section in
`docs/engine-request-cycle.md`), never persisted into the durable session log,
never touching any earlier message so a provider's prompt cache prefix
stays intact. See `docs/design/managed-processes.md` §4 for the exact
mechanism and why it is safe.
