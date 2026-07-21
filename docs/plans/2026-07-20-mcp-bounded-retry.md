# Bounded MCP Retry + `mcp` Session Tool Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace #82's indefinite background MCP retry with a bounded schedule (3 background attempts after the failed first connect), park the server visibly, and add an `mcp` session tool (`status`/`connect`) so the model — prompted by the ambient block — can retrigger connection on demand. Matches Claude Code's bounded-effort-then-explicit-retrigger shape (operator decision).

**Architecture:** The `retryServer` loop gains `mcpRetryMaxAttempts = 3`: after the 3rd failed background retry it logs, marks the entry parked (state visible via `Status()`), and exits — Close/leak semantics unchanged. The ambient block renders parked servers with a "use the mcp tool to reconnect" clause instead of "retrying". New built-in `mcp` session tool (template: the `goal` tool — in-process, registered in `newSession` when the session's MCP registry has ≥1 configured server): `status` returns per-server `{name, connected, attempts, reason}` (classified reasons only — never raw errors); `connect {server}` runs ONE synchronous bounded attempt (per-server `connect_timeout_s`) and commits tools on success exactly like a background-retry success (next `toolDefs` read sees them — same turn's remaining tool calls can even use them).

**Tech Stack:** Go, `testing/synctest`, existing `fakeMCPHTTPServer`/seam idioms.

---

## Locked design decisions

- **`mcpRetryMaxAttempts = 3`** background retries after the failed first attempt (4 total automatic tries; ≈1s+2s+4s jittered ≈ under 10s of background effort). Named const, documented; not config-surfaced (YAGNI — the tool is the escape hatch).
- **Parked is a first-class visible state**: `MCPServerStatus` gains `Parked bool` (or equivalent — attempts exhausted, no goroutine alive). Ambient block clause becomes e.g. `linear (initialize timed out; use the mcp tool action "connect" to retry)`. Block still lists it (still degraded), still classified-error-only.
- **The `mcp` tool**: name `mcp`, input `{action: "status"|"connect", server?: string}`. `status` → all configured servers with classified state (works even when everything is healthy — useful introspection, and the model may call it unprompted). `connect` requires `server`; on an already-connected server → friendly no-op result ("already connected"); on unknown server → error listing configured names; otherwise ONE attempt, synchronous, bounded by the server's connect timeout; success → tools committed under mu + result says so; failure → classified error in the result, server stays parked, model may call again. NO indefinite anything.
- **Concurrency**: a tool-triggered connect must not race the background retry loop or another concurrent tool call into a double-connect — per-server in-flight guard under `m.mu` (attempt-in-progress flag; concurrent `connect` returns "attempt already in progress"). A tool connect while background retries are still running (not yet parked): allowed — the in-flight guard serializes; whoever wins commits, the other observes `Connected` and no-ops. Success from ANY path is the same one-way latch.
- **Tool registration**: in `newSession` when the config's MCP registry reports ≥1 configured server (add a cheap `Names()`/count accessor if none exists — must NOT trigger a connect). Not gated on any new config flag.
- **`Close()` semantics unchanged**: parked servers have no goroutine; an in-flight tool connect must respect `retryCtx` cancellation like the background path (reuse the same commit-declined-then-self-close discipline).
- **Model-visible strings stay classified** (the #82 leak rule applies to every new surface: tool results included).
- **Docs**: AGENTS.md MCP paragraph updated (bounded schedule, parked state, the tool); engine/mcp.go package doc updated. This PR supersedes the "indefinite" wording everywhere.

## Invariants (each gets a test)

1. Background retries stop after exactly `mcpRetryMaxAttempts`: synctest — delays 1s/2s/4s (jittered windows), then the goroutine exits (bubble leak-check proves it), entry parked, `Status()` shows it, no further connect attempts ever fire spontaneously.
2. Ambient block for a parked server carries the mcp-tool hint (and "retrying" before parking); still no raw errors/URLs.
3. Tool `status`: healthy, retrying, and parked servers each render correctly; no raw error text.
4. Tool `connect` on a parked server: failure → classified error result, still parked; success (fake flips healthy) → tools present in the session's very next `toolDefs`/provider request, block gone next request.
5. Tool `connect` no-ops on already-connected; errors helpfully on unknown server names.
6. Double-connect impossible: concurrent tool connects (and tool-vs-background race before parking) serialize via the in-flight guard; exactly one commit; loser reports cleanly. synctest.
7. `Close()` during a tool-triggered in-flight connect: no leak, no commit-after-close (mirror the background test).
8. Tool absent when no MCP servers configured; present (status works) when configured-and-all-healthy.

## Key facts (this worktree, post-#82 main)

- `retryServer` loop + `waitMCPRetryBackoff` + seams (`mcpConnectFunc`, `mcpJitterFunc`, `mcpTestRetryCommitted`): engine/mcp.go (grep). Commit-declined-on-Close discipline lives in the loop's locked section — reuse for the tool path (factor a shared `attemptAndCommit(name) (ok bool, err error)` if it keeps the two paths identical).
- Status/block: `MCPServerStatus`, `Status()`, `mcpStatusSegment`/`formatMCPServerStatus` (engine/mcp_status.go), classifier `classifyMCPConnectError` + `sanitizeMCPCallError` (engine/mcp.go, engine/mcp_classify_test.go idioms).
- Tool template: engine/goal_tool.go (schema/actions/result shapes, gating in newSession, tests in engine/goal_tool_test.go).
- Session's MCP handle: `s.cfg.MCP` (`MCPRegistry` interface) — the tool needs manager-level operations (`Status`, a new `Connect(name)`) — decide interface placement the way `mcpStatusReader` was done (narrow optional interface, type-asserted) so existing fakes keep compiling.
- Test idioms: engine/mcp_test.go (fakeMCPHTTPServer, synctest bubbles, stress patterns), engine/mcp_status_test.go, goal_tool_test.go.

---

### Task 1: Bound the schedule + parked state + block text

engine/mcp.go (`mcpRetryMaxAttempts`, loop exit, `Parked` in status), engine/mcp_status.go (parked clause), tests (invariants 1, 2; adjust any existing test that assumed indefinite retry — deliberately, noted in commit). Red-verify invariant 1 (pre-change: attempts continue past 3). Commit: `feat(engine): bound MCP background retries; parked servers surface the mcp tool hint`.

### Task 2: The `mcp` session tool

engine/mcp_tool.go + test (invariants 3-8), `Connect(name)` manager method with in-flight guard (invariants 6, 7), registration in newSession + narrow interface, goal-tool-style schema/results. Red-verify invariant 4's success case (pre-change: no tool exists; post-Task-1-only: parked forever). Commit: `feat(engine): mcp session tool — status and on-demand connect`.

### Task 3: Docs, review, e2e, PR

AGENTS.md + engine/mcp.go doc comment (supersede "indefinite"), full gates, Opus review, live e2e (stub hangs → observe 3 retries then park + block hint → prompt the model to reconnect via the tool while stub healthy → tools usable same session; also tool status in healthy state), PR referencing #82 and NEP-4814, converge, merge on approval.

## Execution notes

Synctest for all timing; injectable seams; emit/lock discipline as established; Conventional Commits; no AI co-author lines.
