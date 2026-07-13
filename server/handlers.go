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
// restartPaused (see goalTracker.pauseView) OVERRIDES all of that to idle:
// a goal restored from the journal at boot with no loop attached is not
// "goal-running" in any sense an operator or composer can act on — it will
// never progress on its own, and "busy"/"goal-running" forever is exactly
// the operator trap this field exists to close (see docs/design/
// fleet-model.md's ADOPT lifecycle). A provider-backoff pause deliberately
// does NOT take this path — its loop is genuinely alive and running, just
// waiting out provider weather, so it keeps reading goal-running (see
// TestGoalStalledProviderBackoffSurfacesPaused).
func compositeState(running, goalActive, restartPaused bool) string {
	switch {
	case restartPaused:
		return "idle"
	case goalActive:
		return "goal-running"
	case running:
		return "busy"
	default:
		return "idle"
	}
}

// isRestartPaused reports whether goal represents a boot-time restart pause
// (see goalTracker.pauseView) — the one pause reason that forces
// compositeState to idle. nil-safe.
func isRestartPaused(goal *goalJSON) bool {
	return goal != nil && goal.Paused && goal.PauseReason == pauseReasonRestart
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
	// attached (see pauseArmedGoalsAtBoot); true with "provider-backoff"
	// while the retryable-backoff park machinery (engine/goal.go) waits out
	// provider weather. Both clear on re-arm (POST /session/{id}/goal) or,
	// for provider-backoff, the moment the loop's own retry succeeds.
	Paused      bool   `json:"paused,omitempty"`
	PauseReason string `json:"pause_reason,omitempty"`
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

	sess, err := s.opts.NewSession(body.Model, sessionWorkDir, parentSession)
	if err != nil {
		if wt != nil {
			s.discardWorktree(wt)
		}
		writeErr(w, http.StatusInternalServerError, "cannot create session")
		return
	}
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
	if err := sess.Persist(); err != nil {
		if wt != nil {
			s.discardWorktree(wt)
		}
		writeErr(w, http.StatusInternalServerError, "cannot create session")
		return
	}
	s.mu.Lock()
	s.sessions[sess.ID] = &sessionState{sess: sess, lastUsed: time.Now(), shareWorkdir: body.ShareWorkdir, isolation: isolation, worktree: wt}
	s.evictResidentLocked()
	s.mu.Unlock()

	s.emitDurable(Event{Type: evtSessionCreated, SessionID: sess.ID, Model: sess.Model()})
	writeJSON(w, http.StatusCreated, s.buildSession(sess, "idle"))
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
	return compositeState(running, goal != nil && goal.Active, isRestartPaused(goal))
}

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
			writeErr(w, code, "session is busy with another prompt")
		case code == http.StatusServiceUnavailable:
			writeErr(w, code, "server shutting down")
		default:
			writeErr(w, http.StatusNotFound, "no such session")
		}
		return
	}

	// Explicit model wins over the session's persisted model (CLI -model rule).
	if !body.Model.IsZero() {
		before := st.sess.Model()
		st.sess.SetModel(body.Model)
		if st.sess.Model() != before {
			s.emitDurable(Event{Type: evtModel, SessionID: id, Model: body.Model})
		}
	}
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})

	go s.runPrompt(ctx, id, st, text)
	writeJSON(w, http.StatusAccepted, map[string]int64{"seq": fromSeq})
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
	st.lastUsed = time.Now()
	s.evictResidentLocked()
	s.mu.Unlock()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "idle"})
}

