package server

import (
	"context"
	"encoding/json"
	"errors"
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
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.opts.Version})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model message.ModelRef `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sess, err := s.opts.NewSession(body.Model)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot create session")
		return
	}
	s.mu.Lock()
	s.sessions[sess.ID] = &sessionState{sess: sess, lastUsed: time.Now()}
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

	st, ok := s.getOrLoad(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}

	// Claim the prompt slot before doing anything observable: only one prompt
	// per session at a time.
	s.mu.Lock()
	if st.running {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "session is busy with another prompt")
		return
	}
	fromSeq := s.seq
	ctx, cancel := context.WithCancel(context.Background())
	st.running = true
	st.cancel = cancel
	s.mu.Unlock()

	// Explicit model wins over the session's persisted model (CLI -model rule).
	if !body.Model.IsZero() {
		before := st.sess.Model()
		st.sess.SetModel(body.Model)
		if st.sess.Model() != before {
			s.emitDurable(Event{Type: evtModel, SessionID: id, Model: body.Model})
		}
	}
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})

	s.wg.Add(1)
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

// getOrLoad returns the in-memory session state for id, transparently loading
// and registering an on-disk session when it is not resident.
func (s *Server) getOrLoad(id string) (*sessionState, bool) {
	s.mu.Lock()
	if st := s.sessions[id]; st != nil {
		s.mu.Unlock()
		return st, true
	}
	s.mu.Unlock()

	sess, err := s.opts.LoadSession(id)
	if err != nil {
		return nil, false
	}
	st := &sessionState{sess: sess, lastUsed: time.Now()}
	s.mu.Lock()
	if ex := s.sessions[id]; ex != nil {
		st = ex // lost a race; use the winner
	} else {
		s.sessions[id] = st
	}
	s.mu.Unlock()
	return st, true
}

// buildSession assembles the Session shape without holding s.mu across engine
// calls: session fields come from the engine, seq from the journal.
func (s *Server) buildSession(sess *engine.Session, status string) sessionJSON {
	id := sess.ID
	s.mu.Lock()
	seq := s.sessionSeqLocked(id)
	s.mu.Unlock()
	return sessionJSON{
		ID:        id,
		CreatedAt: sess.CreatedAt(),
		Model:     sess.Model(),
		Status:    status,
		Messages:  len(sess.History()),
		Seq:       seq,
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
