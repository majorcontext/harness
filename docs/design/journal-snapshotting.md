# Journal Snapshotting — design

**Status:** draft for review (2026-08-27). Author: coordinator, via brainstorm with Andy.
**Repos:** harness (`majorcontext/harness`, Layer B) + boxes (`meetneptune/boxes`, Layer A).
**Extends:** `boxes/docs/design/console-read-path.md` (this is the source-level fix its workstream 1 gestured at, and the seam its workstream 5 mirror plugs into).

## 1. Problem

Reading or writing a box session requires the in-memory `*engine.Session`, which
`engine.LoadSession` rebuilds by replaying the whole `.jsonl` journal from seq 0
(`os.ReadFile` + record-by-record `scanLog`). Replay cost is **O(journal size)**
and grows for the life of the session. Measured on the deployed Webhooks box:

- `GET /transcript` — **8.0s** (cold journal replay; `max_bytes` truncates
  *after* the replay, so it saves wire bytes, not latency).
- `/model`, `/goal`, `/thinking` — **3–6s each**, tiny payloads: each read
  independently forces a session load.
- `/prompt` — the same `LoadSession` replay fires synchronously in
  `claimForPrompt` whenever the target session is not resident (first prompt
  after harness start / wake-from-hibernation / LRU eviction), blocking the 202.

The endpoint audit (2026-08-27) confirms this replay underlies the worst
findings (serial double-replay on `/transcript`, uncapped child transcript,
the goal/model/thinking reads). It is **not CPU** — the box sits at ~11m during
the reads; it is I/O + parse to rebuild the session, on the box's slow (gVisor)
fs.

The write path already keeps the loaded session resident (LRU, 32), so it pays
the replay once per residency window; the read cold-path discards it and pays
every read. Either way the underlying cost is unbounded journal replay.

Secondarily: control-plane handlers reach the journal in **ad-hoc, inconsistent**
ways — the audit found the correct pattern (parallel, byte-bounded reads)
independently invented in `console_bootstrap` and the MCP tools, but
`handleTranscript` regressed to a serial double-replay and `handleGetChildTranscript`
has no cap at all — because nothing shared enforces the discipline.

## 2. Goals / non-goals

**Goals**
- Bound session-load (replay) cost **independent of session age** — checkpoints.
- Fix reads *and* writes with one mechanism (both go through `LoadSession`).
- A single control-plane journal-access layer so no handler can reintroduce an
  unbounded or serial-double read.

**Non-goals (explicitly deferred)**
- Any residency/read cache in the control plane (deferred until the baseline is
  proven — Andy: "no caching until we get the baseline architecture right").
- The control-plane **mirror/projection** (read-path workstream 5). This design
  is the *seam* it plugs into, not the projection itself.
- **Truncating** the journal. The journal stays the untruncated source of truth
  (the mirror and any audit consume the full log). Snapshots are pure
  acceleration.

## 3. Architecture — two layers

- **Layer B — harness journal discipline (the mechanism).** `engine` gains a
  checkpoint: a snapshot is a *seq-anchored, rebuildable* materialization of the
  session at seq N. `LoadSession` becomes "load newest valid snapshot ≤ head,
  replay only records > N." Shared by both callers of `LoadSession` — the read
  cold-path (`GET /session`) and the write path (`claimForPrompt`).
- **Layer A — control-plane shared journal access (the interface).** One
  boxes-side set of methods every handler uses to touch the journal via harness:
  resolve-once, read-bounded-by-default, multi-read-in-parallel. It makes access
  uniform and is the seam the mirror later backs.

Layer B is the root fix (bounds the cost everything pays). Layer A is what makes
that fix reachable uniformly and un-regressable.

## 4. Layer B — the checkpoint

