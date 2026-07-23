// Package server is the HTTP+SSE surface that `harness serve` exposes inside a
// sandbox. It is a protocol, not a product: a single external orchestrator
// drives many harness instances through it (see server/openapi.yaml).
//
// The server owns an orchestrator-facing event journal (<SessionDir>/events.jsonl):
// an append-only log of durable records — session lifecycle, canonical
// messages, model swaps, status changes — each carrying a global monotonic
// sequence number. Live deltas (text, reasoning, tool progress) stream between
// durable records over SSE but are never journaled. A client that reconnects
// with from=<last seq> replays exactly what it missed.
//
// The engine is injected (Options.NewSession / Options.LoadSession) so the
// server has no opinion on provider wiring; tests use scripted providers.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

// Workdir isolation modes for POST /session's workdir_isolation field (see
// handleCreate). isolationShared is the default: today's behavior, a session
// runs its tools directly in the resolved workdir and is subject to the
// workdir-busy claim in claimForPrompt. isolationWorktree gives the session
// its own git worktree (see worktree.go) so two sessions never contend for
// the same tree, structurally rather than by convention.
const (
	isolationShared   = "shared"
	isolationWorktree = "worktree"
)

// Goal "paused" reasons (see goalTracker.pauseView and GoalSummary.
// pause_reason): "restart" is a boot-time observation (pauseArmedGoalsAtBoot)
// that a goal is armed with no loop attached; "worker_failure" (NEP-4849,
// Task 2) is a LIVE observation that a worker turn exit-parked the goal
// (engine/goal.go's goal.parked, either exhaustion tier) — the loop itself
// has exited, mirroring the restart case's "no loop attached" shape even
// though this server never restarted; "provider-backoff" mirrors the
// existing retryable-backoff park machinery in engine/goal.go (observability
// only — see goalTracker.pauseView).
const (
	pauseReasonRestart         = "restart"
	pauseReasonWorkerFailure   = "worker_failure"
	pauseReasonProviderBackoff = "provider-backoff"
)

