# MCP Init Resilience Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** An MCP server that fails `initialize` at first use must not be silently dropped for the process lifetime. Production incident: 5 of 16 remote servers timed out on a cold-start burst (identical 15s defaults, simultaneous start), were dropped forever with only a serve-log line, and the agent had no in-band signal — it concluded "I have no Linear access."

**Architecture:** Three independent pieces. (1) Config surface: per-server `connect_timeout_s` in `config.MCPServerSpec`, threaded to the existing (already-honored, already-tested) `engine.MCPServerConfig.ConnectTimeout`. (2) Retry: replace `MCPManager`'s process-lifetime `connectOnce` with per-server state (connected/failed/retrying) and a detached background retry goroutine per failed server (capped exponential backoff, indefinite — the `process.Manager` detached-goroutine precedent); `Tools()`/`CallTool()` read live state, so recovered tools appear on the very next turn of every live session via the existing per-request `toolDefs` assembly. (3) Surfacing: an ambient `[mcp: ...]` status block, byte-for-byte structurally mirroring `processStatusSegment` — computed fresh each `streamTurn`, appended to the newest user message only, present ONLY while at least one server is degraded, self-correcting as retries succeed. Never persisted, never touches earlier messages (prompt-cache prefix intact).

**Tech Stack:** Go, `testing/synctest` for all backoff timing, `httptest` fake MCP servers (existing `fakeMCPHTTPServer` idiom).

---

## Locked design decisions