// handleGoal starts a goal loop on a session. Like prompt_async it claims the
// session's single run slot (busy sessions are 409, shutdown is 503) and runs
// PursueGoal on a tracked goroutine under the same wg/drain/abort semantics —
// the goal occupies the session, so a concurrent prompt_async is 409. The
// evaluator model comes from Options.GoalEvaluator (config goal_evaluator_model);
// when unset, goals are rejected with 400.
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
			writeErr(w, code, "session is busy")
		case code == http.StatusServiceUnavailable:
			writeErr(w, code, "server shutting down")
		default:
			writeErr(w, http.StatusNotFound, "no such session")
		}
		return
	}
	// Deliverable 2(c): re-arming a paused/restart goal. claimForPrompt
	// above only 409s on st.running — it knows nothing about goal state —
	// so reaching here with the engine already reporting an active goal
	// means exactly one thing: this session's goal is active with NO loop
	// attached in this process. A genuinely running loop would already have
	// 409'd via st.running above (handleGoal/runGoal hold the claim for the
	// whole PursueGoal call), so this branch is unreachable for a live
	// provider-backoff park — only the boot-time restart pause (see
	// pauseArmedGoalsAtBoot) or an equivalent crash-before-spawn window
	// reaches it. RegisterGoal would error "already active" here, so this
	// resumes the EXISTING condition instead of registering a new one — a
	// mismatched condition is rejected rather than silently resuming the
	// wrong goal or (via PursueGoal's own registered-path check) spuriously
	// reporting "goal cleared".
	condition := body.Condition
	if existing, active := st.sess.ActiveGoal(); active {
		if existing != body.Condition {
			s.mu.Lock()
			st.running = false
			st.cancel = nil
			st.lastUsed = time.Now()
			s.mu.Unlock()
			s.wg.Done()
			writeErr(w, http.StatusConflict, fmt.Sprintf("a different goal is already active: %q", existing))
			return
		}
		condition = existing
		s.mu.Lock()
		if g := s.goalState[id]; g != nil {
			// Reset ALL pause-relevant fold state, mirroring the
			// evtGoalSet fold: if the journal tail before a restart was
			// goal.stalled(retryable, waiting), clearing only
			// pausedRestart leaves pauseView's provider-backoff case
			// firing on a freshly re-armed, genuinely-running goal until
			// its first goal.eval resets waiting.
			g.pausedRestart = false
			g.retryable = false
			g.retryableClass = ""
			g.waiting = false
		}
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
		st.lastUsed = time.Now()
		s.mu.Unlock()
		s.wg.Done()
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})

	go s.runGoal(ctx, id, st, condition, body.MaxTurns)
	writeJSON(w, http.StatusAccepted, map[string]int64{"seq": fromSeq})
}

// runGoal drives one PursueGoal to completion, then flips the session back to
// idle. The loop's context is cancelled only by DELETE /goal (which journals
// goal.cleared BEFORE cancelling, see handleGoalDelete) or by Drain at
// shutdown; a context.Canceled result is therefore a deliberate stop, not a
// failure, and needs no session.error. Any other error is journaled as
// session.error. Message journaling piggybacks on the same syncMessages path
// as runPrompt.
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
	st.lastUsed = time.Now()
	s.evictResidentLocked()
	s.mu.Unlock()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "idle"})
}

// handleGoalDelete cancels an active goal loop: it clears the goal (journaling
// goal.cleared and resetting the engine's goal state), THEN cancels the loop
// context (stopping further turns). Unknown session (not resident, no log on
// disk) is 404; a known session is 204 whether or not a goal was active
// (idempotent — no goal.cleared is journaled when nothing was active).
//
// Ordering guarantee: goal.cleared is always journaled before the
// session.status idle record that ends that goal's occupancy (see runGoal and
// engine.Session.ClearGoal). This is why clear happens before cancel, not
// after: cancelling first would let the goal-loop worker's context-
// cancellation unwind — which ends in that terminal idle record — race the
// handler to the journal, and an SSE collector that reads until idle (the
// wire contract every client relies on) could see goal.set but never
// goal.cleared.
func (s *Server) handleGoalDelete(w http.ResponseWriter, r *http.Request) {
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
	if st != nil {
		// ClearGoal journals goal.cleared (via OnEvent -> publishGoal) and
		// resets the engine goal state; a no-op when no goal is active.
		st.sess.ClearGoal()
	}
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
	st.lastUsed = time.Now()
	s.evictResidentLocked()
	s.mu.Unlock()
	s.wg.Done()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "idle"})

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
		State:           compositeState(status == "busy", goal != nil && goal.Active, isRestartPaused(goal)),
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