// Options configures a Server. RunToken, NewSession, and LoadSession are
// required.
type Options struct {
	// SessionDir is the session log directory. events.jsonl (the durable
	// journal) lives here alongside the per-session <id>.jsonl logs. Empty
	// keeps the journal in memory only (no durability).
	SessionDir string
	// RunToken authenticates every request except /health, compared in
	// constant time.
	RunToken string
	// Version is reported by /health.
	Version string
	// NewSession creates a fresh engine session for the given model (zero
	// model = caller's default), workdir (already resolved and validated
	// by handleCreate against WorkspaceRoots — see resolveWorkDir), and
	// parentSession (already validated by handleCreate — see
	// validateParentSession; empty when POST /session omitted it). The
	// wrapper is expected to wire engine.Config.OnEvent to Server.Publish,
	// engine.Config.WorkDir to the workDir argument, and
	// engine.Config.ParentSession to the parentSession argument.
	NewSession func(model message.ModelRef, workDir string, parentSession string) (*engine.Session, error)
	// LoadSession resumes an on-disk session by ID, wiring OnEvent the same
	// way. It returns an error when no log with that ID exists. The
	// session's workdir is restored from its log header (see
	// engine.LoadSession), not passed in here.
	LoadSession func(id string) (*engine.Session, error)
	// WorkspaceRoots bounds the directories POST /session may accept as an
	// explicit workdir: the request value must clean-resolve (absolute,
	// cleaned) to one of these roots or a descendant of one. Empty means the
	// server process's own working directory is the sole allowed root. A
	// request that omits workdir always defaults to the process's current
	// working directory, which is never itself checked against this list.
	WorkspaceRoots []string
	// HeartbeatInterval is the SSE keep-alive comment period; 0 defaults to
	// 30s.
	HeartbeatInterval time.Duration
	// MaxResident caps the number of in-memory (resident) sessions. After a
	// prompt completes, the longest-idle non-busy sessions beyond this cap are
	// unloaded from memory; they remain listable and promptable via a
	// transparent reload from disk. 0 defaults to 32.
	MaxResident int
	// GoalEvaluator is the model ref used to evaluate goal completion for the
	// POST /session/{id}/goal endpoint. It is resolved by the caller from
	// config (goal_evaluator_model). When zero, goal requests are rejected with
	// 400 (there is no default evaluator).
	GoalEvaluator message.ModelRef
	// CORSOrigin, when non-empty, enables browser CORS support. Its literal
	// value is echoed in the Access-Control-Allow-Origin header on every
	// response (including 401s, so a browser can read the error), and "*" is
	// honored as-is. OPTIONS preflight requests to any route are answered 204
	// without authentication (preflights carry no credentials by spec). Empty
	// (the default) disables CORS entirely — no CORS headers are emitted.
	CORSOrigin string
	// OnError, when non-nil, is invoked for every error that the server would
	// otherwise swallow: journal marshal/write failures and per-session engine
	// persist failures (surfaced once per newly-changed error, not on every
	// poll). Errors are wrapped with context (e.g. "journal write: %w",
	// "session %s persist: %w"). Nil is safe — every call site nil-guards it.
	//
	// It is invoked synchronously and may run with locks held, so the handler
	// must not call back into either the Server or the Session — doing so
	// deadlocks. Specifically: the journal-write-failure path calls it while
	// s.mu is held (see writeJournalLocked); and although the session
	// persist-error path releases s.mu first (see the lock-ordering comment on
	// syncMessages), a journal write reached via a goal.* event runs while the
	// emitting Session's own mutex is held (RegisterGoal/recordGoalEval/
	// achieveGoal/ClearGoal emit under Session.mu). Logging or forwarding to an
	// external sink is the intended use.
	OnError func(context.Context, error)
	// OnCreatePhase, when non-nil, is invoked once per completed phase of
	// handleCreate's success path — "new_session", "persist", "register" (the
	// in-memory session-map insert), "emit_created", and finally "total"
	// (elapsed from handler entry to just before the response is written).
	// Reported only on the success path — a failure at any phase returns an
	// error response without reporting that or any later phase, so the
	// handler's error paths are entirely unchanged. sessionID is the ID
	// minted by NewSession, carried on every phase report including
	// "new_session" itself, since a failed NewSession call is never reported.
	// Called synchronously; keep it fast, mirroring OnError's rules above.
	OnCreatePhase func(sessionID, phase string, elapsed time.Duration)
	// MCP is the MCP client integration shared by every session this server
	// hosts (see engine.MCPRegistry): it is the same *engine.MCPManager the
	// NewSession/LoadSession wrapper wires into each session's
	// engine.Config.MCP, injected here too so ClientAPI.MCPCall (a
	// plugin-initiated client/mcp.call, which names a server and tool
	// directly rather than a session) reaches the exact same connected
	// clients without needing a session lookup. Nil disables MCP entirely
	// for plugin calls, matching a nil engine.Config.MCP.
	MCP engine.MCPRegistry
	// Processes is the managed-process integration shared by every
	// session this server hosts (see engine.ProcessRegistry): the same
	// *process.Manager the NewSession/LoadSession wrapper wires into each
	// session's engine.Config.Processes, injected here too so GET
	// /process and POST /process/{name}/start|stop|restart can answer
	// without a session lookup — process state is box-scoped, not
	// per-session. Nil disables the /process endpoints entirely (they
	// 404), matching a nil engine.Config.Processes.
	Processes engine.ProcessRegistry
}

