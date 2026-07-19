# Goal Self-Set/Adjust Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Let an agent set or adjust a goal on a busy session — including its own session from inside a running turn — instead of receiving 409.

**Architecture:** Three layers. (1) Engine: a new durable `goal.updated` record plus `Session.UpdateGoal`, and `PursueGoal` re-reads the condition at every turn boundary under a generation counter so a changed condition redirects the loop instead of killing it. (2) Engine: a new built-in `goal` session tool (`status`/`set`/`adjust`, no `clear`) that mutates goal state in-process, bypassing the HTTP run slot entirely. (3) Server: `POST /session/{id}/goal` on a busy session becomes an in-place update (goal running) or an arm-for-later (plain prompt running), backed by an auto-arm hook that spawns the goal loop when the current prompt finishes.

**Tech Stack:** Go, `testing/synctest` for timing-sensitive tests, table tests, golden folds.

---

## Locked design decisions (do not relitigate)

- **No self-clear.** The tool has no `clear` action. `DELETE /goal` remains the only clear path.
- **Verdict-generation invariant:** an evaluator verdict only counts for the condition generation it evaluated. A MET verdict for a stale generation is discarded; the loop continues against the new condition. Never journal `goal.achieved` or `goal.eval` for a stale generation.
- **Generation counter is runtime-only** (never persisted, never in records). Replay correctness comes from folding `goal.updated` into the condition.
- **Boot behavior unchanged:** restart still pauses armed goals (`pauseArmedGoalsAtBoot`); resume never auto-runs. Auto-arm fires only on live prompt completion in a running server.
- **Workdir-holder 409 is unchanged.** Only the same-session-busy case gets new semantics.
- **`max_turns` applies only when a loop starts.** The update path ignores it; the tool's `set` takes no max_turns (YAGNI — auto-armed loops run unlimited).
- **Lock order preserved:** `UpdateGoal` takes only `engine.Session.mu` and emits while holding it (matching `RegisterGoal`/`clearGoal`). It must never be called while holding `server.Server.mu` (leaf-order rule, `server/server.go:159-170`).
- **The three folds stay behavior-identical:** `engine/store.go` LoadSession fold, `server/journal.go` `publishGoal`, `server/journal.go` `foldGoalRecordLocked`.

## Invariants to test against (write these tests; they are the spec)

