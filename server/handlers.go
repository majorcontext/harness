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
	// State is the unambiguous composite: idle, busy, goal-running, or
	// awaiting-input. Kept alongside Status (never replacing it) for
	// backward compat. Precedence: awaiting-input outranks everything else
	// — a pending ask_user question is not "idle" in the sense of nothing
	// to do, regardless of the goal/busy bits underneath it — then
	// goal-running wins whenever a goal is active, REGARDLESS of the
	// momentary worker status — including the goal loop's between-turn gap
	// (worker finished a turn, evaluator hasn't answered yet) where Status
	// alone would still read "busy" (the server's busy/idle transition
	// brackets the WHOLE PursueGoal call, not each turn) but a naive
	// orchestrator inferring progress from "status=idle plus goal.active"
	// elsewhere is exactly the ambiguity this field exists to remove. See
	// compositeState.
	State    string    `json:"state"`
	Messages int       `json:"messages"`
	Seq      int64     `json:"seq,omitempty"`
	Goal     *goalJSON `json:"goal,omitempty"`
	WorkDir  string    `json:"workdir"`
	// Question is the pending ask_user question, present exactly when State
	// is "awaiting-input" (docs/design/question-tool.md §3).
	Question *questionJSON `json:"question,omitempty"`
	// LastTurn is the most recent prompt or goal-worker turn's outcome for
	// this process — "completed" or "error", plus the sanitized error detail
	// on failure — so a poller can distinguish "idle because done" from
	// "idle because the turn died" without inferring it from message part
	// shapes. Present only once a turn has finished in this process (like
	// Goal, absent on a freshly reloaded, never-prompted-here session).
	LastTurn *lastTurnJSON `json:"last_turn,omitempty"`
}

// questionJSON is the Session.question sub-object: the pending ask_user
// question, present exactly when State is "awaiting-input" (design doc §3).
type questionJSON struct {
	CallID    string                `json:"call_id"`
	Questions []plugin.QuestionItem `json:"questions"`
}