// Server implements http.Handler for the harness serve API.
type Server struct {
	opts Options
	mux  *http.ServeMux

	// wg tracks in-flight runPrompt goroutines. They are decoupled from their
	// HTTP handlers (the 202 returns immediately), so http.Server.Shutdown does
	// not wait for them; Drain does, via this group.
	wg sync.WaitGroup

	// closing is closed exactly once, at the top of Drain, to signal that
	// shutdown has begun. Connected SSE streams select on it and return
	// promptly so http.Server.Shutdown sees idle connections; disconnected
	// orchestrators recover the records they miss via replay-from-seq.
	closing   chan struct{}
	closeOnce sync.Once

	// mu guards everything below. Lock-ordering invariant: mu is a LEAF with
	// respect to a session's own mutex — code holding mu must never call a
	// *engine.Session method that acquires the session's mutex (History,
	// PersistErr, Persist, RegisterGoal, ClearGoal, ...). The engine emits
	// goal.* (and other) events while the session's mutex is held (see
	// engine/goal.go), and those events flow into Publish, which acquires mu:
	// that is the session.mu -> server.mu order. Acquiring mu -> session.mu
	// anywhere would form the opposite order and deadlock the two together
	// (see journal.go's syncMessages and TestGoalEmitVsSyncMessagesNoDeadlock
	// in lockorder_test.go). Read session state in an unlocked window, then
	// re-acquire mu only for this server's own bookkeeping.
	mu       sync.Mutex
	draining bool                     // set once by Drain; gates prompt admission
	seq      int64                    // global monotonic durable sequence
	journal  []Event                  // in-memory durable records, for replay
	jf       *os.File                 // events.jsonl handle (nil when disabled)
	lastErr  error                    // most recent journal write failure
	subs     map[*subscriber]struct{} // connected SSE clients
	// seen maps session ID -> journaled message IDs; it is authoritative for
	// journal idempotency (syncMessages skips already-journaled IDs), so it is
	// never evicted when resident sessions are unloaded for MaxResident. It is
	// bounded by the number of message IDs, which are small, so retaining it for
	// unloaded sessions is cheap and keeps replay/reconcile correct.
	seen     map[string]map[string]bool
	sessions map[string]*sessionState // in-memory (resident) sessions

	// lastRequest holds the latest fully-assembled model request per session,
	// in memory only (never persisted): GET /session/{id}/request reads it, and
	// its absence is the 404 for a session that has not prompted this process.
	// lastReqHash carries the previous request's system-segment hash per session
	// so request.meta includes the full system only when it changes.
	lastRequest map[string]*requestSnapshot
	lastReqHash map[string]string

	// lastPersistErr tracks, per session, the Error() string of the last
	// engine persist failure forwarded to Options.OnError, so a repeatedly-
	// failing persist is reported once rather than on every syncMessages
	// poll. Never evicted (bounded by session count, mirrors seen).
	lastPersistErr map[string]string

	// goalState tracks the latest goal summary per session for this process
	// (in memory only, like lastRequest): condition, active flag, turn count,
	// and last evaluator reason. It drives the Session JSON goal field and is
	// updated as goal.* events flow through Publish.
	goalState map[string]*goalTracker

	// lastTurn tracks the most recent turn.end outcome per session, for this
	// process only (in memory, like goalState): drives Session.last_turn and
	// the /session/status last_turn field. Set by recordTurnEnd whenever a
	// prompt (runPrompt) or a goal worker loop (runGoal) finishes — the
	// durable turn.end record it also emits is the replayable wire form of
	// the same information.
	lastTurn map[string]*turnOutcome

	// waiters holds every in-flight GET /session/{id}/wait long-poll,
	// registered for the duration of the request. notifyWaitersLocked (see
	// journal.go) wakes matching waiters after every durable event so a
	// waiter re-checks its condition instead of the server polling for it.
	// Caller of any read/write here must hold mu.
	waiters map[*waiter]struct{}

	// goalDeleteRace is a test-only seam: when non-nil, handleGoalDelete
	// invokes it after clearing the goal but before its own call to cancel,
	// passing the loop's cancel func so a test can trigger it early — the
	// earliest structurally possible point — and ride out the worker's
	// unwind to completion (an idempotent second call from the handler
	// follows and is a no-op), forcing the worst-case interleaving
	// deterministically on every run rather than conditionally (see
	// TestGoalDeleteClearBeforeIdleRace). Always nil in production.
	goalDeleteRace func(cancel context.CancelFunc)

	// autoArmRace is a test-only seam: when non-nil, maybeAutoArmGoal invokes
	// it right before its own claimForPrompt call, letting a test force a
	// real concurrent prompt_async (or another POST /goal) to race the
	// auto-arm claim deterministically instead of relying on an unobserved
	// goroutine-scheduling coin flip (see TestAutoArmRaceWithIncomingPrompt).
	// Always nil in production.
	autoArmRace func()

	// queueDispatchRace is a test-only seam mirroring autoArmRace: when
	// non-nil, enqueueOrDispatch (handlePrompt's same-session-busy branch)
	// and maybeDispatchQueued invoke it right before their own claimForPrompt
	// call, letting a test force a real concurrent claim attempt (another
	// prompt_async, a goal auto-arm, or another maybeDispatchQueued call) to
	// land deterministically instead of relying on an unobserved
	// goroutine-scheduling coin flip (see TestPromptQueueRaceWithFreedSlot).
	// handlePrompt's own idle-with-queue branch invokes it too, right before
	// its dispatchQueueHead call, so a test can also force a concurrent
	// DELETE /session/{id}/queue into the narrow gap between that branch's
	// EnqueuePrompt and its dispatch attempt (see
	// TestQueueClearRaceDuringIdleDispatchIsNotAnError and
	// TestQueueClearRaceDuringDispatchIsNotAnError). Always nil in
	// production.
	queueDispatchRace func()

	// queueDeleteRace is a test-only seam: when non-nil, handleQueueDelete's
	// cold path (session not resident) invokes it right after its own
	// LoadSession call returns but before re-acquiring s.mu to register (or
	// yield to) a resident — letting a test force a real concurrent claim
	// (e.g. a prompt_async cold-loading and registering its OWN instance for
	// the same session) to land deterministically in that exact window,
	// instead of relying on an unobserved goroutine-scheduling coin flip. See
	// TestDeleteQueueColdSessionSurvivesResidencyRace. Always nil in
	// production.
	queueDeleteRace func()

	// worktreeBase is the directory 'worktree'-isolation sessions create
	// their per-session git worktrees under (see worktree.go): <SessionDir>/
	// worktrees when SessionDir is durable, otherwise a process-lifetime
	// temp directory created lazily on first use (a fresh, ephemeral
	// SessionDir has nothing for a future sweep to recover anyway). Set once
	// — either eagerly in New when SessionDir is set (so sweepWorktrees has
	// a fixed, predictable path to scan at serve start) or lazily by
	// worktreeBaseDir under mu.
	worktreeBase string
}