### 4.1 Snapshot content — an explicit schema (verified)
`*engine.Session` (`engine/engine.go:638`) is **not directly serializable**:
every field but `ID` is unexported, so `json.Marshal` can't touch it, and
several fields are process-local and must never be snapshotted. So a snapshot
is an **explicitly-defined schema**, modeled on the existing `record` /
`JournalRecord` pattern (`engine/store.go:165`, `engine/journal.go:45`),
capturing exactly the fold-produced replayable state at seq N, plus the anchor
`seq` and a checksum.

**Capture (the fold-produced state, all guarded by `s.mu`):** `history`,
`model`, `effort`, `usage`/`lastUsage`, `turn`, `lastSystem`, goal state
(`goalActive`, `goalCondition`), `compactCount`/`lastCompactedAt`,
`promptQueue`/`promptQueueNextID`/`enqueueSeq`, `toolResults`/`toolResultNextID`/
`toolResultBytes`, `spawnedChildIDs`, the task-notification queues, and the
crash-recovery signals (`turnUnsettled`, `committedOutcome`).

**Exclude (runtime-only / deliberately non-durable — re-created on load exactly
as a full replay does):** `mu`, `logFile`/`logStarted`/`lastPersistErr`,
`tools`, `cfg` (carries live callbacks + the `SessionManager` pointer +
`ProcessRegistry`), and the fields harness already documents as never-persisted:
`goalGen`, `goalParked*`, `compactHysteresis`.

The rule of thumb: **snapshot exactly what `LoadSession`'s folds reconstruct,
nothing that a fresh process re-wires.** Keeping the schema in lock-step with the
fold logic is the one maintenance burden (a fold added without a matching
snapshot field would be silently dropped) — §7 pins this with a
snapshot-equals-replay round-trip test.

### 4.2 Storage layout
- Snapshot file alongside the journal on the box disk, e.g.
  `<session-id>.snap` (single latest) or `<session-id>.<seq>.snap` (rolling,
  keep last M). Start with a single latest snapshot; rolling is a later option.
- **The journal is never truncated.** Snapshots are derived and independently
  deletable; deleting all snapshots reverts to today's full-replay behavior.

### 4.3 Recovery (`LoadSession`)
1. Find the newest snapshot whose anchor `seq` ≤ current journal head and whose
   checksum validates.
2. Deserialize it into the session.
3. Replay only journal records with `seq > N`.
4. On *any* missing/corrupt/mismatched snapshot → full replay from seq 0.
   **Slower, never wrong** — the philosophy already in the codebase.

Correctness invariant: state(snapshot@N) + replay(records N+1..head) ≡
full-replay(0..head). A round-trip test pins this.

**The seq anchor (verified).** Records carry **no persisted `seq`** today — seq
is the 1-based line number assigned at scan time by `scanLog`
(`engine/store.go:1528`), which the read-path already exposes as
`JournalRecord.Seq` and which is stable forever because the log is append-only
and never rewritten. Two ways to anchor a snapshot to it, a §9 decision:
- **(a) Live counter:** track a "records written" count in `Session`, bumped in
  `writeRecord` — trivial given the single-writer lock, no on-disk format change.
  Recovery counts scanned lines to know where the tail starts.
- **(b) Persisted `seq`:** add an explicit `seq` field to `record`. Stronger,
  self-describing anchor that doesn't depend on recounting lines, at the cost of
  a (backward-compatible, additive) journal-format change.

Recommend starting with **(a)** — it's the smaller change and the line-number
seq is already durable; **(b)** is a clean follow-up if we want the anchor
independent of a full line count.

### 4.4 Triggers (cadence) — decided
- **On-idle.** When a session goes quiescent (no active turn), snapshot at the
  current head seq. This is the trivially-correct case (no concurrent append)
  and also makes wake-from-hibernation and post-eviction reload fast.
- **Every-K-messages.** Bound steady-state replay to K records. Start K
  conservative (≈50–100), tunable; a long-lived, continuously-active session
  never accumulates more than ~K records of replay.

The two triggers are coalesced (see 4.5): at most one snapshot in flight per
session.