// lastTurnJSON is the openapi LastTurn shape.
type lastTurnJSON struct {
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// compositeState resolves the unambiguous Session.state field: awaiting-input
// ranks above everything else whenever a question is pending (design doc
// §3), then goal-running whenever a goal is active, regardless of the
// momentary running/busy flag (see sessionJSON.State's doc comment for why
// momentary busy/idle is not enough); otherwise busy or idle mirroring the
// plain status.
func compositeState(running, goalActive, awaitingQuestion bool) string {
	switch {
	case awaitingQuestion:
		return "awaiting-input"
	case goalActive:
		return "goal-running"
	case running:
		return "busy"
	default:
		return "idle"
	}
}

// goalJSON is the Session.goal sub-object: present only when a goal has been
// set for the session in this process.
type goalJSON struct {
	Condition  string `json:"condition"`
	Active     bool   `json:"active"`
	Achieved   bool   `json:"achieved,omitempty"`
	Turns      int    `json:"turns"`
	LastReason string `json:"last_reason,omitempty"`
	Attempt    int    `json:"attempt,omitempty"`
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

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model        message.ModelRef `json:"model"`
		WorkDir      string           `json:"workdir"`
		ShareWorkdir bool             `json:"share_workdir"`
		// WorkdirIsolation is "shared" (default, omitted/empty) or
		// "worktree" — see createWorktreeForSession and workdirHolderLocked.
		WorkdirIsolation string `json:"workdir_isolation"`
	}
	if err := decodeBody(r, &body); err != nil {
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

	sess, err := s.opts.NewSession(body.Model, sessionWorkDir)
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
	}
	result := map[string]entry{}

	type snap struct {
		id      string
		running bool
	}
	s.mu.Lock()
	mem := make([]snap, 0, len(s.sessions))
	for id, st := range s.sessions {
		mem = append(mem, snap{id, st.running})
	}
	s.mu.Unlock()
	for _, m := range mem {
		result[m.id] = entry{Type: statusStr(m.running), State: s.compositeStateFor(m.id, m.running), LastTurn: s.lastTurnFor(m.id)}
	}
	infos, err := engine.ListSessions(s.opts.SessionDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot list sessions")
		return
	}
	for _, info := range infos {
		if _, ok := result[info.ID]; !ok {
			result[info.ID] = entry{Type: "idle", State: s.compositeStateFor(info.ID, false), LastTurn: s.lastTurnFor(info.ID)}
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
	g := s.goalState[id]
	q := s.questionState[id]
	return compositeState(running, g != nil && g.active, q != nil && q.pending)
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
		writeClaimError(w, code, holder)
		return
	}

	// A goal paused on a pending ask_user question owns this session — see
	// docs/design/question-tool.md §3. The run slot IS free (PursueGoal
	// already returned when it paused), so claimForPrompt above succeeded;
	// an unguarded prompt_async here would consume the answer as an
	// ordinary user message without ever resuming the goal, leaving
	// goalActive set with nothing driving it (a zombie pause). Undo the
	// claim (mirrors claimForPrompt's own tail bookkeeping) and 409,
	// naming POST /answer as the one write that resumes it.
	if _, awaiting := st.sess.AwaitingQuestion(); awaiting {
		if _, goalActive := st.sess.ActiveGoal(); goalActive {
			s.undoClaim(st)
			writeErr(w, http.StatusConflict, "a goal is paused awaiting an answer; use POST /session/{id}/answer to resume it, not prompt_async")
			return
		}
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

// undoClaim releases a run-slot claim taken by claimForPrompt without ever
// letting the claimed session run — mirrors claimForPrompt's own tail
// bookkeeping (running/cancel/lastUsed, wg.Done). Used by handlers that
// must peek at post-claim session state before deciding whether to
// actually run anything: handlePrompt's awaiting-question/goal-active
// guard above, and handleGoal's RegisterGoal-error unwind and
// handleAnswer's goal-paused branch below.
func (s *Server) undoClaim(st *sessionState) {
	s.mu.Lock()
	st.running = false
	st.cancel = nil
	st.lastUsed = time.Now()
	s.mu.Unlock()
	s.wg.Done()
}

// writeClaimError renders a claimForPrompt failure code the same way every
// claiming endpoint (prompt_async, goal, answer) does, so they report
// identical status/body shapes for the same underlying condition.
func writeClaimError(w http.ResponseWriter, code int, holder string) {
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
}

// answerBody is the POST /session/{id}/answer request shape (design doc §3).
type answerBody struct {
	CallID  string `json:"call_id"`
	Answers []struct {
		Question string   `json:"question"`
		Selected []string `json:"selected,omitempty"`
		Text     string   `json:"text,omitempty"`
	} `json:"answers"`
}

// formatAnswers renders a POST /answer request's answers into the one
// deterministic text block delivered as the next prompt (design doc §3):
// one "Q: .../A: ..." pair per answer, blank-line separated. A Selected
// answer is joined with ", "; otherwise Text is used verbatim (the engine
// enforces no validation that one or the other was actually supplied — see
// the design doc §5, "No answer validation against the schema").
func formatAnswers(answers []struct {
	Question string   `json:"question"`
	Selected []string `json:"selected,omitempty"`
	Text     string   `json:"text,omitempty"`
}) string {
	blocks := make([]string, 0, len(answers))
	for _, a := range answers {
		answer := a.Text
		if len(a.Selected) > 0 {
			answer = strings.Join(a.Selected, ", ")
		}
		blocks = append(blocks, fmt.Sprintf("Q: %s\nA: %s", a.Question, answer))
	}
	return strings.Join(blocks, "\n\n")
}

// handleAnswer answers a pending ask_user question (design doc §3). 404s on
// an unknown session, 409s when nothing is pending, 400s when call_id
// doesn't match the pending question. On success it formats the answers
// into one deterministic text block and delivers it through the same path
// as prompt_async — a plain Session.Prompt user message — except when a
// goal is paused on the question, where PursueGoal's own "not concurrently
// with itself or Prompt" contract forbids calling Prompt directly: that
// branch instead claims the run slot and re-spawns PursueGoal with
// GoalOptions.ResumeAnswer set (see runGoal).
func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	var body answerBody
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.CallID) == "" {
		writeErr(w, http.StatusBadRequest, "call_id must be non-empty")
		return
	}

	sess, _, ok := s.lookup(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	pendingCallID, awaiting := sess.AwaitingQuestion()
	if !awaiting {
		// Nothing is currently pending. Ordinarily that is the plain 409
		// below (no question was ever asked, or it was already fully
		// delivered) — but it is also exactly the shape a crash leaves
		// between a PRIOR /answer's atomic claim persisting
		// question.answered and the resumed goal worker's first Prompt
		// call ever appending a message (design doc §3, issue #64 item 2):
		// the answer is durably recorded (PendingResumeAnswer) and the
		// goal is still active, but nothing ever delivered it. A retried
		// POST /answer for that same question — the natural client
		// response to a request whose connection died before a response
		// ever arrived — recovers it here instead of 409ing on an answer
		// that, in fact, was never lost.
		if s.tryResumeFromPendingAnswer(w, id, sess) {
			return
		}
		writeErr(w, http.StatusConflict, "no question is pending for this session")
		return
	}
	if pendingCallID != body.CallID {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("call_id %q does not match the pending question %q", body.CallID, pendingCallID))
		return
	}
	answerText := formatAnswers(body.Answers)

	if _, goalActive := sess.ActiveGoal(); !goalActive {
		// Interactive branch: deliver the answer as an ordinary prompt_async
		// user message. Session.Prompt's own clear-and-persist
		// (engine's clearAwaitingQuestionOnPrompt) is the SINGLE, idempotent
		// owner of question.answered here — this handler does not persist
		// it itself (design doc §3).
		st, ctx, fromSeq, code, holder := s.claimForPrompt(id)
		if code != 0 {
			writeClaimError(w, code, holder)
			return
		}
		s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})
		go s.runPrompt(ctx, id, st, answerText)
		writeJSON(w, http.StatusAccepted, map[string]int64{"seq": fromSeq})
		return
	}

	// Goal-paused branch: the atomic claim (design doc §3), mirroring
	// claimForPrompt. The run slot is claimed FIRST, not persist-then-claim
	// as the design doc's prose lists the steps: claiming first closes a
	// race the literal order would leave open — ask_user's Run sets
	// s.awaitingQuestion synchronously mid-turn, which is well BEFORE
	// PursueGoal itself returns and runGoal's tail flips st.running back to
	// false, so a question can already read as pending (compositeState
	// ranks awaiting-input above goal-running/busy) while the pausing
	// runGoal goroutine is still finishing its own unwind. Persisting
	// question.answered and clearing the pending state before confirming
	// the run slot is actually free would let that race win — the session
	// claim would then 409, but the answer would already be durably
	// consumed with nothing left to resume it (a zombie pause). Claiming
	// the slot first means a successful claim here happens-after (via
	// server.mu) any prior runGoal's own tail, so the pause is fully
	// settled before AnswerQuestion ever runs. Exactly one concurrent
	// /answer can hold the claim; a loser is rejected by claimForPrompt
	// itself (409/503), never reaching AnswerQuestion at all — the same
	// "exactly one winner, losers 409" guarantee the design doc asks for,
	// achieved without holding the engine and server mutexes nested
	// (server.mu must never wrap a session-mutex-acquiring call — see
	// server.go's lock-ordering invariant), which a literal single lock
	// section spanning both would require.
	st, ctx, fromSeq, code, holder := s.claimForPrompt(id)
	if code != 0 {
		writeClaimError(w, code, holder)
		return
	}
	// From here on, every read and write goes through st.sess — the
	// instance claimForPrompt resolved and made resident — NEVER the `sess`
	// from the handler's early lookup. For a non-resident session those are
	// two distinct engine.Sessions loaded from the same log: answering the
	// throwaway would leave the claimed instance's awaitingQuestion set
	// (its LoadSession ran before question.answered was persisted), and the
	// resumed worker's own clear-on-prompt would then fire a SECOND
	// question.answered — see TestAnswerResumesGoalNonResident.
	won, hadPending := st.sess.AnswerQuestion(body.CallID, answerText)
	if !won {
		// Lost the race for the pending question itself (answered/gone, or
		// a different call_id, between our checks above and here) — release
		// the claim we just took and report the same status a fresh check
		// would have.
		s.undoClaim(st)
		if hadPending {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("call_id %q does not match the pending question", body.CallID))
		} else {
			writeErr(w, http.StatusConflict, "no question is pending for this session")
		}
		return
	}
	condition, stillActive := st.sess.ActiveGoal()
	if !stillActive {
		// The goal was cleared concurrently (e.g. DELETE /goal) in the
		// narrow window between our goalActive read above and
		// AnswerQuestion's claim: the answer is now durably recorded
		// (question.answered, just persisted), but there is nothing left
		// to resume. Release the claim without spawning anything — this is
		// a success, not an error: the answer was recorded, exactly as it
		// would be for any question answered on a session with no active
		// goal.
		s.undoClaim(st)
		writeJSON(w, http.StatusAccepted, map[string]int64{"seq": fromSeq})
		return
	}
	s.mu.Lock()
	maxTurns := s.goalMaxTurns[id]
	s.mu.Unlock()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})
	go s.runGoal(ctx, id, st, condition, maxTurns, answerText)
	writeJSON(w, http.StatusAccepted, map[string]int64{"seq": fromSeq})
}

