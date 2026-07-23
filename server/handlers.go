package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
)

// sessionJSON is the openapi Session shape.
type sessionJSON struct {
	ID        string           `json:"id"`
	CreatedAt time.Time        `json:"created_at"`
	Model     message.ModelRef `json:"model"`
	Status    string           `json:"status"`
	// State is the unambiguous composite: idle, busy, or goal-running. Kept
	// alongside Status (never replacing it) for backward compat. Precedence:
	// goal-running wins whenever a goal is active, REGARDLESS of the momentary
	// worker status — including the goal loop's between-turn gap (worker
	// finished a turn, evaluator hasn't answered yet) where Status alone would
	// still read "busy" (the server's busy/idle transition brackets the WHOLE
	// PursueGoal call, not each turn) but a naive orchestrator inferring
	// progress from "status=idle plus goal.active" elsewhere is exactly the
	// ambiguity this field exists to remove. See compositeState.
	State    string    `json:"state"`
	Messages int       `json:"messages"`
	Seq      int64     `json:"seq,omitempty"`
	Goal     *goalJSON `json:"goal,omitempty"`
	WorkDir  string    `json:"workdir"`
	// LastTurn is the most recent prompt or goal-worker turn's outcome for
	// this process — "completed" or "error", plus the sanitized error detail
	// on failure — so a poller can distinguish "idle because done" from
	// "idle because the turn died" without inferring it from message part
	// shapes. Present only once a turn has finished in this process (like
	// Goal, absent on a freshly reloaded, never-prompted-here session).
	LastTurn *lastTurnJSON `json:"last_turn,omitempty"`
	// Usage is cumulative token usage plus message count and (when
	// available) the most recent turn's input tokens (issue #62 layer 2) —
	// so an orchestrator can rotate a session BEFORE it hits the provider's
	// context-window cliff (see LastTurn.outcome "context_exhausted" for
	// the case where it didn't rotate in time). Always present, since the
	// engine tracks usage for every session, resident or reloaded from its
	// log — a fresh, never-prompted session simply reports all zeros.
	Usage usageJSON `json:"usage"`
	// LastActivityAt is the timestamp of the most recent message appended
	// to the session (user, assistant, or tool) — or CreatedAt if none has
	// been appended yet. See engine.Session.LastActivityAt's doc comment
	// for why this exists: operators previously had to double-sample Seq to
	// distinguish a session quietly working from one wedged mid-turn; this
	// answers that directly, as a single absolute timestamp, resident or
	// not (a non-resident session gets it from LoadSession replay, same as
	// a resident one gets it from memory — no separate reconcile step
	// needed, unlike Goal/LastTurn above, which really are process-local).
	LastActivityAt time.Time `json:"last_activity_at"`
	// ParentSession is the session's lineage pointer (see
	// engine.Config.ParentSession's doc comment): an opaque provenance
	// pointer to the session this one continues from, set at creation via
	// POST /session's parent_session field, durable across resume/restart.
	// Absent (omitempty) when the session has no recorded parent — the
	// common case.
	ParentSession string `json:"parent_session,omitempty"`
	// CompactionCount/LastCompactedAt surface whether and when this session
	// has been compacted (docs/design/context-compaction.md), auto-
	// triggered or via POST /session/{id}/compact — so a UI can show that
	// compaction happened. CompactionCount is 0 (omitted) until the first
	// compaction; LastCompactedAt is the zero Time (omitted) likewise. Both
	// survive a restart (engine.Session.CompactionCount/LastCompactedAt
	// replay the compact journal record — see engine/store.go).
	CompactionCount int       `json:"compaction_count,omitempty"`
	LastCompactedAt time.Time `json:"last_compacted_at,omitzero"`
	// Queued is the session's current durable prompt-queue depth (see
	// docs/plans/2026-07-19-prompt-queue.md): prompts submitted via
	// prompt_async while the session was busy, waiting for the next natural
	// drain trigger (idle dispatch or a goal loop's turn-boundary
	// injection). Always present (0 when nothing is waiting, never
	// omitted) — unlike Goal/LastTurn, this needs no "never happened here"
	// distinction, so there is no reason to hide the zero value. Read
	// directly from engine.Session.QueuedPrompts(), so it is correct for a
	// resident session and a freshly reloaded one alike (see buildSession).
	Queued int `json:"queued"`
}

// usageJSON is the Session/StatusEntry usage sub-object (issue #62 layer 2):
// cumulative token usage the engine already tracks (engine.Session.Usage),
// plus message count and, when the engine can derive it cheaply, the most
// recent turn's input tokens (engine.Session.LastUsage /
// engine.SessionInfo.LastInputTokens).
type usageJSON struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	Messages         int `json:"messages"`
	// LastInputTokens is the input-token count of the most recent completed
	// turn, omitted (zero) until at least one turn has completed.
	LastInputTokens int `json:"last_input_tokens,omitempty"`
}

// usageJSONForSession builds usageJSON from a fully loaded/resident
// engine.Session — used by buildSession (GET /session/{id}, GET /session)
// and by handleStatus's resident branch.
func usageJSONForSession(sess *engine.Session) usageJSON {
	u := sess.Usage()
	out := usageJSON{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		Messages:         len(sess.History()),
	}
	if last, ok := sess.LastUsage(); ok {
		out.LastInputTokens = last.InputTokens
	}
	return out
}

// usageJSONForInfo builds usageJSON from a cheap engine.SessionInfo (no full
// session load) — used by handleStatus's non-resident branch, where paying
// for a full LoadSession per listed session would defeat the point of a
// lightweight status endpoint.
func usageJSONForInfo(info engine.SessionInfo) usageJSON {
	return usageJSON{
		InputTokens:      info.Usage.InputTokens,
		OutputTokens:     info.Usage.OutputTokens,
		CacheReadTokens:  info.Usage.CacheReadTokens,
		CacheWriteTokens: info.Usage.CacheWriteTokens,
		Messages:         info.Messages,
		LastInputTokens:  info.LastInputTokens,
	}
}

// lastTurnJSON is the openapi LastTurn shape.
type lastTurnJSON struct {
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// compositeState resolves the unambiguous Session.state field: goal-running
// whenever a goal is active, regardless of the momentary running/busy flag
// (see sessionJSON.State's doc comment for why momentary busy/idle is not
// enough); otherwise busy or idle mirroring the plain status.
//
// forceIdle (see goalTracker.pauseView and forcesIdlePause below) OVERRIDES
// all of that to idle for the two pause reasons whose loop has genuinely
// stopped driving the goal — "restart" (a goal restored from the journal at
// boot with no loop ever attached) and "worker_failure" (Task 2:
// a worker turn exit-parked the goal, and PursueGoal has actually returned —
// no goroutine is running it until the next auto-arm or re-POST). Neither is
// "goal-running" in any sense an operator or composer can act on — it will
// never progress on its own, and "busy"/"goal-running" forever is exactly
// the operator trap this field exists to close (see docs/design/
// fleet-model.md's ADOPT lifecycle). A provider-backoff pause deliberately
// does NOT take this path — its loop is genuinely alive and running, just
// waiting out provider weather, so it keeps reading goal-running (see
// TestGoalStalledProviderBackoffSurfacesPaused).
func compositeState(running, goalActive, forceIdle bool) string {
	switch {
	case forceIdle:
		return "idle"
	case goalActive:
		return "goal-running"
	case running:
		return "busy"
	default:
		return "idle"
	}
}

// forcesIdlePause reports whether goal represents a pause reason whose loop
// has genuinely stopped driving the goal — "restart" (see
// pauseArmedGoalsAtBoot) or "worker_failure" (see goalTracker.pausedWorker)
// — the two pause reasons that force compositeState to idle.
// "provider-backoff" deliberately returns false here: that loop is still
// alive, merely waiting. nil-safe.
func forcesIdlePause(goal *goalJSON) bool {
	return goal != nil && goal.Paused && (goal.PauseReason == pauseReasonRestart || goal.PauseReason == pauseReasonWorkerFailure)
}

// goalJSON is the Session.goal sub-object: present only when a goal has been
// set for the session in this process.
//
// Retryable/RetryableClass/Waiting mirror the most recent goal.stalled
// record's classification (see engine/goal.go and GitHub issue #61):
// Retryable is true when that stall was classified provider-retryable
// weather, RetryableClass names it, and Waiting is true while still inside
// the retryable budget ("waiting out provider weather") and false once
// that budget is exhausted (the loop is about to park a turn, not die).
// All three are reset by goal.set/goal.eval/goal.achieved, same as Attempt.
type goalJSON struct {
	Condition      string `json:"condition"`
	Active         bool   `json:"active"`
	Achieved       bool   `json:"achieved,omitempty"`
	Turns          int    `json:"turns"`
	LastReason     string `json:"last_reason,omitempty"`
	Attempt        int    `json:"attempt,omitempty"`
	Retryable      bool   `json:"retryable,omitempty"`
	RetryableClass string `json:"retryable_class,omitempty"`
	Waiting        bool   `json:"waiting,omitempty"`
	// Paused/PauseReason present the "goal armed but nothing is driving it"
	// state (see goalTracker.pauseView): true with pause_reason "restart"
	// when this process booted and found the goal active with no loop ever
	// attached (see pauseArmedGoalsAtBoot); true with "worker_failure"
	// (Task 2) when a worker turn exit-parked the goal (engine/
	// goal.go's goal.parked) — the loop has genuinely exited, resumed only
	// by the next ordinary activity (maybeAutoArmGoal) or an operator
	// re-POST; true with "provider-backoff" while the retryable-backoff
	// park machinery (engine/goal.go) waits out provider weather — that
	// loop is still alive, merely waiting. All three clear on re-arm (POST
	// /session/{id}/goal) or, for provider-backoff, the moment the loop's
	// own retry succeeds.
	Paused      bool   `json:"paused,omitempty"`
	PauseReason string `json:"pause_reason,omitempty"`
	// EvalFailures is the most recent goal.eval_failed record's consecutive
	// failure count (see engine/goal.go's "Round 6" doc section
	// and goalTracker.evalFailures): rises with each failed evaluator
	// boundary below goalEvalFailureLimit and resets to 0 on goal.set,
	// goal.eval, goal.achieved, goal.cleared, or goal.updated. Omitted
	// (zero) whenever no boundary has failed since the last reset.
	EvalFailures int `json:"eval_failures,omitempty"`
}

// goalJSONFrom builds the goalJSON wire shape from a per-session goal
// tracker, deriving the paused presentation via pauseView — the single
// construction path shared by buildSession and waitSnapshot so the two can
// never drift on this. Returns nil for a nil tracker (no goal ever set).
func goalJSONFrom(g *goalTracker) *goalJSON {
	if g == nil {
		return nil
	}
	paused, reason := g.pauseView()
	return &goalJSON{
		Condition:      g.condition,
		Active:         g.active,
		Achieved:       g.achieved,
		Turns:          g.turns,
		LastReason:     g.lastReason,
		Attempt:        g.attempt,
		Retryable:      g.retryable,
		RetryableClass: g.retryableClass,
		Waiting:        g.waiting,
		Paused:         paused,
		PauseReason:    reason,
		EvalFailures:   g.evalFailures,
	}
}

// sessionIDOrNotFound extracts {id} from the request path and validates it
// with engine.ValidSessionID (legacy "ses_" + 16 hex, or a well-formed "ses"
// TypeID), writing 404 and returning ok=false otherwise. Every handler
// keyed by {id} must call this before touching the session directory or
// s.sessions: net/http's ServeMux splits routing segments on the RAW,
// still-percent-encoded path, so a single segment spelled "..%2fleaked"
// matches "/session/{id}" and PathValue decodes it to "../leaked" — parsing
// at this boundary, rather than trusting whatever came back from
// os.ReadFile/filepath.Join, is what keeps that from escaping SessionDir.
func (s *Server) sessionIDOrNotFound(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !engine.ValidSessionID(id) {
		writeErr(w, http.StatusNotFound, "no such session")
		return "", false
	}
	return id, true
}

// healthJSON is the openapi Health shape. VCSRevision and VCSTime are always
// present (never omitted, even empty) so a client never has to special-case
// "field absent" vs "field empty" — see buildInfo.
type healthJSON struct {
	Version     string `json:"version"`
	VCSRevision string `json:"vcs_revision"`
	VCSTime     string `json:"vcs_time"`
}

// buildInfo reads the running binary's VCS revision and commit time from
// runtime/debug.ReadBuildInfo, so GET /health can identify exactly which
// commit is live — a stale box binary otherwise looks identical to a fresh
// one behind a fixed config Version string (an engineer once burned 30
// minutes suspecting exactly that). Both return "" when build info is
// unavailable or carries no VCS settings, which is the ordinary case for a
// `go test` binary — ReadBuildInfo still succeeds there, it simply has no
// "vcs.revision"/"vcs.time" entries in Settings.
func buildInfo() (revision, buildTime string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ""
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.time":
			buildTime = setting.Value
		}
	}
	return revision, buildTime
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	rev, t := buildInfo()
	writeJSON(w, http.StatusOK, healthJSON{Version: s.opts.Version, VCSRevision: rev, VCSTime: t})
}