### 4.5 Concurrency discipline (the five rules)
1. **Seq-anchored.** A snapshot is "valid as of seq N"; recovery replays strictly
   `> N`. A snapshot need not be the latest state — the tail replay closes the gap.
2. **Off the hot path.** Never block a turn/append on snapshot I/O. Capture a
   consistent state + seq quickly, then serialize + fsync in a background goroutine.
3. **Atomic write.** temp file → fsync → atomic rename. A crash mid-write leaves
   the previous snapshot (or none) intact — never a torn file.
4. **Single in-flight, coalesced.** One snapshot per session at a time; the idle
   and every-K triggers cannot race to write the same file (a `snapshotting`
   flag/mutex coalesces them).
5. **Rebuildable + validated.** Snapshot is derived; validate on load (seq +
   checksum); on mismatch, discard and full-replay. A snapshot bug degrades to
   *slow*, never *wrong*.

**How rule 2's "capture a consistent state" is implemented — the clean branch
(verified: single-writer).** Harness serializes every append to one session
through `Session.mu` — `appendWithUsage` (`engine/engine.go:1546`) holds `s.mu`,
mutates `s.history`, and calls `persist*` → `writeRecord` (`engine/store.go:919`)
synchronously under the lock, and SessionManager's `StatusRunning` gating means
at most one turn drives a session at a time. So the snapshot is emitted **by the
append-owner at an append boundary, right after a `persist*` returns while `s.mu`
is already held** — grab a consistent shallow copy of the fold-state + the seq,
release, then serialize + write in a background goroutine. **No new
synchronization primitive and no copy-from-outside-the-lock is needed.** (One
wrinkle to honor: SessionManager's `deferPersist`/`unlockAndFlushPersist`
path — `engine/session_manager.go:318` — flushes some manager-level records
after `m.mu` releases; those re-take `s.mu` and stay per-session serialized, so
the same append-boundary rule applies to them.)

### 4.6 Interaction with residency
`claimForPrompt` already inserts loaded sessions into the resident map (LRU 32).
Checkpointing bounds the *load* cost that both the read cold-path and the write
path pay when a session is not resident. We add **no** new residency cache
(deferred). Opportunistic snapshot-on-eviction (part of on-idle) keeps the next
reload fast.

## 5. Layer A — shared control-plane journal access

A single interface (Go, in `boxes/internal/api`) that every journal-touching
handler uses. Method set (finalized against the endpoint audit):

