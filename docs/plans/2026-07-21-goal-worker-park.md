# Goal Worker-Failure Park Implementation Plan (NEP-4849 upstream)

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Worker-turn exhaustion must never clear an armed goal. Production incident: OpenRouter 404s (non-retryable class) exhausted 3 rapid attempts → `goal.cleared` → `session.error` → box stranded idle for hours. Both exhaustion tiers now PARK: the loop exits with the goal still armed, journals a durable `goal.parked` record, the server maps a distinct loud terminal, and the existing activity-driven auto-arm resumes it on the next prompt completion.

**Architecture:** (1) Engine: `PursueGoal`'s deterministic-exhaustion branch stops calling `clearGoal`; both it AND the retryable-exhausted branch (today an in-loop `continue` that holds the run slot forever during an outage) return a new `*goalWorkerParkedError` sentinel after journaling `goal.parked` (classified error, attempts, tier). A runtime-only `Session` flag drives a small ambient segment so an agent prompted mid-outage sees the parked goal. (2) Server: `runGoal` maps the sentinel to `session.error` + new `turn.end outcome=worker_parked`; the tracker folds `goal.parked` into a third pause arm (`pause_reason: "worker_failure"`), `compositeState` reads idle (no loop attached — the restart-pause precedent, NOT the provider-backoff one), and every re-arm path resets it. Resume needs zero new machinery: `maybeAutoArmGoal` already re-arms any active goal at the next prompt tail, and `runGoal`'s tail deliberately never auto-arms (the anti-churn property is pre-existing and documented at handlers.go:1164-1172 — preserve that comment's truth).

**Locked operator decisions:** park-never-clear (no clear horizon — unlike the evaluator's 5-boundary terminal, parking is immediate at exhaustion and DELETE /goal stays the only clear); 404 classification unchanged (fail-fast cadence, survivability from the park); resume activity-driven only (no timers).

**Tech Stack:** Go, `testing/synctest` for all backoff timing, channel-gated fakes.

---

## Locked design decisions

- **Both tiers exit-park.** Deterministic (3 attempts, ~5s) and retryable (12 attempts, ~30min in-turn backoff) exhaustion both journal `goal.parked` + return the sentinel. This CHANGES #61's retryable-exhausted semantics from continue-in-loop to exit — deliberately: exiting frees the run slot during long outages (queued prompts run as normal turns instead of only injecting), and resume-on-activity re-enters the loop cleanly. Document the supersession in goal.go's round-history style and docs/goal-loop.md.
- **`goal.parked` is an ENGINE record/event** (`recGoalParked`/`EventGoalParked`), persisted+emitted under `s.mu` before `PursueGoal` returns, carrying `{Reason: <classified error — reuse/extend the #82 classifier family; NEVER raw provider error text on this surface>, Attempts, RetryableClass?}` — distinct from the boot-only server-synthesized `goal.paused` (which stays as-is). Generation-gated like `goal.stalled` (a park racing `UpdateGoal` → stale-discard, loop continues against the new condition instead of parking).
- **The goal stays armed**: no `clearGoal`, `ActiveGoal()` true, LoadSession replay unchanged (goal.parked folds as trace; active goal restores; boot pause presentation then applies as today).
- **Server terminal is loud + distinct**: sentinel + exported `engine.IsGoalWorkerParked` predicate (the #81 evaluator_exhausted wiring pattern end-to-end): `runGoal` default-branch emits `session.error` and `turn.end outcome=worker_parked`; openapi outcome enum + Event docs updated. Event order: goal.parked < session.error < turn.end < idle.
- **Pause presentation third arm**: tracker folds `goal.parked` → `pausedWorker` (+ attempts + reason); `pauseView` precedence: restart > worker_failure > provider-backoff; `pause_reason: "worker_failure"` const; `compositeState` forces idle for worker-park (no loop attached — mirror pausedRestart, NOT provider-backoff which keeps goal-running). Reset `pausedWorker` wherever pausedRestart resets today (handleGoal re-arm branch) AND in `maybeAutoArmGoal`'s successful arm, AND on set/achieved/cleared/updated folds. Both fold sites (publishGoal + foldGoalRecordLocked) in lockstep; boot replay reproduces a parked presentation.
- **Ambient parked-goal segment** (runtime-only): `Session.goalParked` set when PursueGoal returns parked, cleared when a loop (re)starts (PursueGoal entry) and on clear. Rendered by the established ambient mechanism (third occupant beside process/mcp segments, shared `withAmbientStatus`): present only while parked AND the session is running some other turn — e.g. `[goal: parked after 3 failed worker attempts (connection failed). It resumes automatically when this turn completes.]`. Classified text only. Not persisted; after a process restart the boot pause presentation covers visibility instead (document this asymmetry).
- **No streak horizon.** Parking is per-exhaustion and immediate; no cross-park terminal counter (explicit operator decision). `GoalSummary` surfaces `paused/pause_reason/attempt/last_reason` — sufficient; no new streak field.
- **Context-overflow mid-goal keeps its existing clear** (different failure class: waiting cannot fix it; already has its own distinct outcome). State this in docs so the asymmetry is deliberate.
- **Lock/fold discipline as always**: emit-under-mu, three folds lockstep, no lock across provider calls.

## Invariants (each gets a test)

1. Deterministic exhaustion (non-retryable error ×3): NO `goal.cleared`, goal still armed (live + after LoadSession), `goal.parked` journaled with classified reason + attempts, sentinel returned; event order goal.parked < session.error < turn.end(worker_parked) < idle at the server.
2. Retryable exhaustion (12 attempts, synctest): same park shape; the old in-loop continue is gone (loop exits; run slot freed — server test proves a queued prompt then runs as a NORMAL turn, not just injection).
3. Park racing `UpdateGoal`: stale-discard — no goal.parked for the stale generation, loop continues against the new condition.
4. Resume: after a worker-park, a plain prompt completing auto-arms the goal (maybeAutoArmGoal), the tracker's parked presentation resets, and a now-healthy provider lets the goal achieve. No auto-arm from runGoal's own tail (assert the anti-churn property survives — a park does NOT immediately respawn a loop with an empty queue).
5. Pause presentation: GET /session shows `paused: true, pause_reason: "worker_failure"` + attempts/last_reason while parked; `state` reads idle; boot replay (journal rebuild) reproduces it; provider-backoff and restart presentations unchanged.
6. Ambient segment: absent normally; present (classified text, no raw error/URL) on a prompt turn while parked; gone after resume; never persisted; newest-user-message-only (mirror mcp segment tests).
7. Operator paths unchanged: DELETE /goal on a parked goal clears it (armed-no-loop case — existing coverage extends); POST /goal with different condition on a parked goal updates+resumes (existing re-arm branch) and resets parked presentation.
8. `TestPursueGoalWorkerFailsPermanentlyClearsGoal` and `TestPursueGoalWorkerFailureEmitsOnce` rewritten to the park contract deliberately (keep their original concerns: wrap-not-swallow error semantics → now sentinel semantics; session.error emission counts; no-zombie → no-clear).

## Key facts from recon (file:line, this worktree)

- Fatal branch: engine/goal.go:773-846 (deterministic exhaustion → clearGoal at ~844-846; retryable-exhausted `continue` at ~820; context-overflow clear ~named branch). promptTurnWithRetry :1030-1099 (goalWorkerRetries=2 :357, goalRetryableMaxAttempts=12 :435, toolGateStops gate, sentinel goalRetryableExhaustedError).
- Anti-churn: runGoal tail handlers.go:1478 calls ONLY maybeDispatchQueued; maybeAutoArmGoal doc :1164-1172 explains why — keep true. runPrompt tail :1073-1084 is the resume trigger.
- #81 sentinel wiring pattern: goalEvaluatorExhaustedError + IsGoalEvaluatorExhausted + turnEndOutcome (journal.go:263-271) + outcome const + openapi enums (:237, :829).
- Pause machinery: goalTracker server.go:289-318, pauseView :328-337, pauseReason consts :48-51, boot-only goal.paused synthesis journal.go:886-899, compositeState handlers.go:150-175 (restart→idle vs backoff→goal-running distinction is the load-bearing precedent), goalJSON :194-245.
- handleGoal re-arm reset block: handlers.go:1268-1279 (resets pausedRestart/retryable/waiting — add parked).
- Classifier family: classifyMCPConnectError precedent (engine/mcp.go) — goal park reason needs its own provider-error classification or reuse of goal.stalled's existing Reason handling (check what goal.stalled Reason carries today — if raw provider text already rides goal.stalled records/events, match that for the RECORD but classify for the AMBIENT segment; decide and document — the #82 leak rule binds only model-visible surfaces).
- Ambient mechanism: engine/mcp_status.go + process.go withAmbientStatus (third occupant slots in alongside).
- Tests changing meaning: TestPursueGoalWorkerFailsPermanentlyClearsGoal (engine/goal_test.go:642), TestPursueGoalWorkerFailureEmitsOnce (:534). Park idiom to mirror: TestPursueGoalRetryableBudgetExhaustedParksInsteadOfClearing (:1451) — note it asserts Reason=="max turns" via MaxTurns; the new exit-park changes ITS meaning too (retryable exhaustion now exits parked instead of burning turns) — rewrite deliberately. Server idioms: TestGoalEvaluatorExhaustedTerminalOutcome (server/goal_eval_resilience_test.go:68), TestGoalPausedRestartYieldsIdleAndUsable / TestGoalStalledProviderBackoffSurfacesPaused (server/goal_paused_test.go:49/:161).

---

### Task 1: Engine — exit-park both tiers + goal.parked record + sentinel

engine/goal.go, engine/engine.go (event const/fields), engine/store.go (record + trace fold), tests. Invariants 1-3, 8 (engine halves). TDD red-first; red-verify invariant 1's headline against the pre-change clear. All timing synctest. Commit: `feat(engine): worker-turn exhaustion parks the goal instead of clearing`.

### Task 2: Server — outcome, pause arm, resume resets

server/journal.go (evt const, Publish route, both folds, outcomeWorkerParked in turnEndOutcome), server/server.go (tracker fields, pauseView arm, pauseReason const), server/handlers.go (compositeState, goalJSON, re-arm + auto-arm resets), openapi. Invariants 1 (server half), 2 (freed-slot proof), 4, 5, 7. Commit: `feat(server): worker-parked goals surface paused/worker_failure and resume on activity`.

### Task 3: Engine — ambient parked-goal segment

Session.goalParked lifecycle + third ambient occupant + tests (invariant 6). Commit: `feat(engine): ambient segment surfaces a worker-parked goal to the session`.

### Task 4: Docs, review, e2e, PR

AGENTS.md (goal-loop section: park-both-tiers, supersede the #61 in-loop-park description; the deliberate context-overflow asymmetry), docs/goal-loop.md new section, full gates, Opus review, live incident replay (stub provider returning 404s → park not clear → GET shows worker_failure → prompt the box → auto-arm resumes → flip healthy → achieves; also retryable-tier park under synctest-less live with short schedule if feasible, else covered by tests), PR referencing NEP-4849, converge, merge on approval, Linear closure.

## Execution notes

Synctest all timing; injectable seams; emit/lock discipline; Conventional Commits; no AI co-author lines.