// handleGoroutines writes the full, all-goroutine stack dump (the exact
// text Go's default SIGQUIT handler prints) as a diagnostic HTTP surface —
// for a box wedged badly enough that even exec is awkward (or unavailable,
// e.g. a managed sandbox with no shell access), this gets the same picture
// SIGQUIT would give over authed HTTP instead. It is registered behind
// s.auth like every other route (see routes()), deliberately NOT under
// net/http/pprof's default mux registration (which would also register
// /debug/pprof/* unauthenticated on http.DefaultServeMux as an import side
// effect) — this calls runtime/pprof directly against the existing mux
// instead, so the only new surface is this one explicit, authed route.
// debug=2 (not the default 1) is what makes the output match SIGQUIT's own
// format: full stack traces in the panic-style layout, not pprof's
// symbolized-count summary.
func (s *Server) handleGoroutines(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Lookup("goroutine") is always non-nil (a predefined profile registered
	// by the runtime itself), so no nil check is needed here.
	pprof.Lookup("goroutine").WriteTo(w, 2) //nolint:errcheck // best-effort diagnostic write; nothing to do with a failure once headers are sent
}

// maxParentSessionLen bounds POST /session's optional parent_session field:
// an opaque provenance pointer, not a session ID this server necessarily
// knows about (lineage may cross boxes — see engine.Config.ParentSession),
// so the only sane validation is a length cap against an accidental
// pasted-blob value, not a format check.
const maxParentSessionLen = 128

// validateParentSession validates POST /session's optional parent_session
// field: nil (the key was omitted) is valid and returns "", no error. A
// present value must be non-empty and at most maxParentSessionLen bytes;
// either violation is a 400. It is deliberately NOT required to name a
// session that exists on this server, or anywhere — see
// engine.Config.ParentSession's doc comment.
func validateParentSession(v *string) (string, error) {
	if v == nil {
		return "", nil
	}
	if *v == "" {
		return "", errors.New("parent_session: must be non-empty when present")
	}
	if len(*v) > maxParentSessionLen {
		return "", fmt.Errorf("parent_session: exceeds maximum length of %d bytes", maxParentSessionLen)
	}
	return *v, nil
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	handlerStart := time.Now()
	var body struct {
		Model        message.ModelRef `json:"model"`
		WorkDir      string           `json:"workdir"`
		ShareWorkdir bool             `json:"share_workdir"`
		// WorkdirIsolation is "shared" (default, omitted/empty) or
		// "worktree" — see createWorktreeForSession and workdirHolderLocked.
		WorkdirIsolation string `json:"workdir_isolation"`
		// ParentSession is an opaque provenance pointer to the session this
		// one continues from (see engine.Config.ParentSession's doc
		// comment). Optional; validated by validateParentSession below.
		ParentSession *string `json:"parent_session"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	parentSession, err := validateParentSession(body.ParentSession)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	isolation := body.WorkdirIsolation
	if isolation == "" {
		isolation = isolationShared
	}
	if isolation != isolationShared && isolation != isolationWorktree {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("workdir_isolation: unknown value %q (want \"shared\" or \"worktree\")", body.WorkdirIsolation))
		return
	}
	workDir, err := resolveWorkDir(s.opts.WorkspaceRoots, body.WorkDir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// sessionWorkDir is what the session's tools actually run in: workDir
	// itself for 'shared', or a dedicated git worktree checked out from it
	// for 'worktree'. wt is nil for 'shared'.
	sessionWorkDir := workDir
	var wt *worktreeInfo
	if isolation == isolationWorktree {
		wt, err = s.createWorktreeForSession(workDir)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		sessionWorkDir = wt.path
	}

	phaseStart := time.Now()
	sess, err := s.opts.NewSession(body.Model, sessionWorkDir, parentSession)
	if err != nil {
		if wt != nil {
			s.discardWorktree(wt)
		}
		writeErr(w, http.StatusInternalServerError, "cannot create session")
		return
	}
	s.reportCreatePhase(sess.ID, "new_session", time.Since(phaseStart))
	// Report "total" on every return past this point — success or error —
	// not just the success tail below. Without this, a failure after
	// new_session (recordWorktreeOwner, Persist) never reports "total", and
	// the cmd-layer accumulator that keys phases by session ID (see
	// cmd/harness/main.go's createPhaseLogger) leaks that entry forever — a
	// saturated storage volume is precisely what makes Persist fail or stall
	// on every create. Reporting "total" on the error path is also strictly
	// better diagnostics: it is the one phase report that survives a failed
	// create, showing which prior phase ran (and how slowly) before the
	// failure.
	defer func() {
		s.reportCreatePhase(sess.ID, "total", time.Since(handlerStart))
	}()
	if wt != nil {
		// Record the owning session in the meta BEFORE the session log
		// becomes durable, and fail creation if it cannot be recorded. The
		// order is load-bearing: the startup sweep keys entirely on
		// sessionResumable(meta.SessionID), so the moment Persist succeeds
		// the meta must already name the owner — a crash between "log on
		// disk" and "owner recorded" would leave a resumable session whose
		// meta has no SessionID, and the sweep would reap its live worktree
		// out from under it. With this order a crash instead leaves a meta
		// whose session log does not exist yet: never resumable, safely
		// adjudicated clean/dirty like any abandoned worktree.
		if err := s.recordWorktreeOwner(wt, sess.ID); err != nil {
			s.discardWorktree(wt)
			writeErr(w, http.StatusInternalServerError, "cannot create session")
			return
		}
	}
	// Persist the log now so the session has durable state even if it is
	// evicted before its first prompt; otherwise eviction below would drop a
	// never-prompted session with no on-disk backing to reload from.
	s.reportCreatePhaseStart(sess.ID, "persist")
	phaseStart = time.Now()
	if err := sess.Persist(); err != nil {
		if wt != nil {
			s.discardWorktree(wt)
		}
		writeErr(w, http.StatusInternalServerError, "cannot create session")
		return
	}
	s.reportCreatePhase(sess.ID, "persist", time.Since(phaseStart))

	s.reportCreatePhaseStart(sess.ID, "register")
	phaseStart = time.Now()
	s.mu.Lock()
	s.sessions[sess.ID] = &sessionState{sess: sess, lastUsed: time.Now(), shareWorkdir: body.ShareWorkdir, isolation: isolation, worktree: wt}
	s.evictResidentLocked()
	s.mu.Unlock()
	s.reportCreatePhase(sess.ID, "register", time.Since(phaseStart))

	s.reportCreatePhaseStart(sess.ID, "emit_created")
	phaseStart = time.Now()
	s.emitDurable(Event{Type: evtSessionCreated, SessionID: sess.ID, Model: sess.Model()})
	s.reportCreatePhase(sess.ID, "emit_created", time.Since(phaseStart))

	writeJSON(w, http.StatusCreated, s.buildSession(sess, "idle"))
}

// reportCreatePhase forwards elapsed to Options.OnCreatePhase, nil-guarded.
// See its doc comment for the reported phases and success-only contract.
func (s *Server) reportCreatePhase(sessionID, phase string, elapsed time.Duration) {
	if s.opts.OnCreatePhase != nil {
		s.opts.OnCreatePhase(sessionID, phase, elapsed)
	}
}

// reportCreatePhaseStart forwards to Options.OnCreatePhaseStart, nil-guarded.
// See its doc comment for which phases are covered (persist/register/
// emit_created — not new_session, not total).
func (s *Server) reportCreatePhaseStart(sessionID, phase string) {
	if s.opts.OnCreatePhaseStart != nil {
		s.opts.OnCreatePhaseStart(sessionID, phase)
	}
}

// createWorktreeForSession validates that workDir is inside a git repository
// and creates a dedicated, detached-HEAD worktree for a new 'worktree'
// isolation session (see addWorktree). Its error message is written directly
// into the 400 response body, so it names the actual problem (not inside a
// git repository vs. the underlying git failure) rather than a generic
// "cannot create session".
func (s *Server) createWorktreeForSession(workDir string) (*worktreeInfo, error) {
	repoRoot, ok, err := gitRepoRoot(workDir)
	if err != nil {
		return nil, fmt.Errorf("workdir_isolation=worktree: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("workdir_isolation=worktree requires workdir %q to be inside a git repository", workDir)
	}
	base, err := s.worktreeBaseDir()
	if err != nil {
		return nil, fmt.Errorf("workdir_isolation=worktree: %w", err)
	}
	id := newWorktreeID()
	path := filepath.Join(base, id)
	// Write a provisional meta BEFORE addWorktree runs: path and repoRoot
	// are already known, and BaseCommit/SessionID are patched in once they
	// exist (the writeWorktreeMeta call below, and recordWorktreeOwner
	// after the session is minted). This closes the crash window that used
	// to exist between addWorktree succeeding and the meta actually landing
	// on disk — a real git worktree with zero bookkeeping, invisible to
	// sweepWorktrees (which only ever reads meta/*.json) and leaked
	// forever. A meta whose worktree directory doesn't exist yet is
	// something sweepWorktrees already knows how to prune.
	metaPath, err := writeWorktreeMeta(base, id, worktreeMeta{RepoRoot: repoRoot, Path: path})
	if err != nil {
		return nil, fmt.Errorf("workdir_isolation=worktree: %w", err)
	}
	baseCommit, err := addWorktree(repoRoot, path)
	if err != nil {
		os.Remove(metaPath) // best effort: no worktree was ever created, nothing for the sweep to adjudicate
		return nil, fmt.Errorf("workdir_isolation=worktree: %w", err)
	}
	// metaPath is deterministic in base+id alone, so it's unchanged by this
	// second write; only its content (BaseCommit) gets patched in.
	if _, err := writeWorktreeMeta(base, id, worktreeMeta{RepoRoot: repoRoot, Path: path, BaseCommit: baseCommit}); err != nil {
		_ = removeWorktree(repoRoot, path)
		os.Remove(metaPath)
		return nil, fmt.Errorf("workdir_isolation=worktree: %w", err)
	}
	return &worktreeInfo{id: id, base: base, path: path, repoRoot: repoRoot, baseCommit: baseCommit, metaPath: metaPath}, nil
}

// recordWorktreeOwner patches wt's meta file with the now-known session ID,
// once NewSession has actually minted one.
func (s *Server) recordWorktreeOwner(wt *worktreeInfo, sessionID string) error {
	_, err := writeWorktreeMeta(wt.base, wt.id, worktreeMeta{
		SessionID:  sessionID,
		RepoRoot:   wt.repoRoot,
		Path:       wt.path,
		BaseCommit: wt.baseCommit,
	})
	return err
}

// discardWorktree removes a just-created worktree when session construction
// fails after it: nothing has been journaled or made resident yet, so there
// is no "kept" record to worry about — a bare best-effort removal (falling
// back to leaving it, never forcing) is enough.
func (s *Server) discardWorktree(wt *worktreeInfo) {
	if err := removeWorktree(wt.repoRoot, wt.path); err == nil {
		os.Remove(wt.metaPath)
	}
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	type snap struct {
		sess    *engine.Session
		running bool
	}
	s.mu.Lock()
	mem := make([]snap, 0, len(s.sessions))
	for _, st := range s.sessions {
		mem = append(mem, snap{st.sess, st.running})
	}
	s.mu.Unlock()

	out := []sessionJSON{}
	seen := make(map[string]bool)
	for _, m := range mem {
		out = append(out, s.buildSession(m.sess, statusStr(m.running)))
		seen[m.sess.ID] = true
	}
	infos, err := engine.ListSessions(s.opts.SessionDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot list sessions")
		return
	}
	for _, info := range infos {
		if seen[info.ID] {
			continue
		}
		sess, err := s.opts.LoadSession(info.ID)
		if err != nil {
			continue
		}
		out = append(out, s.buildSession(sess, "idle"))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	sess, status, ok := s.lookup(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	writeJSON(w, http.StatusOK, s.buildSession(sess, status))
}

// messagePlaceholder substitutes for a resident message that fails to
// marshal (see handleMessages): it carries just enough to identify which
// message broke and why, without ever risking a second marshal failure
// itself (every field here is a plain string).
type messagePlaceholder struct {
	ID           string `json:"id"`
	Role         string `json:"role"`
	MarshalError string `json:"marshal_error"`
}

// handleMessages returns the session's full canonical message history.
//
// It marshals per-message rather than the whole slice at once: a single
// resident message that fails to marshal — e.g. a Reasoning part carrying a
// non-zero-length but invalid ProviderData entry, which
// message.Message.Normalize does not catch (see its doc comment) because it
// only scrubs zero-length entries — used to take the entire endpoint down
// with a 500 ("json: error calling MarshalJSON for type message.Parts"),
// exactly when the transcript view was most needed to diagnose the death
// (observed in production on ses_01kx453ewfedqrg7p3c64f8sca and
// ses_01kx453ev9ejattygpf7rbzptw). Now a message that fails to marshal is
// replaced with a messagePlaceholder carrying its ID, role, and the marshal
// error, and every other message in the response is unaffected: the
// endpoint always returns 200 with as much of the transcript as is actually
// renderable.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	sess, _, ok := s.lookup(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	msgs := sess.History()
	out := make([]json.RawMessage, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		raw, err := json.Marshal(m)
		if err != nil {
			ph, phErr := json.Marshal(messagePlaceholder{
				ID:           m.ID,
				Role:         string(m.Role),
				MarshalError: err.Error(),
			})
			if phErr != nil {
				// messagePlaceholder is a plain string struct; this cannot
				// happen, but never let a placeholder failure reintroduce
				// the wholesale-500 this handler exists to prevent.
				ph = []byte(`{"id":"","role":"","marshal_error":"unmarshalable message and placeholder"}`)
			}
			raw = ph
		}
		out = append(out, raw)
	}
	writeJSON(w, http.StatusOK, out)
}

// requestJSON is the openapi Request shape: the latest fully-assembled model
// request for a session (canonical, in-memory only).
type requestJSON struct {
	Model    message.ModelRef `json:"model"`
	System   []string         `json:"system"`
	Tools    []string         `json:"tools"`
	Messages int              `json:"messages"`
	Params   paramsJSON       `json:"params"`
}

type paramsJSON struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
}

// handleRequest returns the latest fully-assembled request the process was
// about to send for a session. It reads memory only (full requests are never
// persisted), so a session that has not prompted this process is 404 —
// including a valid, on-disk session that has only been created.
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	snap := s.lastRequest[id]
	s.mu.Unlock()
	if snap == nil {
		writeErr(w, http.StatusNotFound, "no assembled request for session")
		return
	}
	system := snap.system
	if system == nil {
		system = []string{}
	}
	tools := snap.tools
	if tools == nil {
		tools = []string{}
	}
	writeJSON(w, http.StatusOK, requestJSON{
		Model:    snap.model,
		System:   system,
		Tools:    tools,
		Messages: snap.messages,
		Params: paramsJSON{
			Temperature: snap.temperature,
			TopP:        snap.topP,
			MaxTokens:   snap.maxTokens,
		},
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	type entry struct {
		Type     string        `json:"type"`
		State    string        `json:"state"`
		LastTurn *lastTurnJSON `json:"last_turn,omitempty"`
		Usage    usageJSON     `json:"usage"`
	}
	result := map[string]entry{}

	type snap struct {
		id      string
		running bool
		sess    *engine.Session
	}
	s.mu.Lock()
	mem := make([]snap, 0, len(s.sessions))
	for id, st := range s.sessions {
		mem = append(mem, snap{id, st.running, st.sess})
	}
	s.mu.Unlock()
	for _, m := range mem {
		result[m.id] = entry{
			Type:     statusStr(m.running),
			State:    s.compositeStateFor(m.id, m.running),
			LastTurn: s.lastTurnFor(m.id),
			Usage:    usageJSONForSession(m.sess),
		}
	}
	infos, err := engine.ListSessions(s.opts.SessionDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot list sessions")
		return
	}
	for _, info := range infos {
		if _, ok := result[info.ID]; !ok {
			result[info.ID] = entry{
				Type:     "idle",
				State:    s.compositeStateFor(info.ID, false),
				LastTurn: s.lastTurnFor(info.ID),
				Usage:    usageJSONForInfo(info),
			}
		}
	}
	writeJSON(w, http.StatusOK, result)
}

// lastTurnFor is lastTurnJSONLocked with its own locking, for callers (like
// handleStatus) that are not already holding s.mu.
func (s *Server) lastTurnFor(id string) *lastTurnJSON {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTurnJSONLocked(id)
}

// compositeStateFor resolves the composite state for a session ID using this
// process's goal tracker (see compositeState) — the same source Session JSON
// uses, so /session/status and GET /session/{id} agree.
func (s *Server) compositeStateFor(id string, running bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	goal := goalJSONFrom(s.goalState[id])
	return compositeState(running, goal != nil && goal.Active, forcesIdlePause(goal))
}

// promptAsyncResponse is POST /session/{id}/prompt_async's success-response
// body (see handlePrompt/enqueueOrDispatch): status is "started" when a
// prompt turn is now running (this call's own claim, or the freed-slot retry
// dispatching THIS request's own just-enqueued prompt — see
// enqueueOrDispatch's doc comment for the exact rule), or "queued" when this
// request's prompt is sitting in the durable FIFO waiting for a future
// drain. Queued carries the current queue depth (including this request's
// own prompt) only when status is "queued" — omitted (0) on "started",
// where it would be meaningless.
//
// One narrow exception: Queued reads 0 (and so is omitted, same JSON shape
// as "started") on a "queued" response when a concurrent DELETE
// /session/{id}/queue cleared the entire queue — including this request's
// own just-enqueued prompt — in the gap between this call's own enqueue and
// its dispatch-the-head attempt (see the two dispatchQueueHead call sites'
// doc comments). This is the most honest shape the existing vocabulary
// offers for "accepted, then cleared before it ran": the request was not an
// error (its prompt WAS durably enqueued and journaled), it simply never
// got the chance to run. See TestQueueClearRaceDuringIdleDispatchIsNotAnError
// and TestQueueClearRaceDuringDispatchIsNotAnError.
type promptAsyncResponse struct {
	Seq    int64  `json:"seq"`
	Status string `json:"status"`
	Queued int    `json:"queued,omitempty"`
}

// handlePrompt is POST /session/{id}/prompt_async (see docs/plans/2026-07-19-
// prompt-queue.md). An idle session claims its run slot exactly as before and
// starts running immediately ("started"). A session already busy with
// ANOTHER prompt or goal loop no longer 409s: the prompt is enqueued
// durably (engine.Session.EnqueuePrompt, synchronously, before any response
// is written — the accept-vs-lose race this closes is the same one
// RegisterGoal/handleGoalBusy already close for goals), then ONE claim retry
// is made to close the freed-slot race where the busy occupant finishes in
// the gap between the failed claim above and the enqueue (see
// enqueueOrDispatch). The workdir-held-by-ANOTHER-session 409 and the
// draining 503 are unchanged — only same-session busy gets queue semantics.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	var body struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
		Model message.ModelRef `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Parts) == 0 {
		writeErr(w, http.StatusBadRequest, "parts must be non-empty")
		return
	}
	var texts []string
	for _, p := range body.Parts {
		if p.Type != "text" {
			writeErr(w, http.StatusBadRequest, "v1 accepts text parts only")
			return
		}
		texts = append(texts, p.Text)
	}
	text := strings.Join(texts, "\n")

	// Resolve the session and atomically claim its prompt slot (also does the
	// wg.Add under the admission gate). See claimForPrompt for the ordering that
	// makes eviction races and drain admission impossible.
	st, ctx, fromSeq, code, holder := s.claimForPrompt(id)
	if code != 0 {
		switch {
		case code == http.StatusConflict && holder != "":
			writeErr(w, code, fmt.Sprintf("workdir busy: held by session %s", holder))
		case code == http.StatusConflict:
			// Same-session busy: queue-on-busy (invariant 9), not a 409.
			s.enqueueOrDispatch(w, id, text)
		case code == http.StatusServiceUnavailable:
			writeErr(w, code, "server shutting down")
		default:
			writeErr(w, http.StatusNotFound, "no such session")
		}
		return
	}

	if len(st.sess.QueuedPrompts()) > 0 {
		// Global FIFO on an idle-with-queue session: the queue can be
		// non-empty even though claimForPrompt just succeeded (the session
		// itself was idle) — a restart refold (TestQueueRestartRefoldNoAuto
		// Dispatch's queue survives a restart with the session left idle),
		// or a prompt stranded by a gap in some OTHER tail's drain wiring.
		// Either way, this request's own prompt must never jump the line:
		// enqueue it durably behind whatever is already waiting, then
		// dispatch the queue's HEAD (not necessarily this request's own
		// text) into the run slot just claimed above. See
		// dispatchQueueHead and enqueueOrDispatch's identical shape for the
		// same-session-BUSY counterpart of this same rule.
		ourID, err := st.sess.EnqueuePrompt(text)
		if err != nil {
			// handlePrompt already rejects an empty parts list and joins
			// non-empty text above, so this is not reachable in practice;
			// fail closed rather than silently drop the request, releasing
			// the claim just taken.
			s.releasePromptClaim(st)
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if s.queueDispatchRace != nil {
			// Test-only seam (see enqueueOrDispatch's identical use): let a
			// test force a concurrent DELETE /session/{id}/queue to land
			// deterministically in the gap between the EnqueuePrompt above
			// and the DequeuePrompt inside dispatchQueueHead below. Nil in
			// production.
			s.queueDispatchRace()
		}
		head, ok := s.dispatchQueueHead(id, st, ctx)
		if !ok {
			// Benign race, not a bug: a concurrent DELETE /session/{id}/queue
			// (safe to call regardless of run-slot state — see its own doc
			// comment) cleared the ENTIRE queue, including the prompt
			// EnqueuePrompt just added above, in the gap between that
			// enqueue and this dispatch attempt — dispatchQueueHead already
			// released the run-slot claim taken above, exactly like
			// runPrompt's own tail would. This request's own prompt WAS
			// accepted (durably enqueued and journaled) but never ran:
			// report the most honest shape the existing status vocabulary
			// offers — "queued" with the current (now necessarily zero)
			// depth — rather than a 500, which would misrepresent a benign,
			// documented race as a server bug. See
			// TestQueueClearRaceDuringIdleDispatchIsNotAnError and
			// promptAsyncResponse's queued field doc for why depth 0 is
			// possible here.
			writeJSON(w, http.StatusAccepted, promptAsyncResponse{
				Seq: fromSeq, Status: "queued", Queued: len(st.sess.QueuedPrompts()),
			})
			return
		}
		status := "queued"
		if head.ID == ourID {
			status = "started"
		}
		resp := promptAsyncResponse{Seq: fromSeq, Status: status}
		if status == "queued" {
			resp.Queued = len(st.sess.QueuedPrompts())
		}
		writeJSON(w, http.StatusAccepted, resp)
		return
	}

	// Explicit model wins over the session's persisted model (CLI -model
	// rule) -- applied only here, on the empty-queue fast path, because this
	// is the one branch where THIS request's own prompt is actually the one
	// about to run next. Applying it earlier (before the queue check above)
	// retargeted the session's model even when a DIFFERENT, already-queued
	// head was what actually got dispatched — contradicting the documented
	// "a per-request model override is silently dropped when the prompt is
	// queued" rule (see AGENTS.md's Prompt queue section and
	// enqueueOrDispatch's identical rule for the same-session-busy branch).
	// See TestQueuedArrivalDoesNotRetargetSessionModel.
	if !body.Model.IsZero() {
		before := st.sess.Model()
		st.sess.SetModel(body.Model)
		if st.sess.Model() != before {
			s.emitDurable(Event{Type: evtModel, SessionID: id, Model: body.Model})
		}
	}

	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})

	go s.runPrompt(ctx, id, st, text)
	writeJSON(w, http.StatusAccepted, promptAsyncResponse{Seq: fromSeq, Status: "started"})
}

