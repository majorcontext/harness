# Unifying session messaging: one send path for a root and a child

## Motivation

Harness had two ways to deliver a durable user message to a session, and
they disagreed on almost everything:

- `POST /session/{id}/prompt_async`: rich `{parts:[text|blob…]}` body, the
  only attachment-capable send path, root-only (`rejectManagedChildTurn`
  409s a managed child outright).
- `POST /session/{id}/send`: one handler for root and child, text-only, no
  attachments. A busy root queues (the same FIFO queue `prompt_async`
  uses); a busy child gets a bare 409 — it has no queue at all.

A managed child (a `task`-tool subagent, or a `session.create` with
`parent_id`) is a session like any other, distinguished from a root by
nothing but lineage metadata (`Session.TaskParentID`) — but it could not
receive an attachment, and a real user message sent to it while busy was
refused with no retry contract instead of queued. `rejectManagedChildTurn`
existed to prevent something worse than a missing feature: routing a
generic per-`{id}` handler at a child's id cold-loads a SECOND
`*engine.Session` over the same on-disk log and drives `Session.Prompt`
concurrently with the child's own `Spawn`-driven turn on the first object —
the exact "never call `Prompt` concurrently with itself" violation
`ExternalRunner` exists to prevent for roots, left wide open for children.
The guard traded that corruption hazard for a blunt, permanent 409.

## The single-owner argument

The corruption hazard is not intrinsic to messaging a child — it is
intrinsic to creating a SECOND `*engine.Session` for a session
`engine.SessionManager` already owns. So the fix is not a smarter refusal;
it is routing every send through the ONE resident node SessionManager
already tracks, so a second object is never created in the first place.

Concretely, this change adds `engine.SessionManager.SendOrQueue`
(`engine/session_manager.go`): the single-owner entry point every
child-directed send now goes through, for both endpoints. It generalizes
the existing `SendToDescendant` (the `task` tool's own send verb) minus its
ancestor/lineage gate — a first-party HTTP endpoint addressing a child by
id directly is not the `task` tool acting on behalf of a spawning parent,
and has no caller id to validate against. Both methods still share the
same core: mutate the child's own `promptQueue` (or reserve+launch a fresh
turn) entirely inside `SessionManager`'s own `m.mu`/`s.mu` critical
sections — no code path outside this package ever calls `LoadSession` or
constructs a second `*engine.Session` for a live child.

**Invariant.** For any session id that `SessionManager` currently tracks,
exactly one `*engine.Session` object exists in this process, and every
mutation to it — a prompt, a queue append, a model/effort/service-tier
swap, an abort — goes through that one object, reached either via
`SessionManager`'s resident node (a child) or via this server's
`s.sessions` residency map, itself backed by at most one live object per id
(a root). Nothing in this change creates a second path to a live child's
`*engine.Session`.

A concurrency test (`TestSendOrQueueConcurrentCallsAgainstSameRunningChild
NeverCorrupt`, `engine/session_manager_send_or_queue_test.go`, run under
`-race`) fires 20 concurrent `SendOrQueue` calls at one running child and
asserts every one of the 20 distinct messages is delivered exactly once,
none lost or duplicated — the practical proof the single-owner routing
holds under real concurrency, not just by inspection.

## The endpoint decision