// goalTracker is the per-session goal summary surfaced in Session JSON.
// attempt is the 1-based worker-turn retry attempt from the most recent
// goal.stalled record; it is reset to 0 by goal.set/goal.eval/goal.achieved
// (a stall is non-terminal — see publishGoal — so it never touches active or
// achieved, only lastReason and attempt).
//
// retryable/retryableClass/waiting mirror the same-named goal.stalled
// fields (see engine/goal.go and GitHub issue #61): retryable and
// retryableClass are set whenever the most recent stall was classified
// provider-retryable weather; waiting is true while still inside the
// retryable budget ("waiting out provider weather") and false once that
// budget is exhausted (the loop is about to park a turn rather than clear
// the goal). All three reset the same way attempt does.
type goalTracker struct {
	condition      string
	active         bool
	achieved       bool
	turns          int
	lastReason     string
	attempt        int
	retryable      bool
	retryableClass string
	waiting        bool
	// pausedRestart is set by pauseArmedGoalsAtBoot when this process booted
	// and found the goal armed (active) with no loop ever attached in this
	// process — the restart half of the "paused" presentation (see
	// pauseView). Cleared the moment POST /session/{id}/goal re-arms it
	// (handleGoal). Never set any other way: it is NOT re-derived live from
	// running/active (see compositeState's doc comment for why that would
	// be wrong — an ordinary max-turns-exhausted goal in a live process is
	// not "paused", only one observed unattended at boot is).
	pausedRestart bool
	// pausedWorker is set by publishGoal/foldGoalRecordLocked when a
	// goal.parked record lands (NEP-4849, Task 2): a worker turn exhausted
	// either exhaustion tier and PursueGoal exit-parked instead of clearing
	// the goal — the worker-failure half of the "paused" presentation (see
	// pauseView). Unlike pausedRestart (a boot-time-only observation), this
	// is set LIVE, by the very loop that just parked; unlike waiting/
	// retryable's provider-backoff pause below, the loop has genuinely
	// exited (no goroutine is driving this goal at all) until the
	// activity-driven auto-arm (maybeAutoArmGoal) or an operator's re-POST
	// starts a fresh one. Reset to false everywhere pausedRestart is reset
	// (goal.set/goal.achieved/goal.cleared) plus goal.eval and goal.updated
	// — see publishGoal's per-case doc comments for why those two additional
	// resets are needed here specifically.
	pausedWorker bool
	// evalFailures is the most recent goal.eval_failed record's CONSECUTIVE
	// failure count (see engine/goal.go's "Round 6" doc section, NEP-4792) —
	// folded straight from the record, so it is idempotent for replay just
	// like every other field here. Reset to 0 by goal.set/goal.eval/
	// goal.achieved/goal.cleared/goal.updated, mirroring attempt's own reset
	// set except that goal.updated resets it too (see publishGoal's
	// evtGoalUpdated case): the streak is measured against a condition, and
	// an UpdateGoal is exactly the event that invalidates the evaluator
	// calls it counted.
	evalFailures int
}