// enqueueOrDispatch implements handlePrompt's same-session-busy branch:
// claimForPrompt 409'd with an empty holder, meaning something in THIS
// session (a running prompt or goal loop) already holds the run slot (the
// workdir-held-by-ANOTHER-session case is handled inline in handlePrompt and
// never reaches here — mirrors handleGoalBusy's same split).
//
// text is enqueued durably (EnqueuePrompt persists prompt.queued and emits
// its event before returning — see engine/queue.go) BEFORE any response is
// written, then ONE claim retry is made: this closes the race where the busy
// occupant's own tail (runPrompt/runGoal, which now calls
// maybeDispatchQueued — see its doc comment) runs between the failed claim
// in handlePrompt and this function's EnqueuePrompt call. If that happened,
// EnqueuePrompt is exactly what maybeDispatchQueued needed to see (it would
// have found the queue empty a moment earlier), so the retry here either
// wins the now-free slot itself or loses it to that same tail's own retry
// (via maybeDispatchQueued, whose own claim attempt may instead win) —
// either way this prompt starts exactly once, never zero times, never
// twice. This is the queue's counterpart to handleGoalBusy's register-then-
// retry pattern.
//
// On a WON retry, the head of the queue is dispatched — not necessarily this
// request's own prompt, since other prompts may already have been queued
// ahead of it (FIFO order is by queue ID, not by which HTTP request happens
// to observe the free slot first). The response's status reflects what
// happened to THIS request's own prompt specifically: "started" only if the
// dispatched head IS the prompt this call just enqueued; otherwise "queued"
// (this call's prompt is still waiting, now one place closer to the front).
// This is the simplest rule that stays correct regardless of how many other
// prompts were already queued: it never requires the caller to reason about
// queue position, only "is my own prompt running or not, right now".
//
// A model override on a request whose prompt gets queued (either branch) is
// silently NOT applied: QueuedPrompt carries only ID and Text (see the plan's
// "text-only" locked decision — no attachment machinery), so there is no
// slot to carry a per-prompt model override through to a future drain. A
// caller that needs a model swap to take effect should re-issue it once its
// prompt is confirmed "started".
func (s *Server) enqueueOrDispatch(w http.ResponseWriter, id string, text string) {
	sess := s.residentSession(id)
	if sess == nil {
		// Benign race window, identical to handleGoalBusy's (see its doc
		// comment): claimForPrompt found the session resident and running
		// (hence the 409 that routed us here), but s.mu is released between
		// that check and this residentSession call, and the busy occupant
		// finished and was evicted in the gap. A client retry resolves it
		// against a freshly (re)loaded, now-idle session.
		writeErr(w, http.StatusConflict, "session is busy with another prompt")
		return
	}
	ourID, err := sess.EnqueuePrompt(text)
	if err != nil {
		// handlePrompt already rejects an empty parts list and joins
		// non-empty text, so this is not reachable in practice; fail closed
		// rather than silently drop the request.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.queueDispatchRace != nil {
		// Test-only seam (mirrors autoArmRace): let a test force a real
		// concurrent claim to land here deterministically. Nil in production.
		s.queueDispatchRace()
	}
	st, ctx, _, code, _ := s.claimForPrompt(id)
	if code != 0 {
		// Lost the retry: still queued, whatever already occupies the slot
		// keeps running undisturbed.
		writeJSON(w, http.StatusAccepted, promptAsyncResponse{
			Seq: s.currentSeq(), Status: "queued", Queued: len(sess.QueuedPrompts()),
		})
		return
	}
	head, ok := s.dispatchQueueHead(id, st, ctx)
	if !ok {
		// Benign race, not a bug: a concurrent DELETE /session/{id}/queue
		// cleared the ENTIRE queue — including the prompt this call's own
		// EnqueuePrompt just added above — somewhere in the gap between
		// that enqueue and this dispatch attempt (the seam above is one
		// deterministic way a test can land squarely in that gap;
		// dispatchQueueHead has already released the run-slot claim taken
		// by claimForPrompt just above, exactly like runPrompt's own tail
		// would). This request's own prompt WAS accepted (durably enqueued
		// and journaled) but never ran: report the same honest shape
		// handlePrompt's idle-with-queue branch uses for this identical
		// race — "queued" with the current (now necessarily zero) depth —
		// rather than a 500, which would misrepresent a benign, documented
		// race as a server bug. See TestQueueClearRaceDuringDispatchIsNotAnError.
		writeJSON(w, http.StatusAccepted, promptAsyncResponse{
			Seq: s.currentSeq(), Status: "queued", Queued: len(sess.QueuedPrompts()),
		})
		return
	}

	status := "queued"
	if head.ID == ourID {
		status = "started"
	}
	resp := promptAsyncResponse{Seq: s.currentSeq(), Status: status}
	if status == "queued" {
		resp.Queued = len(sess.QueuedPrompts())
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// enqueueResponse is POST /session/{id}/enqueue's success body. Unlike
// promptAsyncResponse it never carries the journal's SSE seq — the field
// name "seq" is already taken by the request's own idempotency sequence,
// and an enqueue caller acks by watermark, not by event cursor.
// Watermark is the session's durable-enqueue high-water mark AFTER this
// request (== the request's own seq on accept; the pre-existing mark on
// duplicate). Queued mirrors promptAsyncResponse's rule: depth including
// this prompt, only when status is "queued".
type enqueueResponse struct {
	Status    string `json:"status"` // "started" | "queued" | "duplicate"
	Watermark int64  `json:"watermark"`
	Queued    int    `json:"queued,omitempty"`
}

// handleEnqueue is POST /session/{id}/enqueue (see docs/plans/2026-07-21-
// durable-enqueue.md): prompt_async's shape with an honest durability and
// idempotency contract. The prompt is fsynced into the session journal
// (engine.Session.EnqueuePromptDurable) BEFORE any success response — a 2xx
// authorizes the caller to ack ITS upstream — and a seq at or below the
// session's watermark is a 200 duplicate no-op, so upstream retries are
// always safe. Delivery is unchanged queue machinery: idle sessions
// dispatch the queue head immediately, busy sessions drain at turn/tool
// boundaries. No model override (queued prompts carry text only — see
// enqueueOrDispatch's doc comment); the workdir-busy 409, draining 503, and
// unknown-session 404 mirror handlePrompt.
func (s *Server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	var body struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
		Seq int64 `json:"seq"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Parts) == 0 {
		writeErr(w, http.StatusBadRequest, "parts must be non-empty")
		return
	}
	if body.Seq < 1 {
		writeErr(w, http.StatusBadRequest, "seq must be >= 1")
		return
	}
	var texts []string
	for _, p := range body.Parts {
		if p.Type != "text" {
			writeErr(w, http.StatusBadRequest, "v1 accepts text parts only")
			return
		}
		texts = append(texts, p.Text)
	}
	text := strings.Join(texts, "\n")
	// EnqueuePromptDurable rejects empty/whitespace-only text too, but by
	// then we'd have already taken (or failed to take) the run-slot claim,
	// and from the handler's side that engine error is indistinguishable
	// from a genuine persist failure — both fall through to the 500
	// "enqueue not durable" mapping below, which tells the caller to retry
	// with the same seq. An input that can never succeed must 400 instead,
	// and before any claim is taken.
	if strings.TrimSpace(text) == "" {
		writeErr(w, http.StatusBadRequest, "text must be non-empty")
		return
	}

	st, ctx, _, code, holder := s.claimForPrompt(id)
	if code != 0 {
		switch {
		case code == http.StatusConflict && holder != "":
			writeErr(w, code, fmt.Sprintf("workdir busy: held by session %s", holder))
		case code == http.StatusConflict:
			s.enqueueDurableBusy(w, id, text, body.Seq)
		case code == http.StatusServiceUnavailable:
			writeErr(w, code, "server shutting down")
		default:
			writeErr(w, http.StatusNotFound, "no such session")
		}
		return
	}

	// Idle: we hold the run slot. Durable-first, then dispatch the queue
	// HEAD — not necessarily this request's prompt (global FIFO, same rule
	// as handlePrompt's idle-with-queue branch).
	ourID, dup, err := st.sess.EnqueuePromptDurable(text, body.Seq)
	if dup {
		s.releasePromptClaim(st)
		// Stranded-head liveness fix: THIS request's prompt was a no-op,
		// but the run slot we just released may be stranding SOMEONE
		// ELSE's already-durable prompt. A concurrent same-seq retry can
		// land in enqueueDurableBusy while we hold the claim above, durably
		// enqueue there (advancing the watermark this call sees as a
		// duplicate), and then lose ITS OWN one-shot claim retry to us —
		// see enqueueDurableBusy's doc comment for that race. Without this
		// call, that prompt would sit in the queue on a now-idle session
		// with nothing left to dispatch it until unrelated future
		// activity. maybeDispatchQueued (see its doc comment) is built for
		// exactly this tail position: it re-claims the slot and dispatches
		// the head, or safely no-ops if the queue is empty or something
		// else wins the race — called BEFORE the response is written so
		// that a client polling GET /session/{id}/wait immediately after
		// this 2xx returns never observes a false "idle" ahead of the
		// drain's own claim. See TestEnqueueDuplicateOnIdleWithQueueDrainsHead.
		s.maybeDispatchQueued(id, st)
		writeJSON(w, http.StatusOK, enqueueResponse{Status: "duplicate", Watermark: st.sess.EnqueueSeq()})
		return
	}
	if err != nil {
		s.releasePromptClaim(st)
		// Same stranded-head exposure as the duplicate branch above: our
		// own durable enqueue failed, but a concurrent request may already
		// have durably queued behind us in enqueueDurableBusy and lost its
		// claim retry to us. Drain before responding, for the same reason.
		s.maybeDispatchQueued(id, st)
		writeErr(w, http.StatusInternalServerError, "enqueue not durable: "+err.Error())
		return
	}
	head, ok := s.dispatchQueueHead(id, st, ctx)
	if !ok {
		// Concurrent DELETE /session/{id}/queue cleared everything in the
		// gap — same benign race as handlePrompt's idle-with-queue branch;
		// the prompt WAS durably accepted (watermark advanced), which is
		// exactly what the response must attest.
		writeJSON(w, http.StatusAccepted, enqueueResponse{
			Status: "queued", Watermark: st.sess.EnqueueSeq(), Queued: len(st.sess.QueuedPrompts()),
		})
		return
	}
	resp := enqueueResponse{Status: "queued", Watermark: st.sess.EnqueueSeq()}
	if head.ID == ourID {
		resp.Status = "started"
	} else {
		resp.Queued = len(st.sess.QueuedPrompts())
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// enqueueDurableBusy is handleEnqueue's same-session-busy branch, the
// durable mirror of enqueueOrDispatch: durably enqueue (fsynced, error on
// failure — never a silent 2xx), then ONE claim retry to close the
// freed-slot race. See enqueueOrDispatch's doc comment for the race
// analysis; only the enqueue call and response shape differ.
func (s *Server) enqueueDurableBusy(w http.ResponseWriter, id string, text string, seq int64) {
	sess := s.residentSession(id)
	if sess == nil {
		// Same benign race window as enqueueOrDispatch: busy occupant
		// finished and was evicted between the failed claim and here. The
		// caller retries with the same seq — idempotency makes that free.
		writeErr(w, http.StatusConflict, "session is busy with another prompt")
		return
	}
	ourID, dup, err := sess.EnqueuePromptDurable(text, seq)
	if dup {
		writeJSON(w, http.StatusOK, enqueueResponse{Status: "duplicate", Watermark: sess.EnqueueSeq()})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "enqueue not durable: "+err.Error())
		return
	}
	if s.queueDispatchRace != nil {
		s.queueDispatchRace() // test-only seam, mirrors enqueueOrDispatch
	}
	st, ctx, _, code, _ := s.claimForPrompt(id)
	if code != 0 {
		writeJSON(w, http.StatusAccepted, enqueueResponse{
			Status: "queued", Watermark: sess.EnqueueSeq(), Queued: len(sess.QueuedPrompts()),
		})
		return
	}
	head, ok := s.dispatchQueueHead(id, st, ctx)
	if !ok {
		writeJSON(w, http.StatusAccepted, enqueueResponse{
			Status: "queued", Watermark: sess.EnqueueSeq(), Queued: len(sess.QueuedPrompts()),
		})
		return
	}
	resp := enqueueResponse{Status: "queued", Watermark: sess.EnqueueSeq()}
	if head.ID == ourID {
		resp.Status = "started"
	} else {
		resp.Queued = len(sess.QueuedPrompts())
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// releasePromptClaim releases a run-slot claim taken by claimForPrompt
// without running a turn: the exact reset runPrompt's own tail performs,
// shared by every path that claims the slot and then discovers there is
// nothing to run (an enqueue error, a queue emptied by a concurrent DELETE
// /session/{id}/queue).
func (s *Server) releasePromptClaim(st *sessionState) {
	s.mu.Lock()
	st.running = false
	st.cancel = nil
	st.goalLoop = false
	st.lastUsed = time.Now()
	s.evictResidentLocked()
	s.mu.Unlock()
	s.wg.Done()
}

// dispatchQueueHead dequeues the session's queue head (reason "delivered")
// into the run slot st/ctx already holds — emitting its busy transition and
// spawning its runPrompt turn — shared by every call site that has JUST
// claimed (or already holds) the run slot and knows the queue is (or was
// just made) non-empty: handlePrompt's idle-with-queue branch,
// enqueueOrDispatch's won-retry branch, and maybeDispatchQueued.
//
// ok is false only when the queue turns out to be empty despite the
// caller's own check — reachable solely via a benign concurrent DELETE
// /session/{id}/queue race between that check and this call. In that case
// the claim just taken is released here (mirrors runPrompt's own tail reset)
// so the run slot never gets stuck "running" with nothing driving it; the
// caller only needs to respond, not clean up.
func (s *Server) dispatchQueueHead(id string, st *sessionState, ctx context.Context) (head engine.QueuedPrompt, ok bool) {
	head, ok = st.sess.DequeuePrompt("delivered")
	if !ok {
		s.releasePromptClaim(st)
		return head, false
	}
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})
	go s.runPrompt(ctx, id, st, head.Text)
	return head, true
}

// runPrompt drives one Prompt to completion, then records the trailing
// messages and flips the session back to idle. The prompt's context is
// cancelled only by POST /abort, so a context.Canceled result is a deliberate
// abort — journaled as a durable session.aborted. Any other error is a genuine
// failure (provider error, transcode failure) — journaled as session.error
// with detail. Either way a durable record precedes the idle transition so a
// disconnected orchestrator learns the outcome on replay; the 202 only
// acknowledged receipt.
func (s *Server) runPrompt(ctx context.Context, id string, st *sessionState, text string) {
	defer s.wg.Done()
	_, err := st.sess.Prompt(ctx, text)
	s.syncMessages(id) // catch any message not yet journaled
	switch {
	case err == nil:
		s.recordTurnEnd(id, "completed", nil)
	case errors.Is(err, context.Canceled):
		s.emitDurable(Event{Type: evtSessionAborted, SessionID: id})
	default:
		s.emitDurable(Event{Type: evtSessionError, SessionID: id, Error: err.Error()})
		s.recordTurnEnd(id, turnEndOutcome(err), err)
	}
	s.mu.Lock()
	st.running = false
	st.cancel = nil
	st.goalLoop = false
	st.lastUsed = time.Now()
	s.evictResidentLocked()
	s.mu.Unlock()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "idle"})

	// Queue beats goal auto-arm (invariant 5): a prompt queued while this
	// turn ran outranks a goal merely waiting to auto-arm — direct user
	// input outranks the background objective. maybeDispatchQueued claims
	// the freed slot and runs the queue head if the queue is non-empty; only
	// when it reports nothing to dispatch (queue empty, or it lost the
	// race) do we fall through to maybeAutoArmGoal. Each dispatched queued
	// prompt's own runPrompt tail repeats this same check, so the queue
	// fully drains, one turn at a time, before the goal ever gets a look —
	// see maybeDispatchQueued's doc comment.
	if s.maybeDispatchQueued(id, st) {
		return
	}

	// Auto-arm (Task 5): a goal set (or self-adjust-set) mid-turn via the
	// `goal` session tool, or armed by a POST /goal that arrived while this
	// prompt was busy (handleGoalBusy's "armed" response), begins running
	// now instead of sitting active-but-idle until an operator happens to
	// re-poll. This runs AFTER the idle emit above so an SSE collector
	// always observes the prompt's idle before the goal's own busy (see
	// maybeAutoArmGoal's doc comment for the full race analysis).
	s.maybeAutoArmGoal(id, st)
}

// maybeDispatchQueued is called at the tail of both runPrompt (BEFORE
// maybeAutoArmGoal — see its call site above, invariant 5) and runGoal
// (below — goal termination frees the run slot too, and the engine's own
// turn-boundary drain, engine/goal.go's PursueGoal, only runs BETWEEN turns;
// a prompt queued after the loop's last turn boundary but before it actually
// terminates needs this hook to ever be dispatched). It runs after the
// just-finished turn's own idle transition has already been emitted.
//
// If the session's durable prompt queue is non-empty, it claims the run slot
// exactly like maybeAutoArmGoal, dequeues the head (reason "delivered"), and
// spawns a normal runPrompt turn for it. Returns true when it dispatched a
// turn, false when there was nothing queued or it lost the race.
//
// A losing race — the slot was claimed by an incoming prompt_async's own
// retry (enqueueOrDispatch), a POST /goal's auto-arm retry (handleGoalBusy),
// or another goroutine's own maybeDispatchQueued call — simply returns
// false: whichever request won the claim is now responsible for the
// session's next occupancy, and if that occupant is itself a plain prompt,
// its OWN runPrompt tail calls maybeDispatchQueued again once it finishes —
// so a queued prompt is never stranded, only delayed. See
// TestPromptQueueRaceWithFreedSlot.
//
// No-double-delivery equivalence (invariant 7, documentation only — nothing
// new to enforce beyond what already exists): DequeuePrompt("delivered")
// journals BEFORE the dispatched runPrompt call is even made, mirroring
// EnqueuePrompt's own persist-before-emit shape. A crash between that
// journal write and the dispatched turn's completion leaves the prompt gone
// from the queue on replay — it is not re-delivered, and its text is not
// recoverable from the queue a second time. This is not a new failure mode:
// it is the SAME exposure an ordinary in-flight prompt already has today (a
// crash mid-turn loses that turn's provider call and any partial response;
// replay simply resumes from the last durably appended message). A queued
// prompt becomes, for crash-recovery purposes, indistinguishable from a
// prompt that arrived directly via prompt_async and was already mid-flight
// when the process died, the instant it is dequeued and handed to
// runPrompt. See engine/goal.go's DequeueAllPrompts callsite for the
// engine-side half of this same equivalence (goal-turn injection).
func (s *Server) maybeDispatchQueued(id string, st *sessionState) bool {
	if len(st.sess.QueuedPrompts()) == 0 {
		return false
	}
	if s.queueDispatchRace != nil {
		s.queueDispatchRace()
	}
	claimedSt, ctx, _, code, _ := s.claimForPrompt(id)
	if code != 0 {
		return false // lost the race; see the doc comment above
	}
	// The queue was drained by a concurrent DELETE /session/{id}/queue
	// between the len check above and winning this claim: dispatchQueueHead
	// already released the claim we just took (mirrors runPrompt's own tail
	// reset) — nothing left to dispatch.
	_, ok := s.dispatchQueueHead(id, claimedSt, ctx)
	return ok
}

// maybeAutoArmGoal is called once, at the very tail of runPrompt — never
// from runGoal's tail (see below) — after the prompt's own idle transition
// has already been emitted. If this server has a configured goal evaluator
// and the session's engine-level goal is active with no loop currently
// attached (armed by a POST /goal that arrived while this prompt was busy —
// see handleGoalBusy's "armed" 202 — or by the `goal` session tool's own
// `set` action invoked mid-turn, per docs/plans/2026-07-19-goal-self-
// adjust.md's headline user story), the goal loop starts running right now
// instead of waiting for the next external poke.
//
// It reclaims the run slot itself via claimForPrompt, exactly like a fresh
// POST /goal would. A losing race — the slot got claimed by an incoming
// prompt_async or by handleGoalBusy's own single retry (see its doc
// comment) between runPrompt's unclaim and this call — simply returns
// without starting a second loop: whichever request won the claim is now
// responsible for the session's next occupancy, and if that occupant is
// itself a plain prompt, ITS OWN runPrompt tail will call maybeAutoArmGoal
// again once it finishes, so the still-active goal is never stranded armed
// forever — it just waits one more prompt's length of time. See
// TestAutoArmRaceWithIncomingPrompt.
//
// Deliberately NOT called from runGoal's tail: every terminal outcome of
// PursueGoal (achieved, cleared, max-turns-exhausted, or a permanent error)
// either deactivates the goal or leaves it in the same "active, ordinarily
// re-armable via POST /goal" state a goal has always been left in — none of
// those is the "armed, waiting for a busy run slot to free up" state
// auto-arm exists to bridge. Wiring auto-arm into runGoal's own tail as well
// would add nothing this design needs while risking a self-sustaining spin
// if a future change ever left a loop exiting with the goal still "active
// and freshly re-armable" in the auto-arm sense.
func (s *Server) maybeAutoArmGoal(id string, st *sessionState) {
	if s.opts.GoalEvaluator.IsZero() {
		return
	}
	condition, active := st.sess.ActiveGoal()
	if !active {
		return
	}
	if s.autoArmRace != nil {
		s.autoArmRace()
	}
	claimedSt, ctx, _, code, _ := s.claimForPrompt(id)
	if code != 0 {
		return // lost the race; see the doc comment above
	}
	s.mu.Lock()
	claimedSt.goalLoop = true
	// Activity-driven resume of a paused goal (restart, worker_failure,
	// or a stale retryable-backoff fold): a plain prompt
	// completing is exactly what re-attaches a loop to a goal left armed
	// with no loop running — reset the FULL pause presentation here via the
	// same helper handleGoal's own re-arm branch uses, so the freshly
	// spawned loop below is never seen wearing a stale paused presentation
	// from before it started (see resetGoalPauseLocked's doc comment for
	// why every field, not just pausedWorker, must reset here).
	resetGoalPauseLocked(s.goalState[id])
	s.mu.Unlock()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})
	go s.runGoal(ctx, id, claimedSt, condition, 0)
}

// goalPostResponse is POST /session/{id}/goal's success-response body: seq to
// tail events from, plus status naming which of the three outcomes happened:
//   - "started": a fresh loop is now running (the session was idle, or the
//     retry in handleGoalBusy's "not active" branch won the freed slot).
//   - "armed": the goal is registered, but the run slot is still held by a
//     plain prompt; maybeAutoArmGoal starts the loop once that prompt ends.
//   - "updated": an already-running loop's condition was rewritten in place;
//     no new loop, no run-slot claim.
type goalPostResponse struct {
	Seq    int64  `json:"seq"`
	Status string `json:"status"`
}

// handleGoal starts, updates, or arms a goal loop on a session — see
// docs/plans/2026-07-19-goal-self-adjust.md's Task 5 for the full design.
// Like prompt_async it claims the session's single run slot when the
// session is idle; the evaluator model comes from Options.GoalEvaluator
// (config goal_evaluator_model), and goals are rejected with 400 when it is
// unset.
//
// When claimForPrompt succeeds (the session was idle), three outcomes:
//   - no goal active: RegisterGoal, spawn runGoal, "started".
//   - a goal is active (a paused/restart goal, or one left active-but-idle
//     by an abort — see PursueGoal's context.Canceled branch) with the SAME
//     condition: just resume it, "started".
//   - active with a DIFFERENT condition: UpdateGoal rewrites it in place,
//     then resume with the new condition, "started". This is the one
//     behavior change from the pre-Task-5 contract, which 409'd here
//     instead (see TestGoalReArmDifferentConditionUpdatesAndResumes) —
//     claimForPrompt's success proves no loop is currently running to race
//     UpdateGoal with, so updating in place is always safe here.
//
// When claimForPrompt 409s because THIS session's own run slot is already
// held (empty holder — the non-empty-holder, workdir-held-by-ANOTHER-
// session case is handled inline below, unchanged), handleGoalBusy takes
// over: update-in-place (invariant 7) if a goal loop holds the slot, or
// register-and-arm (invariants 8/9) if a plain prompt does.
func (s *Server) handleGoal(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	if s.opts.GoalEvaluator.IsZero() {
		writeErr(w, http.StatusBadRequest, "goal_evaluator_model is not configured; goals are unavailable")
		return
	}
	var body struct {
		Condition string `json:"condition"`
		MaxTurns  int    `json:"max_turns"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Condition) == "" {
		writeErr(w, http.StatusBadRequest, "condition must be non-empty")
		return
	}

	st, ctx, fromSeq, code, holder := s.claimForPrompt(id)
	if code != 0 {
		switch {
		case code == http.StatusConflict && holder != "":
			writeErr(w, code, fmt.Sprintf("workdir busy: held by session %s", holder))
		case code == http.StatusConflict:
			s.handleGoalBusy(w, id, body.Condition, body.MaxTurns)
		case code == http.StatusServiceUnavailable:
			writeErr(w, code, "server shutting down")
		default:
			writeErr(w, http.StatusNotFound, "no such session")
		}
		return
	}
	// Re-arming a paused/restart (or post-abort) goal. claimForPrompt above
	// only 409s on st.running — it knows nothing about goal state — so
	// reaching here with the engine already reporting an active goal means
	// exactly one thing: this session's goal is active with NO loop
	// attached in this process. A genuinely running loop would already have
	// 409'd via st.running above (handleGoal/runGoal hold the claim for the
	// whole PursueGoal call), so this branch is unreachable for a live
	// provider-backoff park — only the boot-time restart pause (see
	// pauseArmedGoalsAtBoot), an abort that deliberately left the goal
	// active (see PursueGoal's context.Canceled branch), or an equivalent
	// crash-before-spawn window reaches it.
	condition := body.Condition
	if existing, active := st.sess.ActiveGoal(); active {
		if existing != body.Condition {
			// See this function's doc comment: a different condition here
			// now updates and resumes instead of rejecting. UpdateGoal can
			// only fail on "no active goal", which ActiveGoal() just ruled
			// out — structurally unreachable, but fail closed rather than
			// silently resuming the wrong condition if that ever changes.
			if err := st.sess.UpdateGoal(body.Condition); err != nil {
				s.mu.Lock()
				st.running = false
				st.cancel = nil
				st.goalLoop = false
				st.lastUsed = time.Now()
				s.mu.Unlock()
				s.wg.Done()
				writeErr(w, http.StatusConflict, err.Error())
				return
			}
		}
		condition = body.Condition
		s.mu.Lock()
		// Reset ALL pause-relevant fold state via the shared helper,
		// mirroring the evtGoalSet fold: if the journal tail before a
		// restart was goal.stalled(retryable, waiting), clearing only
		// pausedRestart leaves pauseView's provider-backoff case firing on
		// a freshly re-armed, genuinely-running goal until its first
		// goal.eval resets waiting. pausedWorker (Task 2) needs
		// the same treatment: a goal left worker-parked (journal tail
		// goal.parked, no loop attached) reaches this exact branch too —
		// claimForPrompt succeeded, so nothing was running — and must not
		// still read paused/worker_failure the instant this re-arm's fresh
		// loop starts. See resetGoalPauseLocked's doc comment — this is
		// also maybeAutoArmGoal's own reset site.
		resetGoalPauseLocked(s.goalState[id])
		s.mu.Unlock()
	} else if err := st.sess.RegisterGoal(body.Condition); err != nil {
		// Register the goal synchronously BEFORE the loop goroutine spawns
		// and before the 202 returns: by the time the caller can DELETE,
		// the goal is active and clearable — the accept-vs-clear race is
		// structurally gone.
		//
		// Undo the claim taken above: mirror the tail of runPrompt/runGoal.
		s.mu.Lock()
		st.running = false
		st.cancel = nil
		st.goalLoop = false
		st.lastUsed = time.Now()
		s.mu.Unlock()
		s.wg.Done()
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	s.mu.Lock()
	st.goalLoop = true
	s.mu.Unlock()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})

	go s.runGoal(ctx, id, st, condition, body.MaxTurns)
	writeJSON(w, http.StatusAccepted, goalPostResponse{Seq: fromSeq, Status: "started"})
}

