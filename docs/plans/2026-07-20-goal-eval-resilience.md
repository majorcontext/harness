# Goal Evaluator Resilience Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** A goal-evaluator failure must never kill a supervised run. Today ANY `evaluateGoal` error — unparseable output twice, or a transient provider 429/5xx — unconditionally clears the goal, emits `session.error`, and strands the session idle (production incident: two fleet boxes died mid-healthy-work). Evaluation becomes advisory: strict re-ask on unparseable, per-boundary `goal.eval_failed` + keep-armed + backoff, and a distinct loud terminal only after N consecutive failed boundaries.

**Architecture:** Mirror the worker-turn two-tier machinery (issue #61) on the evaluator path. In-boundary: retryable-class provider errors get the retryable backoff schedule before the boundary counts as failed; unparseable output gets one stricter re-ask (already 2 attempts; make attempt 2's prompt stricter). Failed boundary: journal `goal.eval_failed` (generation-gated), substitute an "evaluation unavailable" guidance reason, `continue` the loop (worker keeps working). Terminal: after N consecutive failed boundaries, clear with a dedicated reason + distinct `turn.end` outcome + `session.error` (loud, machine-distinguishable).

**Tech Stack:** Go, `testing/synctest` for all backoff timing, channel-gated fake providers.

---

## Locked design decisions

- **Evaluator failure never aborts work.** No `clearGoal`, no `session.error`, no loop exit on a failed boundary below the terminal horizon. The loop `continue`s — reusing the worker-park plumbing (turn accounting, queue drain at top, generation snapshot all fall out for free).
- **Two failure kinds, two treatments (mirroring `promptTurnWithRetry`):**
  - *Retryable-class provider error* from the evaluator call (`provider.AsRetryable` — the check the evaluator path is missing entirely today): retry the evaluator call in-boundary on the existing `goalRetryableBackoff` schedule (same constants), bounded by the existing retryable budget. Exhaustion → the boundary counts as failed.
  - *Unparseable output*: attempt 2 of the existing 2-attempt loop uses a STRICTER system prompt ("your previous reply could not be parsed; reply with EXACTLY `MET: <reason>` or `NOT MET: <reason>`, nothing else"). Still unparseable → the boundary counts as failed. (Keep harness's MET/NOT MET vocabulary — not the issue's ACHIEVED wording.)
  - Non-retryable provider errors (auth, 4xx): boundary counts as failed immediately (no in-boundary retry) — same as worker-turn treatment of non-retryable errors, minus the fatality.
- **Failed boundary:** journal durable `goal.eval_failed` (new record + event; carries turn, consecutive-failure count, error string; generation-gated exactly like `goal.eval` — never journaled for a stale generation, discarded silently on staleness). Then a short delay (`goalRetryDelay` schedule keyed on the consecutive count) and `continue`.
- **Directive after a failed boundary:** the next turn's guidance reason becomes a fixed evaluation-unavailable notice ("the evaluator could not render a verdict for the last turn; continue working toward the goal and finish it") with `reasonGen = snap.gen` — never silently repeat a stale NOT-MET reason from several turns back, never leak the evaluator's error text into the worker prompt.
- **Success resets the counter.** Any parsed verdict (MET or NOT MET) at a later boundary resets the consecutive-failure count to zero — the horizon is *consecutive* boundaries, not cumulative.
- **Terminal after N consecutive failed boundaries** (`goalEvalFailureLimit = 5`, named const): `clearGoal` with dedicated reason `"goal evaluator failed at N consecutive turn boundaries"` and return a distinct sentinel error type. The server maps it to a NEW `turn.end` outcome `evaluator_exhausted` (sibling of `context_exhausted`/`max_turns_exceeded` — consumers must never have to string-match `GoalReason`), still emits `session.error` (terminal must be LOUD — the incident was silence). At the terminal there is no in-flight worker turn, so nothing is aborted mid-work.
- **`context.Canceled` and the #77 stale/cleared race branches are unchanged.**
- **MaxTurns still bounds everything** — failed boundaries consume turns like retryable parks already do.
- **Surfacing:** `GoalSummary` gains `eval_failures` (current consecutive count, 0 omitted); the tracker folds `goal.eval_failed` (count from the record — idempotent for replay) and resets on `goal.eval`/`goal.achieved`/`goal.cleared`/`goal.updated`. No `pauseView` change — between failed boundaries the loop is genuinely running worker turns; presenting it as paused would be false.
- **Lock/fold discipline:** record persisted + event emitted under `s.mu` (house pattern); `publishGoal` and `foldGoalRecordLocked` updated in lockstep; engine `LoadSession` fold treats `goal.eval_failed` as a trace record (no resume-state change), like `goal.stalled`.

## Invariants (each gets a test)

1. Unparseable attempt 1 → attempt 2 uses the stricter prompt (assert the second request's system text differs and contains the strict instruction); parseable attempt 2 → normal verdict, no `goal.eval_failed`.
2. Unparseable twice → NO `goal.cleared`, NO `session.error`, goal still armed; `goal.eval_failed` journaled with count 1; loop continues; next turn's directive carries the evaluation-unavailable notice (and not the evaluator error text, and not a stale NOT-MET reason).
3. Retryable-class evaluator provider error → in-boundary retries on the retryable schedule (synctest: delays match `goalRetryableBackoff`), success mid-schedule → normal verdict, no failed boundary.
4. Retryable budget exhausted (or non-retryable error) → boundary fails: same as invariant 2's observable shape.
5. Consecutive counting: fail, fail, SUCCEED (parsed NOT MET), fail → counts go 1, 2, reset, 1 (assert via records); terminal never fires.
6. Terminal: `goalEvalFailureLimit` consecutive failures → `goal.cleared` with the dedicated reason, distinct sentinel error to the caller, session no longer goal-active; count of `goal.eval_failed` records equals the limit.
7. Generation guard: evaluator failure racing an `UpdateGoal` → no `goal.eval_failed` for the stale generation (existing stale-discard shape); counter untouched.
8. Server: terminal maps to `turn.end outcome=evaluator_exhausted` + `session.error`; a sub-terminal run of failures produces NEITHER while the loop runs.
9. `GET /session` shows `eval_failures` rising while boundaries fail and resetting on a parsed verdict; boot replay (journal rebuild) reproduces the same count.
10. `ClearGoal` (operator DELETE) mid-failing-boundaries still stops the loop cleanly (existing behavior preserved).

## Key facts from recon (file:line, this worktree)

- Fatal branch to replace: `engine/goal.go:676-728` (evaluator error → `clearGoal("goal evaluator failed: %v")` + `emitSessionError` + return). `errEvaluatorUnparseable` at :217; `evaluateGoal` 2-attempt loop at :1172-1183; `runEvaluator` :1187-1232; system prompt const :198-209; lenient parser :1237-1254.
- Worker-turn machinery to mirror: `promptTurnWithRetry` :832-901; `goalRetryDelay`/`waitGoalRetryBackoff` :267-298; `goalRetryableDelay`/`goalRetryableBackoff`/`waitGoalRetryableBackoff` :331-389 (5s→5min, 12 attempts, jittered); park pattern :622-649; `goalRetryableExhaustedError` :402-408; `provider.AsRetryable` provider/retryable.go:64-74.
- Reason pairing: `reason`/`reasonGen` :529-532, 577-584, 755-756; `goalAdjustedNotice` :1265.
- Server: `runGoal` error classification server/handlers.go:1426-1458; `turnEndOutcome` server/journal.go:218-228 (add the new outcome alongside `outcomeContextExhausted`); Publish switch :234-258; `publishGoal` :286-362 / `foldGoalRecordLocked` :751-804 (lockstep); `goalTracker` server/server.go:289-308; `goalJSON` server/handlers.go:194-237; openapi Event enum :623-638, GoalSummary :290-368.
- Tests changing meaning: `TestPursueGoalUnparseableTwice` (engine/goal_test.go:329), `TestPursueGoalUnparseableTwiceClearsGoal` (:361), `TestClearGoalDuringPendingEvaluatorFailureIsCleanStop` (:914 — its doc comment locks in the old asymmetry; rewrite deliberately). `TestPursueGoalUnparseableThenRecovers` (:414) survives but re-check against the stricter re-ask.
- Doc drift to fix while there: `docs/goal-loop.md:84-92` says evaluator path "unchanged" — stale relative to current code; update alongside AGENTS.md.

---

### Task 1: Engine — strict re-ask + non-fatal boundaries + terminal horizon

**Files:** `engine/goal.go`, `engine/engine.go` (event const/fields), `engine/store.go` (record + fold), tests in `engine/goal_test.go` + new `engine/goal_eval_resilience_test.go`.

All timing under synctest (backoff schedules are real waits). TDD strictly red-first per invariant, in this order: 1 (strict re-ask), 2 (non-fatal + eval_failed + notice), 3-4 (retryable classification), 5 (consecutive reset), 6 (terminal), 7 (gen guard), 10 (operator clear). Rewrite the three meaning-changing tests deliberately, noting each in the commit message. Red-verify invariant 2's headline test against the pre-change fatal path.

Commit: `feat(engine): goal evaluator failures are advisory — re-ask, eval_failed boundaries, bounded terminal`.

### Task 2: Server — outcome, folds, surfacing, contract

**Files:** `server/journal.go` (evt const, Publish route, both folds, `outcomeEvaluatorExhausted` in `turnEndOutcome` keyed on the engine's sentinel via `errors.As`/`Is`), `server/server.go` (tracker field), `server/handlers.go` (`goalJSON` `eval_failures`), `server/openapi.yaml` (Event enum + field docs + GoalSummary + turn.end outcome enum if enumerated), tests: new `server/goal_eval_resilience_test.go` (invariants 8, 9 — including journal rebuild) using the channel-gated goal-provider idioms.

Commit: `feat(server): surface goal.eval_failed and the evaluator_exhausted terminal`.

### Task 3: Docs + review + validation

`AGENTS.md` goal-loop section (evaluator resilience paragraph: advisory evaluation, N-boundary horizon, loud terminal); fix `docs/goal-loop.md`'s stale "unchanged" claim; hub check (unknown `goal.eval_failed` event must not break rendering — same three dispatch sites as prior PRs; add `eval_failures` display ONLY if trivially cheap, else skip). Full gates + `node --test tools/hub/*_test.mjs`. Then Opus full-branch review; then live e2e (drive a real serve with an evaluator pointed at a garbage-returning stub via openaicompat to force unparseable output against a REAL working model doing the work — prove the box keeps working through evaluator failure and terminates loudly at the horizon).

---

## Execution notes

- Backoff waits: `testing/synctest` bubbles only — no wall-clock sleeps in tests (AGENTS.md rule).
- Every new record: persist + emit while holding `s.mu`; both server folds in lockstep; gen-gated like `goal.eval`.
- Conventional Commits; no AI co-author lines.
