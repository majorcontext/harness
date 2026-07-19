# Prompt Queue-on-Busy Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** `POST /session/{id}/prompt_async` against a busy session enqueues the prompt (202 `{status:"queued"}`) instead of 409; queued prompts deliver FIFO when the run slot drains, inject at goal-loop turn boundaries as operator interjections, and survive restarts as session-log records. Parity with #77's goal arming.

**Architecture:** (1) Engine: durable FIFO on `Session` via `prompt.queued`/`prompt.dequeued` records (never in provider-visible history until delivered); `PursueGoal` drains the queue into each turn's directive at the same snapshot boundary #77 added. (2) Server: `handlePrompt` enqueues synchronously-before-202 on same-session-busy; `maybeDispatchQueued` at both `runPrompt` and `runGoal` tails, with queue-beats-goal-auto-arm precedence; `DELETE /session/{id}/queue` clears durably; queue count surfaced on `GET /session`.

**Tech Stack:** Go, testing/synctest where timing-adjacent, channel-gated fake providers, table tests.

---

## Locked design decisions (do not relitigate)

- **Queued prompts never enter `s.history` (or any provider request) until delivered.** They live in a separate `Session` field + their own record types. Delivery = either a normal `Prompt()` call (idle drain) or inclusion in a goal turn's directive (goal injection).
- **Enqueue is durable before the 202 returns** — `EnqueuePrompt` runs synchronously in the handler, like `RegisterGoal` (the accept-vs-lose race is structurally gone). This is deliberately STRONGER than today's plain-prompt fire-and-forget append.
- **FIFO everywhere.** Order is by queue ID (session-monotonic, persisted). Mixed drain paths preserve order: goal injection drains all pending at that boundary in order; idle dispatch takes the head only (each dispatched prompt's tail re-checks).
- **Queue beats goal auto-arm at drain time.** Direct user input outranks the background objective. Goal arms only when the queue is empty.
- **Goal injection judges only the goal.** Interjections are labeled operator messages prepended to the turn directive; the evaluator prompt is unchanged (still only the condition). Injection consumes no extra goal turn.
- **Dequeue journals BEFORE the text enters any turn** (replay can never double-deliver). Reasons: `delivered` (idle dispatch), `injected` (goal boundary), `cleared` (DELETE).
- **Boot never auto-dispatches.** A resumed queue folds from the log, surfaces as a count, and waits for the next natural drain (next prompt/goal activity). Same settled rule as goals.
- **`POST /abort` does not touch the queue.** `DELETE /session/{id}/queue` is the clear surface: journal all `prompt.dequeued(cleared)` records first, then 204; idempotent; never cancels a running turn.
- **Workdir-holder 409 unchanged.** Only same-session-busy gets queue semantics. Draining server still 503s.
- **Text-only** (v1 prompt contract is text parts only). Records carry strings. No attachment machinery.
- **Lock discipline:** engine queue methods take only `session.mu`, emit while holding it; never called under `server.mu` (leaf order). The three fold sites (engine LoadSession, server publish, server boot replay) stay behavior-identical for the new records.

## Invariants (the spec — every one gets a test)

1. `EnqueuePrompt` persists `prompt.queued` + emits `EventPromptQueued` under mu; returns a monotonic ID; empty/whitespace text rejected.
2. A queued prompt is absent from `History()` and absent from the provider request of any in-flight turn.
3. `LoadSession` replay of queued/dequeued records reconstructs exactly the undelivered set, in ID order.
4. Idle drain: when a turn ends and the queue is non-empty, the server dispatches the head as a normal prompt turn (dequeue-`delivered` journaled before the turn starts); each turn's tail repeats until empty. SSE ordering: occupant's idle precedes the dispatched prompt's busy.
5. Queue-beats-goal: with both a non-empty queue and an armed goal at drain, all queued prompts run first; the goal auto-arms after the queue empties. The loser of any slot race no-ops cleanly.
6. Goal injection: prompts queued while a goal loop runs are ALL drained at the next turn boundary, FIFO, prepended to the directive as labeled operator interjections; dequeue-`injected` records journal at that boundary; the evaluator prompt string is unchanged.
7. No double delivery across restarts: kill after dequeue-journal but before turn completion → replay shows the prompt gone from the queue (it was delivered-or-lost with the turn, same as any in-flight prompt today — document this equivalence).
8. Boot: restart with a non-empty queue → `GET /session` shows the queued count, state stays `idle`, nothing dispatches until the next natural drain trigger.
9. `POST /prompt_async`: idle → 202 `{status:"started", seq}`; same-session busy → 202 `{status:"queued", seq, queued: N}`; workdir-holder → 409 unchanged; draining → 503.
10. `DELETE /session/{id}/queue`: journals `cleared` dequeues for every pending item then 204; idempotent on empty; running turn untouched; a goal loop's later boundaries see an empty queue.
11. Server journal parity: `prompt.queued`/`prompt.dequeued` flow through the `Publish` allowlist; live fold and boot replay produce identical queue counts for `GET /session`.

## Key facts from recon (verify then lean on; cites are main-branch)

- `handlePrompt` body/validation `server/handlers.go:706-747`; text-only guard ~723; 409 branch ~735-747; runPrompt `~771-801` ends with unclaim → idle emit → `maybeAutoArmGoal`.
- `claimForPrompt` `~1490-1543` (sets running/cancel, clears goalLoop at claim site).
- `runGoal` tail `~1094-1126` — currently NO drain hook; this plan adds one.
- `maybeAutoArmGoal` `~805-853` — the dispatch-at-tail template, including lost-race no-op discipline and the `autoArmRace` test seam pattern (`server/server.go`).
- `PursueGoal` turn boundary: `engine/goal.go` ~533-566 — `snapshotGoal()` at top of each turn; directive built from condition/guidance; `goalGuidance` ~1233. Queue drain goes at this boundary.
- Engine records: `engine/store.go:29-44` consts, `record` struct ~47-86, `persistGoalLocked` ~212, `LoadSession` fold. Goal records are string-only — fine for text prompts. Note `recMessage` carries `*message.Message` — do NOT reuse it for queue items.
- `Session.Prompt` appends user msg before turn loop (`engine/engine.go` ~651-682); `History()` ~512; managed-processes ephemeral injection (`engine/engine.go` ~760-771) is the never-touch-durable-history precedent.
- Server journal: `Publish` allowlist switch `server/journal.go:194-216`; `goalTracker`/`goalState` fold live ~244-320 and boot ~647-720; `pauseArmedGoalsAtBoot` ~748.
- Tests to rewrite: `TestConcurrentPromptConflict` (`server/server_test.go` ~811) — the 409 pin becomes the queued pin. Templates: `TestGoalPostWhilePromptBusyArmsThenAutoStarts`, `TestAutoArmRaceWithIncomingPrompt` (race seam), restart idioms in `server/goal_paused_test.go`.
- Hub: `tools/hub/index.html` `sendPrompt()` ~2717 — 409 notice becomes a "queued" notice keyed on the new 202 body.

---

### Task 1: Engine — queue records + Session FIFO (`EnqueuePrompt`/`DequeuePrompt`/`QueuedPrompts`)

**Files:** create `engine/queue.go` + `engine/queue_test.go`; modify `engine/engine.go` (events + Session fields), `engine/store.go` (records + fold).

Records: `recPromptQueued`/`recPromptDequeued` with a `promptRecord{ID int64, Text string, Reason string}` payload field on `record` (string-only). Session fields: `promptQueue []QueuedPrompt` (`{ID int64, Text string}`), `promptQueueNextID int64` (max folded ID + 1 on load). `EnqueuePrompt(text)` validates non-empty, assigns ID, persists, emits `EventPromptQueued` under mu. `DequeuePrompt(reason)` pops head, persists with reason, emits `EventPromptDequeued`. `dequeueAllLocked(reason)`-style helper for goal injection and clear paths as needed.

TDD steps (red each first): `TestEnqueuePromptPersistsAndEmits`, `TestEnqueueRejectsEmpty`, `TestDequeueFIFOAndJournalsReason`, `TestQueuedPromptsAbsentFromHistory` (enqueue then assert `History()` unchanged and a subsequent `Prompt` provider request omits it — invariant 2), `TestLoadSessionRefoldsQueue` (queued+dequeued mix → exact undelivered set, ID order, next-ID continues monotonic — invariant 3). Full `go test -race ./engine/`. Commit: `feat(engine): durable session prompt queue (prompt.queued/dequeued records)`.

### Task 2: Engine — goal-boundary injection

**Files:** modify `engine/goal.go`; extend `engine/queue_test.go`/`engine/goal_update_test.go`.

At the top of each `PursueGoal` turn (adjacent to `snapshotGoal()`): drain all queued prompts (single locked op journaling `injected` dequeues in order, emitting events), and if any, build the turn directive as labeled interjections + the normal directive. Suggested template (match goalGuidance tone; final wording implementer's choice, documented): `"OPERATOR MESSAGES (address these, then continue the goal):\n1. <text>\n...\n\n" + directive`. Evaluator call unchanged. Works turn 1 and later turns. Stale/generation interplay: injection happens before the worker turn; discarded turns do NOT restore injected prompts (they were delivered — document).

TDD: `TestGoalInjectsQueuedPromptsAtBoundary` (queue two mid-turn via channel gate → next directive contains both, FIFO, evaluator prompt clean, dequeue-`injected` records journaled — invariants 6), `TestGoalInjectionSurvivesConditionUpdate` (UpdateGoal + queued prompt same window → next turn has new condition AND interjection), `TestInjectedPromptsNotRedeliveredAfterStaleDiscard`. Red-verify the headline test. Commit: `feat(engine): goal loops drain queued prompts as turn-boundary interjections`.

### Task 3: Server — queued admission + drain dispatch + precedence

**Files:** modify `server/handlers.go`, `server/server.go` (race seam if needed), `server/journal.go` (Publish allowlist + queue tracker live/boot folds), `server/openapi.yaml`; tests in new `server/queue_test.go`, rewrite `TestConcurrentPromptConflict`.

`handlePrompt`: claim success → 202 `{status:"started", seq}` (response gains status field). Same-session 409 → `EnqueuePrompt` synchronously → 202 `{status:"queued", seq, queued: <len>}`; then ONE claim retry to close the freed-slot race (mirror `handleGoalBusy`): success → dequeue-`delivered` head + spawn runPrompt (`status:"started"`). Workdir/draining unchanged. `maybeDispatchQueued(id, st)`: claim, dequeue-`delivered` head, spawn runPrompt with its text; called at `runPrompt` tail BEFORE `maybeAutoArmGoal` and at `runGoal` tail (which currently has no hook — add it; goal termination frees the slot). Journal: new evt consts + `Event` fields (ID/Text/Reason/QueueLen as needed), `Publish` cases, `queueTracker` folded live + boot; `GET /session` gains `queued` count. `DELETE /session/{id}/queue` handler (invariant 10).

TDD (each red first): rewrite `TestConcurrentPromptConflict` → queued semantics; `TestQueuedPromptDispatchesOnDrain` (invariant 4, SSE ordering); `TestQueueDrainsFIFOAcrossMultiplePrompts`; `TestQueueBeatsGoalAutoArm` (invariant 5); `TestQueuedDispatchAfterGoalLoopEnds` (runGoal-tail hook); `TestQueueRestartRefoldNoAutoDispatch` (invariant 8, restart idiom); `TestDeleteQueueClearsDurably` (invariant 10); `TestPromptQueueRaceWithFreedSlot` (seam pattern). Commit: `feat(server): prompt_async queues on busy sessions; FIFO drain dispatch`.

### Task 4: Docs + contract sweep

**Files:** `AGENTS.md` (prompt queue paragraph in the server/goal-loop area: semantics, precedence, no-boot-dispatch, DELETE surface), `server/openapi.yaml` re-verify against final handlers, hub `sendPrompt` notice ("queued (N waiting)" on the new 202 body; verify hub tolerates the new SSE event types), full gates including `node --test tools/hub/*_test.mjs`. Commit: `docs: prompt queue semantics` (+ separate hub commit if code changed).

---

## Execution notes

- Red-verify headline regression tests per AGENTS.md (stash/revert, observe old behavior, restore).
- No time.Sleep for sync; channel-gate everything; reuse `autoArmRace`-style seams for deterministic races.
- Emit-under-mutex discipline for every new record; lock-order tests must stay green.
- Conventional Commits; no AI co-author lines.

---

## Design amendment: tool-call-boundary injection (2026-07-19)

Delivery granularity moved from per-turn to per-tool-call-boundary. Originally
(Tasks 1-4 above), a queued prompt's earliest delivery point inside a running
turn was the server's own tail drain (`runPrompt`/`runGoal`/`handleCompact`'s
end) or, for a goal loop, `PursueGoal`'s next turn-boundary snapshot — both
meaning a prompt queued mid-turn waited for that ENTIRE turn (every tool call
in it) to finish before it could be delivered.

The amendment: `Session.Prompt`'s agentic loop (`engine/engine.go`) now
drains the ENTIRE queue, FIFO, immediately after a tool-result message is
appended and before the next provider request in that SAME turn —
`DequeueAllPrompts("injected")`, journaling every dequeue BEFORE the
delivery message is appended, same ordering rule as before. The delivered
content reuses the exact same labeled "OPERATOR MESSAGES" block goal-turn-
boundary injection already used (factored out of `goal.go` into
`operatorMessagesBlock`, `engine/queue.go`, so both call sites render a
drained batch identically), appended as one durable user message straight
into history — append-only, so a provider's prompt-cache prefix stays
intact. A turn that ends with no tool call at all never reaches this drain
point (unchanged: left for the server's tail drain). Because goal worker
turns run through this same `Prompt` loop (`promptTurnWithRetry`), goal
loops inherit tool-call-boundary injection with no separate wiring — a
prompt queued mid-worker-tool is delivered inside that same worker turn.
`PursueGoal`'s own turn-boundary drain (Task 2, `goal.go`) is KEPT as a
complementary fallback: it still catches a prompt queued during a turn that
made no tool calls, or in the gap between one turn ending and the next
one's snapshot. The two drain sites can never double-deliver the same
prompt — `DequeueAllPrompts` is one atomic, locked pop of the whole queue.

New tests: `TestMidTurnInjectionAtToolBoundary` and
`TestNoToolCallTurnLeavesQueueForTail` (`engine/queue_toolcall_boundary_test.go`),
`TestGoalWorkerTurnInheritsMidTurnInjection`
(`engine/goal_toolcall_boundary_test.go`), and
`TestQueueDropsMidTurnAtToolCallBoundary`
(`server/queue_toolcall_boundary_test.go`), which channel-gates a real tool
execution to prove the queue count drops (and the SSE `prompt.dequeued`
event fires) before the occupying turn ends. `AGENTS.md`'s Prompt queue
section and `server/openapi.yaml` were both updated to describe the new
primary delivery granularity, with the goal-turn-boundary drain demoted to
"complementary fallback" language.