// pauseView derives the goal's "paused" wire presentation (see
// GoalSummary.paused/pause_reason): pausedRestart (set once at boot, cleared
// on re-arm) takes priority; then pausedWorker (set live by an exit-parked
// worker turn, NEP-4849 — the loop has genuinely exited, mirroring the
// restart case's "nothing is driving this goal" shape even though the
// server never restarted); otherwise a still-active, still-waiting
// retryable stall (see engine/goal.go's retryable-backoff park machinery,
// whose loop IS still alive, merely waiting) reads as paused/
// provider-backoff — purely derived from the existing retryable/waiting
// fields, so it clears itself the instant the loop's next goal.eval or
// goal.stalled record resets waiting, exactly mirroring the self-re-arm
// behavior that machinery already has (see publishGoal).
func (g *goalTracker) pauseView() (paused bool, reason string) {
	switch {
	case g.pausedRestart:
		return true, pauseReasonRestart
	case g.pausedWorker:
		return true, pauseReasonWorkerFailure
	case g.active && g.retryable && g.waiting:
		return true, pauseReasonProviderBackoff
	default:
		return false, ""
	}
}

// resetGoalPauseLocked clears every pause-fold field pauseView reads from —
// pausedRestart, pausedWorker, and the retryable-backoff trio
// (retryable/retryableClass/waiting) — in one place, so a freshly (re)armed,
// genuinely running loop is never seen wearing a stale paused presentation
// from before it started. Callers MUST hold s.mu.
//
// Two call sites need this, and both mean "a loop is about to actually run
// against g, right now": handleGoal's re-arm branch (an operator's fresh
// POST /session/{id}/goal against a paused/idle goal) and maybeAutoArmGoal's
// successful-claim path (ordinary activity resuming a goal left armed with
// no loop attached — restart or worker-park). Before this helper existed,
// each site reset a different subset of these fields by hand; maybeAutoArmGoal
// resetting only pausedWorker left pausedRestart permanently stuck true when
// a restart-paused goal was resumed by a plain prompt rather than a fresh
// POST /goal — see TestAutoArmAfterRestartResetsPausePresentation.
func resetGoalPauseLocked(g *goalTracker) {
	if g == nil {
		return
	}
	g.pausedRestart = false
	g.pausedWorker = false
	g.retryable = false
	g.retryableClass = ""
	g.waiting = false
}

// turnOutcome is the per-session last-turn summary surfaced on Session JSON
// (last_turn) and /session/status entries. outcome is "completed" or
// "error"; error is the sanitized failure detail (empty on completion).
type turnOutcome struct {
	outcome string
	error   string
}