// handleGoalBusy implements handleGoal's same-session-busy branch:
// claimForPrompt 409'd with an empty holder, meaning something in THIS
// session already holds the run slot — either a running goal loop or a
// plain prompt (the workdir-held-by-ANOTHER-session case is handled inline
// in handleGoal and never reaches here).
func (s *Server) handleGoalBusy(w http.ResponseWriter, id string, condition string, maxTurns int) {
	sess := s.residentSession(id)
	if sess == nil {
		// Reachable, in a narrow window: claimForPrompt found the session
		// resident and running (hence the 409 that routed us here), but
		// s.mu is released between that check and this residentSession
		// call. If the busy prompt/goal finishes in that gap, the session
		// goes idle and an eviction sweep (evictResidentLocked, run from
		// several other request paths) can unload it before we look. The
		// 409 below is benign — nothing was mutated — and a client retry
		// resolves it against a freshly (re)loaded, now-idle session.
		writeErr(w, http.StatusConflict, "session is busy")
		return
	}
	if existing, active := sess.ActiveGoal(); active {
		// A goal loop is running RIGHT NOW: claimForPrompt's 409 plus an
		// active goal can only mean the loop itself holds the slot for the
		// whole PursueGoal call (see runGoal) — a plain prompt never leaves
		// ActiveGoal() true. Update in place: no second loop, no run-slot
		// claim (invariant 7).
		if existing != condition {
			if err := sess.UpdateGoal(condition); err != nil {
				writeErr(w, http.StatusConflict, err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, goalPostResponse{Seq: s.currentSeq(), Status: "updated"})
		return
	}

	// No goal active: a plain prompt occupies the slot. Register the goal
	// now — RegisterGoal needs no run slot — so it exists the instant this
	// call returns, then retry the claim ONCE. This closes the race where
	// the prompt's own runPrompt tail (and its maybeAutoArmGoal auto-arm
	// check) runs between our failed claim above and this RegisterGoal: if
	// that happened, RegisterGoal is exactly what maybeAutoArmGoal was
	// waiting to see, and this retry either wins the now-free slot itself
	// (spawning the loop here) or loses it to that same auto-arm call
	// (which will already have spawned it) — either way the goal starts
	// exactly once, never zero times, never twice. See maybeAutoArmGoal's
	// doc comment for the other half of this argument.
	if err := sess.RegisterGoal(condition); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	st, ctx, fromSeq, code, _ := s.claimForPrompt(id)
	if code == 0 {
		s.mu.Lock()
		st.goalLoop = true
		s.mu.Unlock()
		s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})
		go s.runGoal(ctx, id, st, condition, maxTurns)
		writeJSON(w, http.StatusAccepted, goalPostResponse{Seq: fromSeq, Status: "started"})
		return
	}
	writeJSON(w, http.StatusAccepted, goalPostResponse{Seq: s.currentSeq(), Status: "armed"})
}

