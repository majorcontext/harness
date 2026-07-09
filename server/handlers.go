package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

// sessionJSON is the openapi Session shape.
type sessionJSON struct {
	ID        string           `json:"id"`
	CreatedAt time.Time        `json:"created_at"`
	Model     message.ModelRef `json:"model"`
	Status    string           `json:"status"`
	Messages  int              `json:"messages"`
	Seq       int64            `json:"seq,omitempty"`
	Goal      *goalJSON        `json:"goal,omitempty"`
	WorkDir   string           `json:"workdir"`
}

// goalJSON is the Session.goal sub-object: present only when a goal has been
// set for the session in this process.
type goalJSON struct {
	Condition  string `json:"condition"`
	Active     bool   `json:"active"`
	Achieved   bool   `json:"achieved,omitempty"`
	Turns      int    `json:"turns"`
	LastReason string `json:"last_reason,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.opts.Version})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model        message.ModelRef `json:"model"`
		WorkDir      string           `json:"workdir"`
		ShareWorkdir bool             `json:"share_workdir"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	workDir, err := resolveWorkDir(s.opts.WorkspaceRoots, body.WorkDir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sess, err := s.opts.NewSession(body.Model, workDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot create session")
		return
	}
	// Persist the log now so the session has durable state even if it is
	// evicted before its first prompt; otherwise eviction below would drop a
	// never-prompted session with no on-disk backing to reload from.
	if err := sess.Persist(); err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot create session")
		return
	}
	s.mu.Lock()
	s.sessions[sess.ID] = &sessionState{sess: sess, lastUsed: time.Now(), shareWorkdir: body.ShareWorkdir}
	s.evictResidentLocked()
	s.mu.Unlock()

	s.emitDurable(Event{Type: evtSessionCreated, SessionID: sess.ID, Model: sess.Model()})
	writeJSON(w, http.StatusCreated, s.buildSession(sess, "idle"))
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
	sess, status, ok := s.lookup(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	writeJSON(w, http.StatusOK, s.buildSession(sess, status))
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.lookup(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	msgs := sess.History()
	if msgs == nil {
		msgs = []message.Message{}
	}
	writeJSON(w, http.StatusOK, msgs)
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
	id := r.PathValue("id")
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
		Type string `json:"type"`
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
		result[m.id] = entry{Type: statusStr(m.running)}
	}
	infos, err := engine.ListSessions(s.opts.SessionDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot list sessions")
		return
	}
	for _, info := range infos {
		if _, ok := result[info.ID]; !ok {
			result[info.ID] = entry{Type: "idle"}
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
	case errors.Is(err, context.Canceled):
		s.emitDurable(Event{Type: evtSessionAborted, SessionID: id})
	default:
		s.emitDurable(Event{Type: evtSessionError, SessionID: id, Error: err.Error()})
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
	id := r.PathValue("id")
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
	// Register the goal synchronously BEFORE the loop goroutine spawns and
	// before the 202 returns: by the time the caller can DELETE, the goal is
	// active and clearable — the accept-vs-clear race is structurally gone.
	if err := st.sess.RegisterGoal(body.Condition); err != nil {
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

	go s.runGoal(ctx, id, st, body.Condition, body.MaxTurns)
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
// The terminal session.status idle record emitted at the end of this
// function is the same record an SSE collector waits for as the session's
// "occupancy over" signal (collect-until-idle is the wire contract). DELETE
// /goal's clear-before-cancel ordering guarantees goal.cleared always
// precedes it in the journal — this function must never emit idle before a
// goal.cleared that is still in flight.
func (s *Server) runGoal(ctx context.Context, id string, st *sessionState, condition string, maxTurns int) {
	defer s.wg.Done()
	_, err := st.sess.PursueGoal(ctx, condition, engine.GoalOptions{
		Registered: true,
		MaxTurns:   maxTurns,
		Evaluator:  s.opts.GoalEvaluator,
	})
	s.syncMessages(id)
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		// Cleared via DELETE (goal.cleared already journaled) or drained.
	default:
		s.emitDurable(Event{Type: evtSessionError, SessionID: id, Error: err.Error()})
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
	id := r.PathValue("id")
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
	id := r.PathValue("id")
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
// into share_workdir — in which case it returns "" (no conflict). Caller
// holds s.mu.
func (s *Server) workdirHolderLocked(id string, st *sessionState) string {
	if st.shareWorkdir {
		return ""
	}
	wd := st.sess.WorkDir()
	for otherID, other := range s.sessions {
		if otherID == id || !other.running || other.shareWorkdir {
			continue
		}
		if other.sess.WorkDir() == wd {
			return otherID
		}
	}
	return ""
}

// buildSession assembles the Session shape without holding s.mu across engine
// calls: session fields come from the engine, seq from the journal.
func (s *Server) buildSession(sess *engine.Session, status string) sessionJSON {
	id := sess.ID
	s.mu.Lock()
	seq := s.sessionSeqLocked(id)
	var goal *goalJSON
	if g := s.goalState[id]; g != nil {
		goal = &goalJSON{Condition: g.condition, Active: g.active, Achieved: g.achieved, Turns: g.turns, LastReason: g.lastReason}
	}
	s.mu.Unlock()
	return sessionJSON{
		ID:        id,
		CreatedAt: sess.CreatedAt(),
		Model:     sess.Model(),
		Status:    status,
		Messages:  len(sess.History()),
		Seq:       seq,
		Goal:      goal,
		WorkDir:   sess.WorkDir(),
	}
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