// sessionState tracks an in-memory session and any in-flight prompt. lastUsed
// is the time the session was created, loaded, or last finished a prompt; it
// orders MaxResident eviction (longest-idle first).
type sessionState struct {
	sess     *engine.Session
	running  bool
	cancel   context.CancelFunc
	lastUsed time.Time
	// shareWorkdir opts this session out of the workdir-busy exclusivity rule
	// in claimForPrompt (see workdir.go): set from POST /session's
	// share_workdir, in memory only (a reloaded/cold session defaults back to
	// false, like running/cancel).
	shareWorkdir bool
	// isolation is the POST /session workdir_isolation value ("" behaves as
	// isolationShared), in memory only like shareWorkdir: a reloaded/cold
	// session defaults back to shared, same limitation the doc comment above
	// already accepts. isolationWorktree also bypasses the workdir-busy
	// claim (see workdirHolderLocked) — though since each worktree session
	// gets its own unique path, it would never collide anyway.
	isolation string
	// worktree is non-nil exactly when isolation == isolationWorktree and
	// worktree creation succeeded; it is the handle handleEnd uses to tear
	// the worktree down (or keep it and journal why).
	worktree *worktreeInfo
	// goalLoop is true exactly when the CURRENT occupant of running/cancel is
	// a goal loop (a runGoal call), false when it is a plain prompt (or
	// nothing, its zero value). Set at every site that spawns runGoal while
	// holding the claim (handleGoal's fresh-start and re-arm paths,
	// handleGoalBusy's retry-win path, maybeAutoArmGoal) -- always AFTER
	// claimForPrompt itself has returned, never before -- and reset to false
	// both at claimForPrompt's own claim site (so the flag is self-contained
	// and never depends on a previous occupant's tail having run) and
	// everywhere running/cancel themselves are reset (runPrompt's and
	// runGoal's tails, handleCompact's tail, and handleGoal's two rollback
	// branches) -- so it always names the true occupant, never stale from a
	// previous claim.
	//
	// handleGoalDelete reads it to decide whether to cancel: a plain prompt
	// occupying the run slot while a goal sits merely "armed" (see
	// handleGoalBusy's 202 "armed" response and maybeAutoArmGoal) must be
	// left running -- DELETE only clears the goal in that case, exactly like
	// the boot-time-paused/no-loop-attached case already handled below. See
	// TestDeleteGoalDuringArmedPromptLeavesPromptRunning.
	goalLoop bool
}

// New builds a Server and reconciles its journal against the on-disk session
// logs (appending any message records lost to a crash between the session-log
// and journal appends). Reconciliation reads the session directory only — it
// touches no network and spawns nothing, so it is safe on the startup path.
func New(opts Options) (*Server, error) {
	if opts.RunToken == "" {
		return nil, errors.New("server: RunToken is required")
	}
	if opts.NewSession == nil || opts.LoadSession == nil {
		return nil, errors.New("server: NewSession and LoadSession are required")
	}
	if opts.Version == "" {
		opts.Version = "0.1.0"
	}
	if opts.MaxResident <= 0 {
		opts.MaxResident = 32
	}
	s := &Server{
		opts:           opts,
		subs:           make(map[*subscriber]struct{}),
		seen:           make(map[string]map[string]bool),
		sessions:       make(map[string]*sessionState),
		lastRequest:    make(map[string]*requestSnapshot),
		lastReqHash:    make(map[string]string),
		lastPersistErr: make(map[string]string),
		goalState:      make(map[string]*goalTracker),
		lastTurn:       make(map[string]*turnOutcome),
		waiters:        make(map[*waiter]struct{}),
		closing:        make(chan struct{}),
	}
	if err := s.reconcile(); err != nil {
		return nil, err
	}
	// Deliverable 2(a): mark every goal restored active-but-unattended by
	// reconcile's journal replay as paused/restart, and journal that
	// observation, before the server is reachable by any client.
	s.pauseArmedGoalsAtBoot()
	if opts.SessionDir != "" {
		// Fixed, predictable path (rather than worktreeBaseDir's lazy
		// temp-dir fallback) so a crashed predecessor's worktrees — created
		// under this exact path — are always found here on restart. An
		// ephemeral (SessionDir == "") server has nothing durable to sweep:
		// its own worktreeBase is a fresh temp directory every run.
		s.worktreeBase = filepath.Join(opts.SessionDir, "worktrees")
		sweepWorktrees(s.worktreeBase, opts.SessionDir, func(sessionID, path string) {
			s.emitDurable(Event{Type: evtWorktreeKept, SessionID: sessionID, WorktreePath: path})
		})
	}
	s.routes()
	return s, nil
}