// runGoal drives one PursueGoal to completion, then flips the session back to
// idle. The loop's context is cancelled only by DELETE /goal (which journals
// goal.cleared BEFORE cancelling, see handleGoalDelete) or by Drain at
// shutdown; a context.Canceled result is therefore a deliberate stop, not a
// failure, and needs no session.error. Any other error is journaled as
// session.error. Message journaling piggybacks on the same syncMessages path
// as runPrompt.
//
// A worker-parked error (engine.IsGoalWorkerParked — either
// exhaustion tier exit-parking instead of clearing) falls into the default
// branch below like any other error: session.error, then turn.end via
// turnEndOutcome, which maps it to outcomeWorkerParked rather than the
// generic "error". This is NOT a clear — engine/goal.go's goal.parked record
// (journaled by PursueGoal itself, under its own lock, before returning —
// always ordered before this function's own session.error/turn.end) leaves
// the goal fully active; publishGoal folds it into goalTracker.pausedWorker,
// the third paused arm (pause_reason "worker_failure", see pauseView), which
// forces compositeState to idle exactly like a restart pause. This function
// deliberately does NOT auto-arm a fresh loop for it (see maybeDispatchQueued
// below and maybeAutoArmGoal's own doc comment for why runGoal's tail never
// auto-arms) — resume is entirely activity-driven, via the next plain
// prompt's own runPrompt tail.
//
// turn.end outcome is decided from the RESULT, not merely from err == nil:
// PursueGoal returns a nil error with Achieved:false in two cases that are
// emphatically not "completed" —
//
//   - MaxTurns exhausted without the evaluator ever returning MET (Reason
//     "max turns"): the goal gave up, it did not finish. That is recorded as
//     its own outcomeMaxTurnsExceeded, never "completed" — see PR #55 review
//     finding: keying turn.end on err == nil alone told a poller "idle
//     because done" for a goal that was never met, exactly the ambiguity
//     this primitive exists to remove.
//   - ClearGoal won a race against an in-flight worker retry or evaluator
//     call (Reason "goal cleared"), without the loop's own context ever
//     being cancelled. This is a clear, same as the context.Canceled path
//     below, just reached without cancellation — and the openapi contract
//     is that DELETE /goal (or any clear) never emits turn.end. So this case
//     suppresses the record entirely, matching the context.Canceled branch.
//
// The terminal session.status idle record emitted at the end of this
// function is the same record an SSE collector waits for as the session's
// "occupancy over" signal (collect-until-idle is the wire contract). DELETE
// /goal's clear-before-cancel ordering guarantees goal.cleared always
// precedes it in the journal — this function must never emit idle before a
// goal.cleared that is still in flight.
func (s *Server) runGoal(ctx context.Context, id string, st *sessionState, condition string, maxTurns int) {
	defer s.wg.Done()
	res, err := st.sess.PursueGoal(ctx, condition, engine.GoalOptions{
		Registered: true,
		MaxTurns:   maxTurns,
		Evaluator:  s.opts.GoalEvaluator,
	})
	s.syncMessages(id)
	switch {
	case err == nil && res.Achieved:
		s.recordTurnEnd(id, "completed", nil)
	case err == nil && res.Reason == "goal cleared":
		// Cleared in flight without the context being cancelled: goal.cleared
		// is already journaled (ClearGoal/handleGoalDelete); no turn.end, same
		// contract as the context.Canceled case below.
	case err == nil:
		// Any other nil-error, non-achieved result is MaxTurns exhaustion —
		// PursueGoal's only remaining terminal case (see its doc comment).
		s.recordTurnEnd(id, outcomeMaxTurnsExceeded, nil)
	case errors.Is(err, context.Canceled):
		// Cleared via DELETE (goal.cleared already journaled) or drained.
	default:
		s.emitDurable(Event{Type: evtSessionError, SessionID: id, Error: err.Error()})
		s.recordTurnEnd(id, turnEndOutcome(err), err)
	}
	s.mu.Lock()
	st.running = false
	st.cancel = nil
	st.goalLoop = false
	st.lastUsed = time.Now()
	s.evictResidentLocked()
	s.mu.Unlock()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "idle"})

	// A prompt queued after the loop's last turn-boundary drain (engine/
	// goal.go's PursueGoal only drains BETWEEN turns) but before the loop
	// actually terminated would otherwise sit queued indefinitely once the
	// loop is gone — this is that gap's dispatch hook. Unlike runPrompt's
	// tail, this is never followed by maybeAutoArmGoal: every terminal
	// PursueGoal outcome either deactivates the goal or leaves it in the
	// ordinary "active, re-armable via POST /goal" state it has always been
	// left in, never the "auto-arm is watching for this" state (see
	// maybeAutoArmGoal's own doc comment for why it is deliberately not
	// wired in here either).
	s.maybeDispatchQueued(id, st)
}