// tryResumeFromPendingAnswer is handleAnswer's crash-window recovery path
// (design doc §3, issue #64 item 2), attempted only when no question is
// currently pending for sess. If sess carries a PendingResumeAnswer for a
// still-ACTIVE goal — the exact shape a process death leaves between a
// PRIOR /answer's atomic claim persisting question.answered and the resumed
// worker's first Prompt call ever running (engine.Session.PendingResumeAnswer's
// doc comment) — this claims the run slot and re-spawns PursueGoal with that
// recovered text as ResumeAnswer, delivering the same turn-1 directive the
// original resume would have. It does NOT call AnswerQuestion: that record
// is already durably on disk from the answer that actually happened; calling
// it again here would double it (see TestAnswerRecoversPendingResumeAfterCrash).
//
// sess is the handler's early lookup, used only to decide WHETHER recovery
// is possible — every mutation goes through st.sess, the instance
// claimForPrompt resolves, for the same non-resident-instance-identity
// reason the goal-paused branch above always operates on the claimed
// instance (see TestAnswerResumesGoalNonResident): for a non-resident
// session, sess and st.sess are two distinct engine.Sessions loaded from
// the same log.
//
// Returns handled=true once it has written the HTTP response itself
// (a successful resume, or a claim failure); handled=false means the
// conditions for recovery did not hold (no pending answer, no active goal,
// or the race was lost between the check here and the claim) and the
// caller falls back to its own 409.
func (s *Server) tryResumeFromPendingAnswer(w http.ResponseWriter, id string, sess *engine.Session) (handled bool) {
	if _, ok := sess.PendingResumeAnswer(); !ok {
		return false
	}
	if _, goalActive := sess.ActiveGoal(); !goalActive {
		return false
	}
	st, ctx, fromSeq, code, holder := s.claimForPrompt(id)
	if code != 0 {
		writeClaimError(w, code, holder)
		return true
	}
	// Consume, not read: once this path commits to the resume, the pending
	// answer must be gone so a retried /answer cannot re-deliver it (see
	// TakePendingResumeAnswer).
	resumeText, resumeOK := st.sess.TakePendingResumeAnswer()
	condition, goalActive := st.sess.ActiveGoal()
	if !resumeOK || !goalActive {
		// Lost the race (someone else already resumed it, or the goal was
		// cleared) between the check above and this claim: release and let
		// the caller report the same status a fresh check would give.
		s.undoClaim(st)
		return false
	}
	s.mu.Lock()
	maxTurns := s.goalMaxTurns[id]
	s.mu.Unlock()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})
	go s.runGoal(ctx, id, st, condition, maxTurns, resumeText)
	writeJSON(w, http.StatusAccepted, map[string]int64{"seq": fromSeq})
	return true
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
		writeClaimError(w, code, holder)
		return
	}
	// Register the goal synchronously BEFORE the loop goroutine spawns and
	// before the 202 returns: by the time the caller can DELETE, the goal is
	// active and clearable — the accept-vs-clear race is structurally gone.
	if err := st.sess.RegisterGoal(body.Condition); err != nil {
		// Undo the claim taken above: mirror the tail of runPrompt/runGoal.
		s.undoClaim(st)
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})

	s.mu.Lock()
	s.goalMaxTurns[id] = body.MaxTurns
	s.mu.Unlock()
	go s.runGoal(ctx, id, st, body.Condition, body.MaxTurns, "")
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
// PursueGoal returns a nil error with Achieved:false in three cases that are
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
//   - The worker turn ended on a pending ask_user question (Reason
//     "awaiting_input", docs/design/question-tool.md §2): the goal is
//     neither done nor exhausted, it is paused waiting on POST
//     /session/{id}/answer. Recorded as its own outcomeAwaitingInput,
//     inserted BEFORE the MaxTurns catch-all below (Go's switch stops at
//     the first matching case, so it must precede that case or it would be
//     unreachable). Like achieved/cleared, this must not re-arm anything —
//     the pause's continuation belongs entirely to /answer.
//
// The terminal session.status idle record emitted at the end of this
// function is the same record an SSE collector waits for as the session's
// "occupancy over" signal (collect-until-idle is the wire contract). DELETE
// /goal's clear-before-cancel ordering guarantees goal.cleared always
// precedes it in the journal — this function must never emit idle before a
// goal.cleared that is still in flight.
func (s *Server) runGoal(ctx context.Context, id string, st *sessionState, condition string, maxTurns int, resumeAnswer string) {
	defer s.wg.Done()
	res, err := st.sess.PursueGoal(ctx, condition, engine.GoalOptions{
		Registered:   true,
		MaxTurns:     maxTurns,
		Evaluator:    s.opts.GoalEvaluator,
		ResumeAnswer: resumeAnswer,
	})
	s.syncMessages(id)
	switch {
	case err == nil && res.Achieved:
		s.recordTurnEnd(id, "completed", nil)
	case err == nil && res.Reason == "goal cleared":
		// Cleared in flight without the context being cancelled: goal.cleared
		// is already journaled (ClearGoal/handleGoalDelete); no turn.end, same
		// contract as the context.Canceled case below.
	case err == nil && res.Reason == "awaiting_input":
		// Paused on ask_user (see the doc comment above): recorded as its
		// own outcome, inserted before the MaxTurns catch-all so it is
		// never mistaken for it. Nothing here clears the goal or the
		// pending question — question.asked (already journaled by the
		// engine's own publish path, see publishQuestion) is the durable
		// record explaining the pause, exactly as goal.stalled explains a
		// retry.
		s.recordTurnEnd(id, outcomeAwaitingInput, nil)
	case err == nil:
		// Any other nil-error, non-achieved, non-cleared, non-paused result
		// is MaxTurns exhaustion — PursueGoal's only remaining terminal
		// case reachable here (see its doc comment).
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
	var goal *goalJSON
	if g := s.goalState[id]; g != nil {
		goal = &goalJSON{Condition: g.condition, Active: g.active, Achieved: g.achieved, Turns: g.turns, LastReason: g.lastReason, Attempt: g.attempt}
	}
	var question *questionJSON
	if q := s.questionState[id]; q != nil && q.pending {
		question = &questionJSON{CallID: q.callID, Questions: append([]plugin.QuestionItem(nil), q.questions...)}
	}
	lastTurn := s.lastTurnJSONLocked(id)
	s.mu.Unlock()
	return sessionJSON{
		ID:        id,
		CreatedAt: sess.CreatedAt(),
		Model:     sess.Model(),
		Status:    status,
		State:     compositeState(status == "busy", goal != nil && goal.Active, question != nil),
		Messages:  len(sess.History()),
		Seq:       seq,
		Goal:      goal,
		WorkDir:   sess.WorkDir(),
		Question:  question,
		LastTurn:  lastTurn,
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