1. `UpdateGoal` on an inactive goal errors; on an active goal it rewrites the condition, journals `goal.updated`, and emits `EventGoalUpdated` while holding `s.mu`. Same-condition update is a silent no-op (nil error, no record).
2. A running `PursueGoal` observes an updated condition at the next turn boundary: the next worker directive (guidance template) and the next evaluator call both carry the new text.
3. A MET verdict computed against generation N is discarded if the generation is now N+1; the loop neither achieves nor journals `goal.eval` for it, and continues with the new condition.
4. `ClearGoal` still stops the loop cleanly at every point it does today (the generation machinery must not regress clear detection — the loop's "still active?" check keys on `goalActive`, not condition equality).
5. `LoadSession` replay of `goal.set` → `goal.updated`* → (nothing) restores `ActiveGoal() == (<last updated condition>, true)`.
6. Server journal rebuild (`foldGoalRecordLocked`) after `goal.updated` reports the new condition in `GoalSummary`.
7. `POST /goal` while a goal loop runs: 200, condition updated in place, no second loop, no run-slot claim.
8. `POST /goal` while a plain prompt runs and no goal is active: 202 (armed); when the prompt completes, the server auto-arms — claims the run slot and spawns the goal loop with the stored condition. Status events: the prompt's `idle` still precedes the goal's `busy`.
9. Auto-arm goes through `claimForPrompt`; a racing incoming prompt is resolved by the slot (either order is legal, no deadlock, no double loop).
10. The `goal` tool: present only when the host enables it; `set` arms via `RegisterGoal` (goal begins after the current turn via auto-arm); `adjust` rewrites via `UpdateGoal`; `status` reports `{active, condition}`; there is no `clear` action; `set` while a goal is active returns an error message telling the model to use `adjust`.
11. `DELETE /goal` clears an armed-but-not-running goal (nil `st.cancel` must be handled) and still clear-before-cancels a running one.

## Key facts from code recon (verify, then lean on)

- Run slot: `st.running` bool, `server/handlers.go` `claimForPrompt` (~line 1240); callers: `handlePrompt` (~734), `handleGoal` (~821), one more (~1129). Unclaim mirrors at tails of `runPrompt`/`runGoal` and two rollback branches in `handleGoal`.
- `PursueGoal` (`engine/goal.go:444`) captures `condition` as a parameter; `directive := condition` (~468); liveness check `goalActiveWith(condition)` (~470, 481, 575) does string equality `s.goalCondition == condition` — this is what conflates "cleared" with "changed" and must be replaced by an active+generation check.
- `evaluateGoal(ctx, condition, …)` (~540) and `goalGuidance(condition, reason)` (~596, template at ~993) both use the captured parameter — switch both to the per-turn snapshot.
- `RegisterGoal` (~836), `ClearGoal`/`clearGoal` (~802), `ActiveGoal` (~780) all lock `s.mu`, persist, and emit while holding it. `UpdateGoal` copies that shape exactly.
- Store fold: `engine/store.go` ~391-406 (`recGoalSet`/`recGoalAchieved`/`recGoalCleared`/`recGoalEval`/`recGoalStalled`).
- Server folds: `publishGoal` and `foldGoalRecordLocked` in `server/journal.go` — comments require them to mirror each other exactly; add the `goal.updated` case to both (update `g.condition` only; do not touch pause/retry fields).
- Built-in tool registration: `engine/engine.go:347-355` in `newSession`; template for a state-reading tool: `engine/session_info.go`. Tools run inside `runToolCall` (engine.go ~973) — in-process, inside the held run slot; `Session` methods they call do their own locking.
- Existing tests that change meaning: `server/goal_paused_test.go` `TestGoalReArmMismatchedConditionRejected` (mismatched re-arm now updates + resumes instead of 409) and `server/goal_test.go` `TestGoalBusyRejectsPromptAndGoal` (prompt-during-goal stays 409; goal-during-goal becomes 200 update).

---

### Task 1: Engine — `goal.updated` record + `Session.UpdateGoal`

**Files:**
- Modify: `engine/goal.go` (new method + record type near `RegisterGoal`)
- Modify: `engine/engine.go` (add `EventGoalUpdated = "goal.updated"` const; goal event fields already exist)
- Modify: `engine/store.go` (record type + LoadSession fold case)
- Test: `engine/goal_update_test.go` (new file)

**Steps (TDD, one behavior at a time — red, then green, then commit each):**
1. `TestUpdateGoalRequiresActive` — no active goal → error mentioning no active goal. Run `go test -race -run TestUpdateGoalRequiresActive ./engine/`, watch it fail (method undefined), implement minimal `UpdateGoal`, green.
2. `TestUpdateGoalRewritesConditionJournalsAndEmits` — register, subscribe to events, update; assert `ActiveGoal()` returns new condition, a `goal.updated` record with the new condition is in the log, and `EventGoalUpdated` was emitted. Follow `RegisterGoal`'s persist/emit-under-mu pattern exactly.
3. `TestUpdateGoalSameConditionNoop` — nil error, no new record, no event.
4. `TestUpdateGoalEmptyConditionRejected` — whitespace-only errors (mirror `RegisterGoal`'s validation).
5. `TestLoadSessionFoldsGoalUpdated` — write set + updated records via real methods, `LoadSession` from the store, assert `ActiveGoal()` shows the updated condition. Fold case: `recGoalUpdated` overwrites `goalCondition` (only meaningful while active; guard on `goalActive` like the surrounding cases behave).
6. `go test -race ./engine/` full package green. Commit: `feat(engine): goal.updated record and Session.UpdateGoal`.

### Task 2: Engine — generation-checked `PursueGoal`

**Files:**
- Modify: `engine/goal.go` (`goalGen uint64` on the goal state guarded by `s.mu`; bump in `RegisterGoal` and `UpdateGoal`; new locked snapshot helper returning `(condition string, gen uint64, active bool)`; loop rewrite; retire `goalActiveWith`'s equality semantics)
- Modify: `engine/engine.go` if the gen field lives on `Session`
- Test: extend `engine/goal_update_test.go`; touch existing `engine/goal_test.go` only where `goalActiveWith` semantics leak

**Loop contract:** at the top of each turn take a snapshot; `!active` → existing "goal cleared" exit. Turn 1 directive is the snapshot condition; later turns build `goalGuidance(snapshot.condition, reason)`. `evaluateGoal` uses the snapshot condition. After the evaluator returns, `recordGoalEval`/`achieveGoal` must (under `s.mu`) verify `goalActive && goalGen == snapshot.gen` before journaling/achieving; on mismatch, journal nothing, discard the verdict, continue the loop (fresh snapshot next iteration). `recordGoalStalled` gets the same guard. The existing "cleared raced the evaluation" behavior (return `Reason: "goal cleared"`) keys on `!goalActive` only.

**Steps:**
1. `TestPursueGoalPicksUpUpdatedConditionNextTurn` — scripted provider; between turn 1 and 2 call `UpdateGoal`; assert turn 2's guidance prompt and evaluator request contain the new condition (capture prompts in the fake provider). Red first: today the loop exits "goal cleared" on the very next check — the failing assertion should show that.
2. `TestStaleMetVerdictDiscarded` — evaluator call for gen N in flight (gate it on a channel per repo synctest rules), `UpdateGoal` lands, then the evaluator returns MET; assert no `goal.achieved`, no `goal.eval` record for the stale verdict, and the loop runs another turn against the new condition.
3. `TestClearGoalStillStopsUpdatedLoop` — update, then clear mid-loop; loop exits with the cleared reason (invariant 4).
4. Run the FULL existing `go test -race ./engine/` — the ~30 goal tests encode the state machine; any red there is a regression to fix, not a test to rewrite (except tests that assert the old equality-conflation directly — change those deliberately and say so in the commit message).
5. Commit: `feat(engine): PursueGoal re-reads goal condition each turn under a generation guard`.

### Task 3: Engine — `goal` session tool

**Files:**
- Create: `engine/goal_tool.go` (+ `engine/goal_tool_test.go`)
- Modify: `engine/engine.go` (config gate + registration in `newSession`; follow `session_info.go`'s shape)
- Modify: `engine/config.go` or wherever `Config` lives — add the enable flag (suggested: `GoalTool bool`)

**Tool contract:** name `goal`. Input schema: `{action: "status"|"set"|"adjust", condition?: string}`. `status` → `{active, condition}`. `set` → `RegisterGoal`; on "already active" error, return a tool-result error telling the model to use `adjust`; description must say the goal begins after the current turn ends. `adjust` → `UpdateGoal`. No `clear` action — the description states clearing is operator-only. Registered only when `Config.GoalTool` is true.

**Steps:** TDD each action (`TestGoalToolStatus`, `TestGoalToolSetArms`, `TestGoalToolSetWhileActiveSaysAdjust`, `TestGoalToolAdjust`, `TestGoalToolRejectsUnknownAction`, `TestGoalToolAbsentWhenDisabled` — assert the tool is not in the advertised defs when the flag is off). Commit: `feat(engine): goal session tool (status/set/adjust)`.

### Task 4: Server — journal folds for `goal.updated`

**Files:**
- Modify: `server/journal.go` (`publishGoal` + `foldGoalRecordLocked`: `EventGoalUpdated`/its record → `g.condition = new`; nothing else)
- Modify: `server/server.go`/`handlers.go` where engine events map to journal event types, if an explicit allowlist exists
- Test: `server/goal_test.go` or a new `server/goal_update_test.go` — `TestGoalUpdatedFoldRebuild`: journal set + updated, rebuild, `GET /session` `GoalSummary.condition` is the new text; and live-path equivalent via `publishGoal`.

Commit: `feat(server): fold goal.updated into the goal tracker (live + rebuild)`.

### Task 5: Server — `POST /goal` update/arm semantics + auto-arm

**Files:**
- Modify: `server/handlers.go` (`handleGoal`, `runPrompt` tail, new `maybeAutoArmGoal`)
- Modify: `server/openapi.yaml` (startGoal: add 200-updated and 202-armed semantics; add a `status` field — `"started" | "armed" | "updated"` — to the response body; narrow the 409 description to workdir-holder and prompt-during-goal cases)
- Test: `server/goal_update_test.go`, adjust `server/goal_test.go`, `server/goal_paused_test.go`

**`handleGoal` new flow:** validate as today → `claimForPrompt`. On success, keep today's paths with ONE change: the paused-re-arm branch with a *different* condition calls `UpdateGoal(new)` and resumes with it (response `status: "updated"`... then started — use `"started"` with the new condition; pick one and document in openapi) instead of 409. On 409-busy with empty holder: `ActiveGoal()` active → different condition ? `UpdateGoal` → 200 `{status:"updated"}` : 200 no-op same shape; not active → `RegisterGoal` → then immediately retry `claimForPrompt` ONCE (closes the freed-slot race): success → spawn `runGoal` now, 202 `{status:"started"}`; still busy → 202 `{status:"armed"}` (auto-arm will attach). On 409 with non-empty holder: unchanged 409.

**Auto-arm:** at the tail of `runPrompt` (after the unclaim and `idle` emit), if the server's evaluator is configured and `st.sess.ActiveGoal()` is active, call `maybeAutoArmGoal`: `claimForPrompt` (loser of a race just returns), emit `busy`, spawn `runGoal` with the stored condition, `MaxTurns` 0. Do NOT add this to `runGoal`'s tail (a finishing goal loop with the goal still active cannot happen — achieved/cleared/error all end it — and adding it would risk a spin; assert that reasoning in a comment).

**Steps (each red first):**
1. `TestGoalPostWhileGoalRunningUpdatesInPlace` (invariant 7).
2. `TestGoalPostWhilePromptBusyArmsThenAutoStarts` (invariant 8; block the prompt on a channel-gated fake provider, POST goal, release, observe `goal.set`… then loop start; collect-until-idle ordering).
3. `TestGoalToolSetAutoArmsAfterPrompt` — prompt whose scripted tool call invokes the `goal` tool `set`; after the prompt ends the loop starts (this is the headline user story: an agent sets its own goal mid-turn).
4. `TestAutoArmRaceWithIncomingPrompt` — synctest/channel-ordered: whichever claims first wins, the other 409s or queues per existing rules; no deadlock, no two loops.
5. `TestDeleteGoalClearsArmedNoLoop` — nil `st.cancel` path (invariant 11).
6. Rewrite `TestGoalReArmMismatchedConditionRejected` → `TestGoalReArmDifferentConditionUpdatesAndResumes`; narrow `TestGoalBusyRejectsPromptAndGoal` to prompt-during-goal.
7. Wire `Config.GoalTool = true` in server session construction when `opts.GoalEvaluator` is configured (and in `harness run` when `-goal`/evaluator config present — check `cmd/harness`).
8. `go test -race ./...` green. Commit: `feat(server): POST /goal updates or arms busy sessions; auto-arm after prompt`.

### Task 6: Docs + contract sweep

**Files:**
- Modify: `AGENTS.md` (goal loop section: adjustable conditions, the tool, auto-arm, no-self-clear rationale)
- Modify: `server/openapi.yaml` already in Task 5 — re-verify descriptions match final behavior
- Check (read-only): `tools/hub/index.html` tolerates an unknown `goal.updated` SSE event (it should ignore unknown types; if it hard-fails, minimal fold: update displayed condition)
- Run: full `go test -race ./...`, `go vet ./...`, `node --test tools/hub/*_test.mjs` if hub touched.

Commit: `docs: goal self-set/adjust semantics`.

---

## Execution notes for the implementer

- **Red-verify every regression-shaped test**: run it against pre-change code (stash/revert) at least for Task 2's headline tests, per AGENTS.md's red-verification rule.
- **No raw `time.Sleep` anywhere; no guessed deadlines.** Channel-gate fake providers; synctest bubbles for anything timing-adjacent.
- **Emit-under-mutex discipline**: every new journal/emit follows the existing "emit while holding s.mu" comments; the lock-order test `TestGoalEmitVsSyncMessagesNoDeadlock` must stay green.
- Conventional Commits, no AI co-author lines.