// worktreeBaseDir returns the directory 'worktree'-isolation sessions create
// their per-session git worktrees under, creating it (and its meta
// subdirectory) on first use. New already sets it eagerly when SessionDir is
// configured; this lazily falls back to a process-lifetime temp directory
// otherwise, computed once and reused for every worktree session this
// process creates.
func (s *Server) worktreeBaseDir() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.worktreeBase == "" {
		tmp, err := os.MkdirTemp("", "harness-worktrees-")
		if err != nil {
			return "", err
		}
		s.worktreeBase = tmp
	}
	if err := os.MkdirAll(filepath.Join(s.worktreeBase, "meta"), 0o755); err != nil {
		return "", err
	}
	return s.worktreeBase, nil
}

func (s *Server) routes() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /session", s.auth(s.handleList))
	mux.HandleFunc("POST /session", s.auth(s.handleCreate))
	// A precise pattern outranks the {id} wildcard, so /session/status is not
	// mistaken for a session named "status".
	mux.HandleFunc("GET /session/status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /session/{id}", s.auth(s.handleGet))
	mux.HandleFunc("DELETE /session/{id}", s.auth(s.handleEnd))
	mux.HandleFunc("GET /session/{id}/wait", s.auth(s.handleWait))
	mux.HandleFunc("GET /session/{id}/message", s.auth(s.handleMessages))
	mux.HandleFunc("GET /session/{id}/request", s.auth(s.handleRequest))
	mux.HandleFunc("POST /session/{id}/prompt_async", s.auth(s.handlePrompt))
	mux.HandleFunc("POST /session/{id}/enqueue", s.auth(s.handleEnqueue))
	mux.HandleFunc("GET /session/{id}/queue", s.auth(s.handleQueueGet))
	mux.HandleFunc("DELETE /session/{id}/queue", s.auth(s.handleQueueDelete))
	mux.HandleFunc("POST /session/{id}/compact", s.auth(s.handleCompact))
	mux.HandleFunc("POST /session/{id}/goal", s.auth(s.handleGoal))
	mux.HandleFunc("DELETE /session/{id}/goal", s.auth(s.handleGoalDelete))
	mux.HandleFunc("POST /session/{id}/abort", s.auth(s.handleAbort))
	mux.HandleFunc("GET /event", s.auth(s.handleEvent))
	mux.HandleFunc("GET /process", s.auth(s.handleProcessList))
	mux.HandleFunc("POST /process/{name}/start", s.auth(s.handleProcessStart))
	mux.HandleFunc("POST /process/{name}/stop", s.auth(s.handleProcessStop))
	mux.HandleFunc("POST /process/{name}/restart", s.auth(s.handleProcessRestart))
	s.mux = mux
}

// ServeHTTP implements http.Handler. When CORS is enabled it layers the CORS
// contract over the mux: the allow-origin/Vary headers on every response and a
// short-circuited 204 for OPTIONS preflights (which carry no credentials, so
// they bypass auth entirely).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.opts.CORSOrigin != "" {
		h := w.Header()
		// Echo the configured origin (literal, including "*"). Setting it here,
		// before the mux runs, means it rides along on every downstream response
		// — success, 401, 404, or SSE — so a browser can always read the result.
		h.Set("Access-Control-Allow-Origin", s.opts.CORSOrigin)
		h.Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// Drain waits for in-flight prompts to finish, then returns. Under s.mu, and