`POST /session/{id}/send` is the canonical path. Its body now accepts an
optional `parts` array — the same `{type:"text"|"blob", ...}` wire shape
`prompt_async` already used (`decodePromptParts`, `server/prompt_parts.go`)
— a strict superset of the original `{text}` shape, which is still
accepted unchanged for a caller that sends it (`decodeSessionSendBody`,
`server/session_tree.go`). A root routes through `sendTextToRoot`
(extended to thread `blobs` through, now identical in capability to
`prompt_async`'s own root path) — the same `claimForPrompt` admission gate
`prompt_async` uses, never through SessionManager, so it can never compete
with a concurrent `prompt_async` request for the same root (see
`ExternalRunner`'s own doc comment). A child routes through
`SendOrQueue` directly.

`POST /session/{id}/prompt_async` is kept, not removed — `cmd/harness`'s
own client and existing external callers depend on its response shape
(`promptAsyncResponse`, distinct from `session.send`'s `{session_id,
status, queued}`) and its request-scoped features `session.send` does not
have (a per-request `model` override, `MaxBytesReader`-bounded body — both
already present on `prompt_async` before this change). Rewriting it as a
literal alias of `session.send` would have meant either dropping those
features or growing `session.send`'s own contract to match — more churn
than the actual problem (a child could not use this endpoint at all)
required. Instead, `handlePrompt`'s child branch is now a thin wrapper
around the exact same `SendOrQueue` call `session.send`'s child branch
makes (`server/handlers.go`), reusing its admission/queuing/blob-threading
verbatim, only translating the response into `prompt_async`'s own shape.
The two endpoints are therefore not one function, but they now drive a
child through the identical single-owner path — the property that matters.
`prompt_async`'s per-request model override is silently not applied on the
child branch, mirroring `enqueueOrDispatch`'s already-documented rule that
a queued prompt carries no model-ref slot (`server/handlers.go`) — not a
new asymmetry, the same one root callers already accept whenever their own
prompt ends up queued instead of started immediately.

## The queue, generalized

`SendOrQueue`'s busy-target branch is `SendToDescendant`'s own
memory-append-then-deferred-persist sequence (`enqueueMemoryOnlyLocked` +
`queueRecordDeferredLocked`, both under one `m.mu`→`s.mu` nested hold,
flushed to disk after `m.mu` releases via `deferPersist`/
`unlockAndFlushPersist`) — not a new queue implementation. A child's
`promptQueue` is the exact same field and mechanism a root's queue already
is (`engine/queue.go`); this change does not introduce a second queue
type, only a second, ancestor-free caller of the existing one.

`drainQueueAndPrompt` (the function that runs a child's initial turn, then
drains its queue one item at a time once that turn ends) used to call the
attachment-less `Session.Prompt` and drop `QueuedPrompt.Blobs`/`MessageID`
entirely on every dequeue — a pre-existing gap, not something this change
introduces, but one it closes as a direct consequence of threading blobs
through: it now calls `Session.PromptWithOrigin` with each queued item's
own id and blobs. This also fixes the identical drop on `finalizeTurnFrom`'s
own queued-message re-drive path, which shares the same function.

## Child turn-lifecycle events

A child previously emitted **zero** SSE/journal events — `turn.end`,
`session.status`, `session.error`, `session.aborted` are all emitted by
this server's `runPrompt`/`freeRunSlotAndEmitIdle`
(`server/handlers.go`), which only a ROOT's turn ever reaches. A child's
turn is driven entirely inside `engine.SessionManager` (`Spawn`, `Send`,
`SendOrQueue`), with no hook back into this server at all.

`engine.SessionManager` gains `ChildTurnObserver` and
`SetChildTurnObserver` (mirroring the existing `ExternalRunner`/
`SetExternalRunner` pair): a callback `finalizeTurnFrom` invokes, via the
same `deferPersist`/`unlockAndFlushPersist` mechanism every other side
effect in that function already uses, once a CHILD's (`n.parentID != ""`)
turn settles — done, failed, or canceled. It is gated on `n.parentID !=
""` specifically so it can never double-fire for a root (a root's own
`ReportTurnEnd`-driven settle reaches the SAME `finalizeTurnFrom`, but
takes the `n.parentID == ""` branch of its outcome switch, which this hook
sits after and does not touch).

`server.New` installs `onChildTurnEnd` (`server/journal.go`), which
mirrors `runPrompt`'s own err/cancellation switch exactly: `canceled` →
`session.aborted` (no `turn.end`, matching a root's `context.Canceled`
branch); `err == nil` → `turn.end(completed)`; otherwise → `session.error`
then `turn.end(<classified outcome>)` — always followed by
`session.status(idle)`, mirroring `freeRunSlotAndEmitIdle`'s unconditional
idle emission. A child now streams the identical vocabulary a root does.

The turn-START side is symmetric: `engine.SessionManager` also gains
`ChildTurnStartObserver`/`SetChildTurnStartObserver`, fired from every
choke point that transitions a child node into `StatusRunning` to drive
an actual turn — `reserveSendLocked` (shared by `Send`, `SendOrQueue`'s
settled-target relaunch, and `SendToDescendant`'s settled-target
relaunch, gated on `n.depth > 0` so a root sharing that same helper in
bare-CLI/engine usage never fires it) and `Spawn`'s own initial
reservation (which never calls `reserveSendLocked`, since it creates a
brand-new node rather than reserving an existing one). `server.New`
installs `onChildTurnStart` (`server/journal.go`), which emits the exact
same event a root's own admission path already emits at the identical
moment — `Event{Type: evtSessionStatus, Status: "busy"}`, the identical
type/field shape `sendTextToRoot`/`dispatchQueueHead`/`handleGoal`/
`handleCompact` all already use.

`ChildTurnStartObserver` stays deliberately 1:1 with `ChildTurnObserver`
rather than firing once per item `drainQueueAndPrompt` drains internally:
a message queued against an ALREADY-running child (`SendOrQueue`'s/
`SendToDescendant`'s running-target branch) is delivered within the SAME
reserved run the preceding start already announced, and does not get its
own settle either — the whole drained sequence still starts once and
settles once. Firing a start per drained item without a matching
per-item settle would leave more starts than ends for one child, a
worse mismatch with a root's own well-formed busy/idle bracket than the
coarser, but internally consistent, one-reservation/one-settle pairing
this change ships.

## What is NOT unified, and why

`rejectManagedChildTurn` (`server/handlers.go`) still guards `handleGoal`,
`handleGoalDelete`, `handleEnqueue`, `handleQueueDelete`, and
`handleCompact`. Each of these, unlike `prompt_async`'s child branch and
the three knob swaps below, resolves its session via `claimForPrompt` (or
the equivalent `s.sessions` residency map) and — for a goal loop or a
synchronous compact call — actually DRIVES a turn on whatever object that
resolution hands it. Single-owner routing was not built for any of these
in this change: doing so would mean threading `SessionManager`'s goal-loop
and compaction machinery through a child's own resident node, a
meaningfully larger change than collapsing a send path. The hazard
`rejectManagedChildTurn` exists to prevent is still fully live for these
five routes, so the guard stays, unchanged, exactly where it already was.

`handleSetModel`, `handleSetThinking`, and `handleSetServiceTier`
(`server/handlers.go`) no longer call `rejectManagedChildTurn` at all: each
now resolves a managed child straight from `SessionManager`'s own resident
node (`sess.TaskParentID() != ""`) and mutates it directly — exactly like
`handleAbort` already did for `AbortTurn`, and exactly the single-owner
argument above. `SetModel`/`SetEffort`/`SetServiceTier` are documented as
concurrency-safe, run-slot-free swaps (they take effect on the session's
NEXT turn, never the current one) — safe to call directly against a
resident child's live object regardless of whether a turn happens to be
in flight on it, the same property that already made them safe against a
busy root.

## Verification

- `go build ./... && go vet ./... && test -z "$(gofmt -l .)"` — clean.
- `go test -race ./engine/... ./server/...` — clean, including the new
  concurrency test above and the full existing suite (updated where an
  old test pinned the 409-refuses-a-busy-child or 409-refuses-prompt_async
  contract this change deliberately replaces — see
  `TestSessionSendToBusyChildIsQueuedNotLost`,
  `TestGenericTurnRoutesUnifiedSendAllowsManagedChild`).
- `go test -race ./...` — clean except one pre-existing, unrelated `e2e`
  failure (`TestAppendSystemPromptReachesModelOnServe`), reproduced
  identically on `origin/main` before this change.