// handleGoalDelete cancels an active goal loop: it clears the goal (journaling
// goal.cleared and resetting the engine's goal state), THEN cancels the loop
// context (stopping further turns) -- but ONLY when the run slot's current
// occupant IS a goal loop (st.goalLoop). A goal can be active while a PLAIN
// PROMPT holds the slot (the 202 "armed" path -- see handleGoalBusy's
// register-and-arm branch and maybeAutoArmGoal): in that window st.cancel
// belongs to the prompt, not to any loop, and cancelling it would abort that
// prompt's turn (typically the very turn that armed the goal via the `goal`
// session tool). See TestDeleteGoalDuringArmedPromptLeavesPromptRunning.
// Clearing the goal is enough in that case: maybeAutoArmGoal's own tail check
// (run when the prompt finishes) reads ActiveGoal() as false and no-ops, so
// no loop ever starts. Unknown session (not resident, no log on disk) is
// 404; a known session is 204 whether or not a goal was active (idempotent
// -- no goal.cleared is journaled when nothing was active).
//
// Ordering guarantee: goal.cleared is always journaled before the
// session.status idle record that ends that goal's occupancy (see runGoal and
// engine.Session.ClearGoal). This is why clear happens before cancel, not
// after: cancelling first would let the goal-loop worker's context-
// cancellation unwind — which ends in that terminal idle record — race the
// handler to the journal, and an SSE collector that reads until idle (the
// wire contract every client relies on) could see goal.set but never
// goal.cleared.
//
// Non-resident-but-on-disk case (issue #78): a session with no in-memory
// sessionState at all is not necessarily gone -- it may be exactly the
// boot-time restart-paused goal (pauseArmedGoalsAtBoot): active in the
// journal, paused/restart in goalState, with no loop ever attached in this
// process. That is precisely the case an operator needs DELETE /goal to be
// able to clear. The old code's `st != nil` guard skipped ClearGoal for it
// entirely -- still returning 204 (nothing to reject), but journaling
// nothing, never flipping engine.Session.goalActive, and leaving
// goalState[id].active true so the goal re-paused at the next boot. See
// TestDeleteGoalNonResidentClearsAndJournals for the red/green case.
func (s *Server) handleGoalDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	st := s.sessions[id]
	s.mu.Unlock()
	if st == nil {
		// Not resident: load it from disk exactly like claimForPrompt's cold
		// path -- LoadSession OUTSIDE s.mu (it may hit disk), then re-acquire
		// the lock and re-check for a resident that appeared in the
		// meantime (a concurrent POST /goal or /prompt_async racing us),
		// using that winner instead so two *engine.Session instances for the
		// same log are never both mutated. See claimForPrompt's doc comment
		// for the full race argument this mirrors.
		//
		// The freshly loaded session is made resident here (lastUsed set,
		// evictResidentLocked invoked) -- deliberately, rather than used
		// transiently and discarded. That keeps exactly one "load a cold
		// session into residency" shape in this server (claimForPrompt's),
		// instead of a second copy of its race handling that would need to
		// be kept in sync by hand. It costs nothing beyond one ordinary
		// MaxResident slot: the loaded session is idle (running/goalLoop are
		// both the zero value false), so it is immediately eviction-eligible
		// like any other idle resident on the next evictResidentLocked
		// sweep.
		sess, err := s.opts.LoadSession(id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "no such session")
			return
		}
		s.mu.Lock()
		if ex := s.sessions[id]; ex != nil {
			st = ex // a resident appeared while we loaded; use the winner
		} else {
			st = &sessionState{sess: sess, lastUsed: time.Now()}
			s.sessions[id] = st
			s.evictResidentLocked()
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	var cancel context.CancelFunc
	if st.goalLoop {
		cancel = st.cancel
	}
	s.mu.Unlock()
	// Orderly shutdown: clear BEFORE cancel. ClearGoal journals goal.cleared
	// and emits the event synchronously, under the session's own lock,
	// before it returns (see engine.Session.ClearGoal) — so by the time
	// cancel() below wakes the goal-loop worker, goal.cleared is already in
	// the durable journal. Cancelling first would let the worker's unwind
	// (which ends in the terminal session.status idle record, see runGoal)
	// race the handler to the journal: on an unlucky scheduling the idle
	// record could land before goal.cleared, and an SSE collector that reads
	// until idle (the wire contract every client relies on) would never see
	// the clear. See TestGoalDeleteClearBeforeIdleRace, which forces that
	// worst case deterministically.
	//
	// ClearGoal journals goal.cleared (via OnEvent -> publishGoal, wired the
	// same way whether st.sess came from claimForPrompt's residency, this
	// handler's own cold-load branch above, or was already resident) and
	// resets the engine goal state; a no-op when no goal is active.
	st.sess.ClearGoal()
	if s.goalDeleteRace != nil {
		// Handed cancel (rather than a bare notification) so a test can force
		// the worst case unconditionally: fire the worker's unblock as early
		// as structurally possible — right here, before this function's own
		// cancel() below — and ride out its unwind to completion before
		// letting this handler proceed. See TestGoalDeleteClearBeforeIdleRace.
		s.goalDeleteRace(cancel)
	}
	if cancel != nil {
		cancel() // stop the loop; runGoal treats context.Canceled as a clean stop (no-op if the hook above already fired it)
	}
	w.WriteHeader(http.StatusNoContent)
}