- **Init stays lazy and parallel with per-server deadlines** (it already is — the incident's same-second cluster was identical defaults, not a shared context). First `Tools()` call still blocks up to the per-server timeout for the FIRST attempt (bounded, existing behavior); all retries are background-only, detached via `context.WithoutCancel` (same discipline `ensureConnected` already uses), zero cost on serve boot or first paint.
- **Retry is indefinite with capped backoff** (schedule mirroring the goal retryable shape: ~1s doubling to a 5min cap, jittered via an injectable jitter func for synctest). No "given up" state — the status block keeps the degradation honest, and a fleet box that recovers its proxy after 20 minutes should heal without a respawn. Attempts/last-error tracked per server.
- **Recovered tools appear mid-session automatically**: `Tools()` returns the live merged set under `m.mu`; no session/engine.go changes for registration (per-request `toolDefs` already re-reads). `CallTool`/`CallServerTool` against a currently-failed server return a clear error naming the state ("failed to initialize, retrying") — distinguishable from "not configured".
- **The exactly-once invariant changes deliberately**: `connectOnce` becomes per-server first-attempt-once + retry state. Tests that pin exactly-once semantics (`TestMCPManagerConnectsOnce`, Close-race tests) are rewritten to pin the NEW invariant: a HEALTHY server is listed exactly once and never re-probed; only failed servers retry. `Close()` must stop all retry goroutines promptly (no goroutine leaks — synctest bubbles will catch them).
- **Surfacing is the ambient status block, not a system-prompt segment and not engine events.** Segment caching can't track recovery; event fan-out from a process-shared singleton has no precedent in this codebase (process.Manager doesn't emit either). The block lists each degraded server with a short error class and retry posture, e.g. `[mcp: unavailable — linear (initialize timed out; retrying), ashby (auth error; retrying)]`. Absent entirely when all servers are healthy (zero happy-path cost). Serve-log warnings stay.
- **Config field naming/units follow existing config conventions** (check how other duration-ish fields are expressed in config/config.go — match; suggested `connect_timeout_s` integer seconds if no precedent dictates otherwise). Zero/absent = engine default (15s). Applies to both stdio and HTTP servers.
- **No server/HTTP-API/openapi changes.** This is engine + config + cmd wiring only. GET /session is untouched (the status block is in-band to the model; operators have serve logs and can see agent behavior).
- **Lock discipline**: all per-server state under `m.mu`; no lock held across network calls (connect attempts run outside the lock, results committed under it — study how ensureConnected's goroutines already commit under mu).

## Invariants (each gets a test)

1. Config `connect_timeout_s` on an MCP server spec round-trips to `engine.MCPServerConfig.ConnectTimeout` through `buildMCPManager`; absent → zero → engine default.
2. A server failing its first connect is retried in the background on the capped schedule (synctest: assert delay sequence); a mid-schedule success populates its tools and a subsequent `Tools()` call includes them — WITHOUT any new session or explicit trigger.
3. A healthy server is initialized exactly once — background retries never re-probe connected servers, and repeated `Tools()` calls never re-attempt anything (the old exactly-once test's concern, restated for the new state machine).
4. `CallServerTool` on a failed-but-retrying server errors with the retrying-state message; after recovery the same call succeeds.
5. `Close()` during an in-flight backoff wait or connect attempt terminates promptly, leaks no goroutines (synctest bubble exit is the leak detector), and prevents further retries.
6. The status block: absent when all healthy; present listing exactly the degraded servers while any are down; disappears from the NEXT request after recovery (fresh-per-turn, self-correcting); appended only to the newest user message (prompt-cache prefix of earlier messages byte-identical — mirror the existing processStatusSegment test's assertions).
7. First-caller semantics preserved: the first `Tools()` call blocks at most the per-server timeout (existing `TestMCPManagerConnectSurvivesFirstCallerCancellation` behavior intact).
8. Multiple servers failing simultaneously retry independently (one server's long backoff doesn't delay another's).

## Key facts from recon (file:line, this worktree)

- Init loop + per-goroutine deadline: `engine/mcp.go:154-213` (`ensureConnected`), `connectMCPServer` :219-251, `defaultMCPConnectTimeout = 15s` :60, failure log line :176. `connectOnce` :123. Engine `ConnectTimeout` field :79-81 already honored + tested (`TestMCPManagerConnectTimeoutFailsOpen`, engine/mcp_test.go:274-300).
- Config gap: `config.MCPServerSpec` config/config.go:145-159 (no timeout field); `buildMCPManager` cmd/harness/mcp.go:28-43 (doesn't thread it).
- Per-request tool assembly: `toolDefs` engine/engine.go:1063-1081 called from streamTurn :876 — late tools arrive next turn automatically.
- Failed-server call error: engine/mcp.go:284-286 (conflates not-configured with failed — split the message).
- Ambient block precedent: `processStatusSegment` engine/process.go:296-322, wired at engine.go:869-871 (`withProcessStatus`); mirror both the function shape and the wiring; note its "computed fresh every call / never persisted" doc.
- Detached-goroutine precedent: process/process.go:508 (waiter goroutines updating shared state under mu); `context.WithoutCancel` discipline engine/mcp.go:143-153.
- Test idioms: `fakeMCPHTTPServer` engine/mcp_test.go:48-122; hang simulation via channel closed in t.Cleanup (:274-300); `TestMCPManagerConnectsOnce` :305-333 (rewrite deliberately); mcp/stdio_test.go net.Pipe + synctest idioms.
- Startup budget: AGENTS.md — nothing heavy at boot; current laziness must be preserved (NewMCPManager stays network-free).

---

### Task 1: Config surface + engine retry state machine

**Files:** `config/config.go` (+ its test), `cmd/harness/mcp.go` (+ test), `engine/mcp.go`, `engine/mcp_test.go`.

Invariants 1, 2, 3, 4, 5, 7, 8. TDD red-first per invariant; all backoff timing in synctest bubbles with an injectable jitter/sleep seam (study how goal.go's `goalJitterFunc` seam works and mirror). Rewrite `TestMCPManagerConnectsOnce` and any Close-race tests to the new invariant, saying so in the commit message. Red-verify invariant 2's headline test (pre-change: a failed server stays failed forever — assert tools never appear).

Commit: `feat(engine,config): MCP servers retry initialize with backoff; per-server connect_timeout_s`.

### Task 2: Ambient degraded-MCP status block

**Files:** `engine/mcp.go` (Status() on the manager/registry interface), new `engine/mcp_status.go` (or alongside — follow process.go's layout), `engine/engine.go` (one append alongside processStatusSegment), tests mirroring the process-status tests.

Invariant 6. The block must read live state under mu, render deterministically (sorted server names), and include a one-clause hint the model can act on ("tools from these servers are temporarily unavailable; they may return later in this session"). Red-verify: pre-change, a degraded server produces no in-band signal.

Commit: `feat(engine): ambient status block surfaces degraded MCP servers to the session`.

### Task 3: Docs + review + validation

`AGENTS.md` (MCP bullet in External Protocol Surfaces or a short new paragraph — document retry/backoff, config field, status block, and the changed not-configured-vs-failed error split); check `docs/` for any MCP design doc to update. Full gates. Then Opus full-branch review; then live e2e: real `harness serve` + real worker model + a stub MCP HTTP server that (A) times out initialize for the first ~30s then recovers — prove the agent sees the status block, tools absent, then present mid-session after recovery; (B) config `connect_timeout_s` honored (set 2s, observe fast failure); (C) healthy-path unchanged (no block). Then PR (Fixes nothing app-side), converge review rounds, merge on user approval.

---

## Execution notes

- Backoff/timing: synctest only; injectable jitter; no wall-clock sleeps in tests.
- No lock across network I/O; commit results under mu (existing pattern).
- Conventional Commits; no AI co-author lines.