// before it starts waiting, it sets the draining flag and closes the closing
// signal (once). Setting draining before wg.Wait is what makes the prompt
// admission gate correct: a new prompt's wg.Add(1) happens in the same s.mu
// critical section that checks draining (see claimForPrompt), so by mutex
// ordering every Add that ever runs either preceded draining=true — and is
// therefore counted by the Wait below — or observes draining and is rejected
// with 503. A WaitGroup Add can never race after this Wait begins.
//
// Closing the signal ends every connected SSE stream promptly, which lets a
// concurrently-running http.Server.Shutdown see idle connections and return
// instead of blocking on a live /event tail for the whole grace budget;
// disconnected orchestrators recover the trailing records via replay-from-seq.
//
// Drain then waits up to ctx's deadline for prompts to complete on their own;
// if ctx expires while prompts are still running, it cancels their contexts
// (which journals a durable session.aborted for each) and waits for them to
// unwind. It must be called before Close so the journal file stays open while
// the trailing records — the final assistant message and the
// session.aborted/idle transitions — are written; otherwise those records are
// lost on shutdown.
func (s *Server) Drain(ctx context.Context) {
	s.mu.Lock()
	s.draining = true
	s.closeOnce.Do(func() { close(s.closing) })
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	// Grace period expired with prompts still in flight: cancel them so their
	// runPrompt goroutines observe context.Canceled, journal session.aborted,
	// and exit. Wait for that to finish so the records land before Close.
	s.mu.Lock()
	for _, st := range s.sessions {
		if st.cancel != nil {
			st.cancel()
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Shutdown gracefully stops a harness serve instance. It runs the HTTP server's
// Shutdown and the Server's Drain CONCURRENTLY under one deadline, waits for
// both, and returns Shutdown's error.
//
// The two must overlap, not run in sequence:
//
//   - httpSrv.Shutdown closes the listener as its first synchronous action, so
//     no new request is accepted the instant shutdown begins. It then waits for
//     open connections to go idle.
//   - Drain closes the closing signal at entry, which ends connected SSE tails
//     promptly; that is what lets the concurrent Shutdown see idle connections
//     and return quickly instead of blocking on a live /event tail for the whole
//     grace budget. In parallel, Drain gives the detached prompt goroutines
//     (their 202 already returned; Shutdown does not track them) the full grace
//     budget to finish before it cancels them and journals their trailing
//     records.
//
// Running them sequentially either way loses: Shutdown-then-Drain would block
// Shutdown on the SSE tail, and Drain-then-Shutdown would keep the listener open
// for the whole drain window, admitting new prompts mid-drain (a data-loss bug
// the draining gate exists to prevent).
func Shutdown(ctx context.Context, httpSrv *http.Server, srv *Server) error {
	drained := make(chan struct{})
	go func() {
		srv.Drain(ctx)
		close(drained)
	}()
	err := httpSrv.Shutdown(ctx)
	<-drained
	return err
}

// Close releases the journal file, if any.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jf != nil {
		return s.jf.Close()
	}
	return nil
}

// auth wraps a handler with constant-time bearer-token authentication.
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	tok := h[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.opts.RunToken)) == 1
}

// reportError forwards err to Options.OnError, nil-guarded. Safe to call
// with s.mu held: the callback must not call back into the Server (see the
// OnError doc comment).
func (s *Server) reportError(err error) {
	if s.opts.OnError == nil {
		return
	}
	s.opts.OnError(context.Background(), err)
}

// writeJSON marshals v to a buffer BEFORE writing anything to w: the status
// line and the body must not be allowed to disagree. The previous version
// wrote the header first and streamed the encoder straight to w, so a
// mid-encode marshal failure (e.g. a poisoned json.RawMessage deep in a
// session's history) left a 200 response truncated after however many bytes
// the encoder had already flushed — indistinguishable, to a client, from a
// network glitch, and impossible to retry into success. Marshaling first
// means a failure here is caught before any bytes go out, so it can be
// reported honestly as a 500 with a real error body instead.
func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		// The error body is a fixed, always-marshalable shape (a
		// map[string]string), so this cannot recurse into the same
		// failure — the resilience path must not itself fail.
		eb, _ := json.Marshal(map[string]string{"error": "internal: " + err.Error()})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(eb)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