// evictResidentLocked unloads the longest-idle non-busy sessions from memory
// when the resident count exceeds Options.MaxResident. Busy sessions are never
// evicted; s.seen is retained so journal idempotency survives the unload (the
// session reloads transparently from disk on its next access). Caller holds
// s.mu.
func (s *Server) evictResidentLocked() {
	excess := len(s.sessions) - s.opts.MaxResident
	if excess <= 0 {
		return
	}
	type cand struct {
		id   string
		last time.Time
	}
	cands := make([]cand, 0, len(s.sessions))
	for id, st := range s.sessions {
		if st.running {
			continue // busy sessions hold an in-flight prompt; keep them resident
		}
		cands = append(cands, cand{id, st.lastUsed})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].last.Before(cands[j].last) })
	for i := 0; i < excess && i < len(cands); i++ {
		delete(s.sessions, cands[i].id)
		// Release the request snapshot (it holds a full copy of the
		// assembled system segments). lastReqHash survives deliberately:
		// it is small and keeps hash-on-change journaling correct if the
		// session is later reloaded.
		delete(s.lastRequest, cands[i].id)
	}
}

// handleAbort interrupts a session's in-flight prompt. Unknown session (not
// resident and no session log on disk) is 404; a known session is 204 whether
// or not anything was running (idempotent). A non-resident session cannot
// have a prompt in flight, so a bare existence check suffices — the abort
// never loads it into memory.
func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	st := s.sessions[id]
	var cancel context.CancelFunc
	if st != nil {
		cancel = st.cancel
	}
	s.mu.Unlock()
	if st == nil && !s.sessionOnDisk(id) {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	if cancel != nil {
		cancel()
	}
	w.WriteHeader(http.StatusNoContent)
}

// queueGetResponse is GET /session/{id}/queue: the durable-enqueue
// watermark plus the pending (undelivered) prompt queue in FIFO order.
// Queued is always present (empty array, never null) so consumers need no
// nil check. Seq is 0/omitted on plain prompt_async-queued entries.
type queueGetResponse struct {
	Watermark int64            `json:"watermark"`
	Queued    []queuedItemJSON `json:"queued"`
}

type queuedItemJSON struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
	Seq  int64  `json:"seq,omitempty"`
}

// handleQueueGet is the reconciliation read surface for durable enqueue
// (see docs/plans/2026-07-21-durable-enqueue.md): an upstream recovering
// from its own crash reads the watermark to learn which messages are
// already accepted rather than re-sending blind. It resolves the session
// via s.lookup — the same resolve-or-load helper handleGet uses for every
// other read endpoint: resident sessions answer from live state, and a
// non-resident session gets a transparent transient load (idle status,
// same as GET /session/{id}). Unlike handleQueueDelete's cold path, this
// transient load is deliberately NOT registered into residency and takes no
// run-slot claim — a read must never have those side effects. Resident and
// non-resident answers can never disagree: both are folds of the exact same
// on-disk journal.
func (s *Server) handleQueueGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	sess, _, ok := s.lookup(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	watermark, prompts := sess.QueueState()
	resp := queueGetResponse{Watermark: watermark, Queued: []queuedItemJSON{}}
	for _, p := range prompts {
		resp.Queued = append(resp.Queued, queuedItemJSON{ID: p.ID, Text: p.Text, Seq: p.Seq})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleQueueDelete is DELETE /session/{id}/queue (invariant 10): drains the
// session's durable prompt queue, journaling a prompt.dequeued(reason=
// "cleared") record for every pending item (see
// engine.Session.DequeueAllPrompts), then 204. Idempotent on an already-empty
// queue (still 204, nothing journaled). A running turn is left completely
// untouched — this clears only prompts waiting for a FUTURE turn, never
// cancels the current one (see POST /session/{id}/abort for that); a running
// goal loop's later turn-boundary drains simply see an empty queue, exactly
// as if the clear had raced ahead of them.
//
// The session is resolved exactly like handleGoalDelete's cold path, NOT via
// a bare s.lookup: a not-resident session is loaded from disk OUTSIDE s.mu,
// then s.mu is re-acquired to re-check for a resident that appeared in the
// meantime (a concurrent POST /prompt_async or /goal racing us), using that
// winner instead — registering the freshly loaded instance into residency
// otherwise — so DequeueAllPrompts below always mutates the exact
// *engine.Session instance every future drain (maybeDispatchQueued) actually
// reads. A bare transient load (the old behavior) would let a concurrent
// cold-load-and-register elsewhere win residency with its OWN, divergent
// instance: the clear would land on a copy nothing else ever touches again
// — 204 and even a durable prompt.dequeued(cleared) record, journaled via
// the shared OnEvent wiring, while the session that matters keeps
// dispatching the "cleared" prompts. See
// TestDeleteQueueColdSessionSurvivesResidencyRace and claimForPrompt's doc
// comment for the same race argument.
//
// DequeueAllPrompts takes only the engine session's own mutex and persists
// synchronously to its log, so this works correctly whether the resolved
// session is idle, busy with a prompt, or mid goal-loop, with no run-slot
// claim involved at all. Unknown session (not resident, no log on disk) is
// 404.
func (s *Server) handleQueueDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	st := s.sessions[id]
	s.mu.Unlock()
	if st == nil {
		sess, err := s.opts.LoadSession(id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "no such session")
			return
		}
		if s.queueDeleteRace != nil {
			// Test-only seam: let a test force a real concurrent claim (a
			// prompt_async cold-loading and registering its own instance) to
			// land here deterministically. Nil in production.
			s.queueDeleteRace()
		}
		s.mu.Lock()
		if ex := s.sessions[id]; ex != nil {
			st = ex // a resident appeared while we loaded; use the winner
		} else {
			st = &sessionState{sess: sess, lastUsed: time.Now()}
			s.sessions[id] = st
			s.evictResidentLocked()
		}
		s.mu.Unlock()
	}
	st.sess.DequeueAllPrompts("cleared")
	w.WriteHeader(http.StatusNoContent)
}

// compactResponseJSON is the openapi POST /session/{id}/compact response
// shape (docs/design/context-compaction.md §1): turns_folded is 0 (not an
// error) when there was nothing worth folding — see engine.CompactResult.
type compactResponseJSON struct {
	TurnsFolded int              `json:"turns_folded"`
	FirstID     string           `json:"first_id,omitempty"`
	LastID      string           `json:"last_id,omitempty"`
	Summary     *message.Message `json:"summary,omitempty"`
}