- `ResolveSession(ctx, box) (sid, error)` — trust `box.CurrentSessionID`; on
  empty, `scanRootSession` fallback **and persist the result** so the residual
  `firstSession` scan (audit #7) fires at most once per box, ever.
- `ReadSessionState(ctx, sid, bounds)` — bounded by default.
- `ReadTranscript(ctx, sid, {tail|limit|before_seq})` — a **default cap**
  applied when the caller omits bounds (fixes audit #2 uncapped box transcript,
  #3 uncapped child transcript).
- `Bootstrap(ctx, sid)` — the console envelope, generalized: one resolve, the
  multiple harness reads fired **concurrently** (fixes audit #1 serial
  double-replay by making parallel the only way to multi-read).
- `AppendPrompt(ctx, sid, text)` — resolve-once, no transcript read.

**Invariants the layer enforces** (each maps to an audit finding):
- No unbounded read — every read has a default bound (#2, #3).
- Multi-read is parallel, never serial (#1).
- Resolve once — sticky pointer, persisted fallback (#7).

**The mirror seam:** `ReadSessionState`/`ReadTranscript`/`Bootstrap` read through
the mirror/projection when it exists and fall back to harness otherwise — so
workstream 5 lands behind this interface with **no handler changes**.

## 6. Migration & sequencing

1. **Quick wins first (independent, ship now):** the three audit fixes —
   parallelize `handleTranscript`'s two reads (#1), default byte caps on the two
   transcript routes (#2/#3). These are the first callers to move onto Layer A's
   discipline and give immediate relief before Layer B lands.
2. **Layer B (harness):** additive in `engine`. `LoadSession` gains
   snapshot-aware recovery; a snapshot writer + the two triggers are added.
   Fully backward compatible — no snapshot present ⇒ full replay ⇒ today's
   behavior. Ship dark, validate round-trip on real sessions, then rely on it.
3. **Layer A (boxes):** introduce the interface; migrate handlers
   (transcript, goal/model/thinking, bootstrap, prompt) onto it. The mirror
   (workstream 5) later backs the read methods behind the same interface.

## 7. Testing

**Layer B**
- Round-trip: snapshot@N restores identical state to full-replay@N.
- Recovery: state(snapshot@N) + replay(N+1..head) ≡ full-replay(0..head), for
  N at several points.
- Fallback: missing / checksum-mismatch / seq-ahead-of-head snapshot ⇒ full
  replay, correct result.
- Crash safety: a truncated/partial `.snap.tmp` never loads; prior snapshot
  stands.
- **Bounded replay:** a synthetic long session's `LoadSession` time is ~constant
  as journal length grows past K (the actual point of the feature) — assert the
  *replayed-record count* is ≤ K, not a wall-clock number.

**Layer A**
- `ResolveSession` issues no `firstSession` scan on a box with a sticky pointer;
  fires (and persists) at most once when empty.
- Reads are bounded by default when the caller omits bounds.
- `Bootstrap`/multi-read fires its harness reads concurrently, not serially
  (assert the concurrency, e.g. via overlapping timing or a fake harness that
  records call ordering) — pins the #1 regression shut.

House rule: assert the specific behavior (bounded, parallel, resolve-once, seq
count ≤ K), **not** raw aggregate request counts.

## 8. Rollout & risk
- Snapshots are rebuildable and validated ⇒ safe to ship dark and fall back.
- Journal never truncated ⇒ the mirror and audit are unaffected.
- Backward compatible ⇒ old sessions with no snapshot behave as today until they
  earn their first snapshot.

## 9. Decisions (locked 2026-08-27)
Both harness facts are **verified** (§4.1 schema, §4.5 single-writer). Tuning/
layout choices, decided:
1. **K cadence = 64 records**, exposed as **config** (not a magic constant) so it
   can be tuned without a code change. On-idle snapshotting also always applies.
2. **Snapshot file layout = single-latest** (`<id>.snap`). Rolling last-M is a
   later option if a corrupt-newest fallback (to an older snapshot instead of a
   full replay) proves worth it; single-latest already falls back to full replay
   safely.
3. **Seq anchor = live counter** (§4.3 option a) — track records-written in
   `Session`, bumped under `s.mu` in `writeRecord`. Persisted `record.seq`
   (option b) is a clean follow-up, not needed for v1.

## 10. Implementation touch points (verified against `/Users/andybons/dev/harness`)
- `engine/store.go`: `LoadSession` (**:968** — snapshot-aware recovery),
  `writeRecord` (:919 — bump the seq counter / emit the append-boundary trigger),
  `scanLog` (:1528 — tail replay from > N), the `record` type (:165 — if we take
  seq-anchor option (b)), and the `persist*` helpers (~:440–670).
- `engine/engine.go`: the `Session` struct (:638 — the snapshot schema mirrors
  its fold-state fields), `appendWithUsage` (:1546 — the append boundary),
  `newSession`/`NewSession` (:956 — snapshot-aware construction).
- `engine/session_manager.go`: `deferPersist`/`unlockAndFlushPersist` (:318 —
  the deferred manager-level writes that also snapshot per-session).
- `engine/journal.go`: reference for the existing curated-replayable-state
  projection to model the snapshot schema on.
- Boxes (Layer A): `internal/api` — the shared journal-access interface and the
  handler migrations (`harness_proxy.go` transcript, `harness_{goal,model,thinking}.go`,
  `console_bootstrap.go`, `session_resolve.go`).