// handleCompact is POST /session/{id}/compact (docs/design/context-
// compaction.md §1 "Explicit: POST /session/{id}/compact"): always
// available regardless of the automatic threshold. It claims the session's
// single run slot exactly like prompt_async/goal (409 if already running,
// 503 if draining) — compaction never runs concurrently with a turn — then
// runs synchronously (the response carries the full result, so there is no
// async job to poll). Optional JSON body {"keep_turns": N, "model": "..."}
// overrides Config.CompactionKeepTurns/CompactionModel for this call only;
// keep_turns has a hard floor of 1 — 0 or negative is a 400, never silently
// clamped, so a caller's mistake is visible rather than silently ignored.
//
// Its tail calls maybeDispatchQueued then maybeAutoArmGoal, the same
// order/precedence runPrompt's tail uses (queue beats goal auto-arm): a
// prompt queued or a goal armed while compact ran must drain/start the
// instant this call releases the run slot, not wait for some later
// runPrompt/runGoal tail to happen to fire first.
func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	var body struct {
		KeepTurns *int   `json:"keep_turns"`
		Model     string `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.KeepTurns != nil && *body.KeepTurns <= 0 {
		writeErr(w, http.StatusBadRequest, "keep_turns must be >= 1")
		return
	}
	var model message.ModelRef
	if body.Model != "" {
		m, err := message.ParseModelRef(body.Model)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		model = m
	}

	st, ctx, _, code, holder := s.claimForPrompt(id)
	if code != 0 {
		switch {
		case code == http.StatusConflict && holder != "":
			writeErr(w, code, fmt.Sprintf("workdir busy: held by session %s", holder))
		case code == http.StatusConflict:
			writeErr(w, code, "session is busy with another prompt")
		case code == http.StatusServiceUnavailable:
			writeErr(w, code, "server shutting down")
		default:
			writeErr(w, http.StatusNotFound, "no such session")
		}
		return
	}
	// Deferred (not called bare at the tail) so it runs even if Compact or
	// either of the tail's own maybeDispatchQueued/maybeAutoArmGoal calls
	// panics -- a bare call would never execute past a panic, leaking this
	// claim's wg.Add and hanging Drain forever. A defer here still runs
	// strictly after those tail calls (defers fire after the function body's
	// remaining statements, on any return path — panic or normal), so the
	// wg.Add-before-wg.Done ordering those calls rely on (see the comment at
	// the call site below) is unchanged; this only adds panic-safety, same
	// shape as runPrompt's/runGoal's own `defer s.wg.Done()`. See
	// TestCompactPanicReleasesClaim.
	defer s.wg.Done()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})

	opts := engine.CompactOptions{Model: model}
	if body.KeepTurns != nil {
		opts.KeepTurns = *body.KeepTurns
	}
	res, err := st.sess.Compact(ctx, opts)
	// Session.Compact's own emits (EventMessage for the summary, then
	// EventHistoryCompacted — see engine/compact.go) already flowed through
	// Publish synchronously by the time Compact returns, journaling the
	// summary message and the durable history.compacted record in that
	// order (see publishHistoryCompacted). syncMessages here is a harmless,
	// idempotent extra pass — the same belt-and-suspenders every other
	// handler's tail already relies on.
	s.syncMessages(id)

	s.mu.Lock()
	st.running = false
	st.cancel = nil
	st.goalLoop = false
	st.lastUsed = time.Now()
	s.evictResidentLocked()
	s.mu.Unlock()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "idle"})

	// Same drain-then-auto-arm precedence as runPrompt's tail (invariant 5):
	// a prompt queued (or a goal armed) while this compact call ran must not
	// sit stranded just because the run slot happened to be released by
	// compact instead of an ordinary prompt or goal turn — see
	// maybeDispatchQueued/maybeAutoArmGoal's own doc comments for the full
	// race analysis, identical here. wg.Done for THIS claim is the deferred
	// call above, which fires after both checks below (defers run after the
	// function body's remaining statements), so the WaitGroup never
	// transiently reads zero between this claim's release and a
	// dispatched/auto-armed one's own wg.Add (mirrors runPrompt's
	// defer-at-function-exit shape) — and, unlike a bare call here, still
	// fires even if one of these two calls (or Compact above) panics.
	if !s.maybeDispatchQueued(id, st) {
		s.maybeAutoArmGoal(id, st)
	}

	if err != nil {
		writeErr(w, http.StatusInternalServerError, plugin.SanitizeSessionError(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, compactResponseJSON{
		TurnsFolded: res.TurnsFolded,
		FirstID:     res.FirstID,
		LastID:      res.LastID,
		Summary:     res.Summary,
	})
}

// sessionOnDisk reports whether a session log for id exists in the session
// directory, without loading the session.
func (s *Server) sessionOnDisk(id string) bool {
	if s.opts.SessionDir == "" {
		return false
	}
	infos, err := engine.ListSessions(s.opts.SessionDir)
	if err != nil {
		return false
	}
	for _, info := range infos {
		if info.ID == id {
			return true
		}
	}
	return false
}

// lookup resolves a session for read endpoints: the in-memory session (with
// live status) if present, else a transparent load from disk (always idle).
func (s *Server) lookup(id string) (*engine.Session, string, bool) {
	s.mu.Lock()
	st := s.sessions[id]
	var running bool
	if st != nil {
		running = st.running
	}
	s.mu.Unlock()
	if st != nil {
		return st.sess, statusStr(running), true
	}
	sess, err := s.opts.LoadSession(id)
	if err != nil {
		return nil, "", false
	}
	return sess, "idle", true
}

// residentSession returns the resident *engine.Session for id, or nil if the
// session is not currently resident. Unlike claimForPrompt, this never loads
// from disk and never claims the run slot — it is only used by
// handleGoalBusy, whose caller (handleGoal) reaches it exclusively when
// claimForPrompt just reported id as resident and running.
func (s *Server) residentSession(id string) *engine.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.sessions[id]; st != nil {
		return st.sess
	}
	return nil
}

// currentSeq reads the durable journal's current sequence counter, for a
// response that (unlike claimForPrompt's fromSeq) does not correspond to a
// run-slot claim.
func (s *Server) currentSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// claimForPrompt atomically resolves the session for id and claims its single
// prompt slot. It replaces the old getOrLoad-then-claim two-step on the write
// path, which left a gap between resolving the resident session and setting
// st.running: in that gap a concurrent evictResidentLocked could unload the
// session and a racing cold-load could insert a second, divergent
// *engine.Session for the same log. Here the resolve and the claim complete in
// ONE s.mu critical section, so a claimed (running) session can never be evicted
// (evictResidentLocked skips running sessions) and no duplicate can appear.
//
// Loading an on-disk session may block, so it happens outside the lock; the
// re-lock then re-checks both that no resident appeared meanwhile and that Drain
// has not begun. On success it sets st.running, records the cancel func, and
// does wg.Add(1) — all before releasing the lock. The wg.Add sits in the same
// critical section that observed draining==false, so by mutex ordering it always
// happens-before Drain's draining=true (and thus before wg.Wait): a WaitGroup
// Add after Wait is impossible, and a prompt admitted during drain is impossible.
//
// On failure it returns a non-zero HTTP status and leaves nothing claimed:
// StatusServiceUnavailable (draining), StatusNotFound (unknown session), or
// StatusConflict (already running, or another running session holds the same
// workdir — see workdirHolderLocked — in which case holder names it). code ==
// 0 means success.
//
// A successful claim also resets st.goalLoop to false (see the field's doc
// comment in server.go): the claim site is the natural place for this
// because every occupant that wants goalLoop true sets it only after this
// function returns, so the reset here can never race a legitimate true. This
// makes the flag self-contained rather than relying solely on every prior
// occupant's tail having reset it.
func (s *Server) claimForPrompt(id string) (st *sessionState, ctx context.Context, fromSeq int64, code int, holder string) {
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		return nil, nil, 0, http.StatusServiceUnavailable, ""
	}
	st = s.sessions[id]
	if st == nil {
		// Not resident: load from disk with the lock released, then re-acquire.
		s.mu.Unlock()
		sess, err := s.opts.LoadSession(id)
		if err != nil {
			return nil, nil, 0, http.StatusNotFound, ""
		}
		loaded := &sessionState{sess: sess, lastUsed: time.Now()}
		s.mu.Lock()
		// Drain may have begun during the unlocked load: re-check before we
		// insert or claim, so no wg.Add slips past the admission gate.
		if s.draining {
			s.mu.Unlock()
			return nil, nil, 0, http.StatusServiceUnavailable, ""
		}
		if ex := s.sessions[id]; ex != nil {
			st = ex // a resident appeared while we loaded; use the winner
		} else {
			s.sessions[id] = loaded
			st = loaded
		}
	}
	if st.running {
		s.mu.Unlock()
		return nil, nil, 0, http.StatusConflict, ""
	}
	if h := s.workdirHolderLocked(id, st); h != "" {
		s.mu.Unlock()
		return nil, nil, 0, http.StatusConflict, h
	}
	fromSeq = s.seq
	ctx, cancel := context.WithCancel(context.Background())
	st.running = true
	st.cancel = cancel
	// Reset here too, not just at every prior occupant's tail: this makes the
	// claim self-contained rather than trusting every past and future tail to
	// reset it, and it is always correct because every runGoal-spawning call
	// site below sets it back to true only AFTER claimForPrompt returns (never
	// before), so this can never stomp a legitimate true.
	st.goalLoop = false
	s.wg.Add(1)
	// A cold load grew the resident set; cap it now. st is running, so
	// evictResidentLocked will not evict the session we just claimed.
	s.evictResidentLocked()
	s.mu.Unlock()
	return st, ctx, fromSeq, 0, ""
}

// workdirHolderLocked returns the session ID of another RUNNING session that
// holds the same workdir as st, unless st itself or that other session opted
// into share_workdir, or either is a 'worktree'-isolation session — in which
// case it returns "" (no conflict). The claim exists to stop two sessions
// interleaving writes in one shared tree; a 'worktree' session never shares
// its tree with anything (each gets its own dedicated git worktree, so their
// WorkDir()s can never even be equal), so the claim is moot for it by
// construction — this check is belt-and-suspenders, not load-bearing. Caller
// holds s.mu.
func (s *Server) workdirHolderLocked(id string, st *sessionState) string {
	if st.shareWorkdir || st.isolation == isolationWorktree {
		return ""
	}
	wd := st.sess.WorkDir()
	for otherID, other := range s.sessions {
		if otherID == id || !other.running || other.shareWorkdir || other.isolation == isolationWorktree {
			continue
		}
		if other.sess.WorkDir() == wd {
			return otherID
		}
	}
	return ""
}

// handleEnd ends a session: removes it from residency and, for a
// 'worktree'-isolation session, tears its git worktree down — removed when
// it has no uncommitted changes and no unpushed commits, otherwise kept in
// place with its path journaled (workdir.worktree_kept) so work is never
// destroyed (see teardownWorktree). A busy session is 409 (ripping the
// worktree out from under an in-flight tool call would corrupt whatever it
// is mid-writing — abort it first); unknown (not resident, no log on disk)
// is 404; ending a 'shared' or already-ended session is a plain 204.
func (s *Server) handleEnd(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	st := s.sessions[id]
	if st == nil {
		s.mu.Unlock()
		if !s.sessionOnDisk(id) {
			writeErr(w, http.StatusNotFound, "no such session")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if st.running {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "session is busy; abort it before ending it")
		return
	}
	wt := st.worktree
	delete(s.sessions, id)
	delete(s.lastRequest, id)
	s.mu.Unlock()
	if wt != nil {
		s.teardownWorktree(id, wt)
	}
	w.WriteHeader(http.StatusNoContent)
}

// teardownWorktree decides a 'worktree'-isolation session's fate at session
// end: removed (and evtWorktreeRemoved journaled) when worktreeClean reports
// no uncommitted changes and no unpushed commits; otherwise — including when
// the clean check itself fails, or removal unexpectedly fails after a clean
// read — left exactly where it is and evtWorktreeKept is journaled with its
// path, so an orchestrator polling the event stream can find and finish the
// work instead of losing it.
func (s *Server) teardownWorktree(sessionID string, wt *worktreeInfo) {
	clean, err := worktreeClean(wt.path, wt.baseCommit)
	if err == nil && clean {
		if rmErr := removeWorktree(wt.repoRoot, wt.path); rmErr == nil {
			os.Remove(wt.metaPath)
			s.emitDurable(Event{Type: evtWorktreeRemoved, SessionID: sessionID, WorktreePath: wt.path})
			return
		}
	}
	s.emitDurable(Event{Type: evtWorktreeKept, SessionID: sessionID, WorktreePath: wt.path})
}

// buildSession assembles the Session shape without holding s.mu across engine
// calls: session fields come from the engine, seq from the journal.
func (s *Server) buildSession(sess *engine.Session, status string) sessionJSON {
	id := sess.ID
	s.mu.Lock()
	seq := s.sessionSeqLocked(id)
	goal := goalJSONFrom(s.goalState[id])
	lastTurn := s.lastTurnJSONLocked(id)
	s.mu.Unlock()
	return sessionJSON{
		ID:              id,
		CreatedAt:       sess.CreatedAt(),
		Model:           sess.Model(),
		Status:          status,
		State:           compositeState(status == "busy", goal != nil && goal.Active, forcesIdlePause(goal)),
		Messages:        len(sess.History()),
		Seq:             seq,
		Goal:            goal,
		WorkDir:         sess.WorkDir(),
		LastTurn:        lastTurn,
		Usage:           usageJSONForSession(sess),
		LastActivityAt:  sess.LastActivityAt(),
		ParentSession:   sess.ParentSession(),
		CompactionCount: sess.CompactionCount(),
		LastCompactedAt: sess.LastCompactedAt(),
		Queued:          len(sess.QueuedPrompts()),
	}
}

// lastTurnJSONLocked builds the Session.last_turn / StatusEntry.last_turn
// shape from s.lastTurn, or nil if no turn has finished for id in this
// process. Caller holds s.mu.
func (s *Server) lastTurnJSONLocked(id string) *lastTurnJSON {
	t := s.lastTurn[id]
	if t == nil {
		return nil
	}
	return &lastTurnJSON{Outcome: t.outcome, Error: t.error}
}

func statusStr(running bool) string {
	if running {
		return "busy"
	}
	return "idle"
}

// decodeBody decodes an optional JSON request body into v. An absent body is
// not an error (v keeps its zero value).
func decodeBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}
