package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// Event is one server-sent event, matching the openapi Event schema. Durable
// records carry a non-zero Seq and are journaled and replayable; live events
// have no Seq and stream only while connected.
type Event struct {
	Type      string            `json:"type"`
	SessionID string            `json:"session_id"`
	Seq       int64             `json:"seq,omitempty"`
	Status    string            `json:"status,omitempty"`
	Message   *message.Message  `json:"message,omitempty"`
	Model     message.ModelRef  `json:"model,omitzero"`
	Text      string            `json:"text,omitempty"`
	ToolCall  *message.ToolCall `json:"tool_call,omitempty"`
	Output    message.Parts     `json:"output,omitempty"`
	IsError   bool              `json:"is_error,omitempty"`
	Error     string            `json:"error,omitempty"`

	// request.meta fields: a durable, replayable record of the assembled model
	// request. SystemHash fingerprints the joined system segments; the full
	// System array is included only when the hash differs from the session's
	// previous request (the hash-on-change trick), so unchanged prompts stay
	// cheap. SystemLen is the byte length of the joined system, Segments the
	// count, Tools the offered tool names, and Messages the history length.
	SystemHash string   `json:"system_hash,omitempty"`
	SystemLen  int      `json:"system_len,omitempty"`
	Segments   int      `json:"segments,omitempty"`
	Tools      []string `json:"tools,omitempty"`
	Messages   int      `json:"messages,omitempty"`
	System     []string `json:"system,omitempty"`

	// Goal-loop fields, carried by the goal.* durable records (see goal.go).
	GoalCondition string `json:"goal_condition,omitempty"`
	GoalReason    string `json:"goal_reason,omitempty"`
	GoalMet       bool   `json:"goal_met,omitempty"`
	GoalTurn      int    `json:"goal_turn,omitempty"`
	GoalTurns     int    `json:"goal_turns,omitempty"`
}

// Durable and live event types (a superset of engine.Event types plus the
// server-owned lifecycle records).
const (
	evtSessionCreated = "session.created"
	evtSessionStatus  = "session.status"
	evtSessionError   = "session.error"
	evtSessionAborted = "session.aborted"
	evtMessage        = "message"
	evtModel          = "model"
	evtRequestMeta    = "request.meta"
	evtGoalSet        = "goal.set"
	evtGoalEval       = "goal.eval"
	evtGoalAchieved   = "goal.achieved"
	evtGoalCleared    = "goal.cleared"
)

const journalName = "events.jsonl"

// Publish maps an engine event onto the journal/SSE stream. Message events
// trigger a journal sync (which durably records every new canonical message,
// including the user and tool messages the engine does not surface via
// OnEvent); the live deltas fan out to connected clients only.
func (s *Server) Publish(ev engine.Event) {
	switch ev.Type {
	case engine.EventMessage:
		s.syncMessages(ev.SessionID)
	case engine.EventTextDelta:
		s.publishLive(Event{Type: engine.EventTextDelta, SessionID: ev.SessionID, Text: ev.Text})
	case engine.EventReasoningDelta:
		s.publishLive(Event{Type: engine.EventReasoningDelta, SessionID: ev.SessionID, Text: ev.Text})
	case engine.EventToolStart:
		s.publishLive(Event{Type: engine.EventToolStart, SessionID: ev.SessionID, ToolCall: ev.ToolCall})
	case engine.EventToolEnd:
		s.publishLive(Event{
			Type: engine.EventToolEnd, SessionID: ev.SessionID,
			ToolCall: ev.ToolCall, Output: ev.Output, IsError: ev.IsError,
		})
	case engine.EventGoalSet, engine.EventGoalEval, engine.EventGoalAchieved, engine.EventGoalCleared:
		s.publishGoal(ev)
	}
}

// publishGoal journals a durable goal.* record and folds the event into the
// per-session goal tracker that backs the Session JSON goal field.
func (s *Server) publishGoal(ev engine.Event) {
	out := &Event{
		Type:          ev.Type,
		SessionID:     ev.SessionID,
		GoalCondition: ev.GoalCondition,
		GoalReason:    ev.GoalReason,
		GoalMet:       ev.GoalMet,
		GoalTurn:      ev.GoalTurn,
		GoalTurns:     ev.GoalTurns,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.goalState[ev.SessionID]
	if g == nil {
		g = &goalTracker{}
		s.goalState[ev.SessionID] = g
	}
	switch ev.Type {
	case engine.EventGoalSet:
		g.condition = ev.GoalCondition
		g.active = true
		g.achieved = false
		g.turns = 0
		g.lastReason = ""
	case engine.EventGoalEval:
		g.turns = ev.GoalTurn
		g.lastReason = ev.GoalReason
	case engine.EventGoalAchieved:
		g.active = false
		g.achieved = true
		g.turns = ev.GoalTurns
		g.lastReason = ev.GoalReason
	case engine.EventGoalCleared:
		g.active = false
	}
	s.emitDurableLocked(out)
}

// requestSnapshot is the latest fully-assembled model request for a session,
// held in memory only (never persisted) to answer GET /session/{id}/request.
type requestSnapshot struct {
	model       message.ModelRef
	system      []string
	tools       []string
	messages    int
	temperature *float64
	topP        *float64
	maxTokens   int
}

// OnRequest is the engine's OnRequest callback (wired per session at
// construction). It runs synchronously in the engine's prompt goroutine with
// the exact final request about to hit the provider. It journals a durable
// request.meta record — including the full system segments only when their hash
// differs from this session's previous request — and stashes the latest
// assembled request in memory for the /request endpoint. It does not mutate req.
func (s *Server) OnRequest(sessionID string, _ int, req *provider.Request) {
	tools := make([]string, len(req.Tools))
	for i, td := range req.Tools {
		tools[i] = td.Name
	}
	sort.Strings(tools)

	joined := strings.Join(req.System, "\n")
	sum := sha256.Sum256([]byte(joined))
	hash := hex.EncodeToString(sum[:])
	system := append([]string(nil), req.System...)

	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.lastReqHash[sessionID] != hash
	s.lastReqHash[sessionID] = hash
	s.lastRequest[sessionID] = &requestSnapshot{
		model:       req.Model,
		system:      system,
		tools:       tools,
		messages:    len(req.Messages),
		temperature: req.Temperature,
		topP:        req.TopP,
		maxTokens:   req.MaxTokens,
	}
	ev := &Event{
		Type:       evtRequestMeta,
		SessionID:  sessionID,
		Model:      req.Model,
		SystemHash: hash,
		SystemLen:  len(joined),
		Segments:   len(req.System),
		Tools:      tools,
		Messages:   len(req.Messages),
	}
	if changed {
		ev.System = system
	}
	s.emitDurableLocked(ev)
}

// syncMessages appends a durable message record for every message in the
// session's history not yet journaled, in order. It is the single journaling
// path for messages — used live (on each assistant turn) and at boot
// (reconcile) — so the "journal mirrors the session log" invariant cannot
// drift. It is idempotent: already-journaled message IDs are skipped.
func (s *Server) syncMessages(sessionID string) {
	s.mu.Lock()
	st := s.sessions[sessionID]
	s.mu.Unlock()
	if st == nil {
		return
	}
	history := st.sess.History()

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range history {
		m := history[i]
		if s.isSeenLocked(sessionID, m.ID) {
			continue
		}
		s.markSeenLocked(sessionID, m.ID)
		s.emitDurableLocked(&Event{Type: evtMessage, SessionID: sessionID, Message: &m})
	}
	s.checkPersistErrLocked(sessionID, st.sess)
}

// emitDurable assigns the next sequence number, journals the event, and fans it
// out to connected clients.
func (s *Server) emitDurable(ev Event) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitDurableLocked(&ev)
	return ev.Seq
}

// emitDurableLocked is emitDurable with s.mu held.
func (s *Server) emitDurableLocked(ev *Event) {
	s.seq++
	ev.Seq = s.seq
	s.writeJournalLocked(*ev)
	s.journal = append(s.journal, *ev)
	s.fanoutLocked(*ev)
}

// publishLive fans a non-durable event out to clients without journaling it.
func (s *Server) publishLive(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fanoutLocked(ev)
}

// fanoutLocked delivers ev to every matching subscriber. A slow subscriber's
// event is dropped rather than blocking the publisher; that client can
// reconnect with from=<last seq> to recover durable records. Caller holds s.mu.
func (s *Server) fanoutLocked(ev Event) {
	for sub := range s.subs {
		if sub.session != "" && sub.session != ev.SessionID {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
		}
	}
}

// writeJournalLocked appends one record to events.jsonl. Write failures are
// recorded in s.lastErr and, when Options.OnError is set, forwarded (wrapped
// with "journal write: %w") — never fatal either way. Caller holds s.mu.
func (s *Server) writeJournalLocked(ev Event) {
	if s.jf == nil {
		return
	}
	b, err := json.Marshal(ev)
	if err != nil {
		s.lastErr = err
		s.reportError(fmt.Errorf("journal write: %w", err))
		return
	}
	if _, err := s.jf.Write(append(b, '\n')); err != nil {
		s.lastErr = err
		s.reportError(fmt.Errorf("journal write: %w", err))
	}
}

// checkPersistErrLocked polls sess.PersistErr() and forwards it to
// Options.OnError (wrapped with "session %s persist: %w") the first time it
// appears for this session or changes from the last-forwarded error, so a
// persistently-failing write is reported once rather than on every
// syncMessages call. Caller holds s.mu.
func (s *Server) checkPersistErrLocked(sessionID string, sess *engine.Session) {
	err := sess.PersistErr()
	if err == nil {
		return
	}
	msg := err.Error()
	if s.lastPersistErr[sessionID] == msg {
		return
	}
	s.lastPersistErr[sessionID] = msg
	s.reportError(fmt.Errorf("session %s persist: %w", sessionID, err))
}

func (s *Server) markSeenLocked(sessionID, msgID string) {
	m := s.seen[sessionID]
	if m == nil {
		m = make(map[string]bool)
		s.seen[sessionID] = m
	}
	m[msgID] = true
}

func (s *Server) isSeenLocked(sessionID, msgID string) bool {
	return s.seen[sessionID][msgID]
}

// sessionSeqLocked returns the highest durable seq recorded for a session, or 0.
// Caller holds s.mu.
func (s *Server) sessionSeqLocked(sessionID string) int64 {
	var max int64
	for _, ev := range s.journal {
		if ev.SessionID == sessionID && ev.Seq > max {
			max = ev.Seq
		}
	}
	return max
}

// reconcile loads the existing journal into memory, then appends message
// records for any session-log message missing from it — the crash-recovery
// path for a process that died between the engine's session-log append and the
// server's journal append. Runs once, at New, before any client connects.
func (s *Server) reconcile() error {
	if s.opts.SessionDir == "" {
		return nil // in-memory only; nothing to reconcile
	}
	path := filepath.Join(s.opts.SessionDir, journalName)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	s.loadJournal(data)

	if err := os.MkdirAll(s.opts.SessionDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	s.jf = f

	infos, err := engine.ListSessions(s.opts.SessionDir)
	if err != nil {
		return err
	}
	for _, info := range infos {
		sess, err := s.opts.LoadSession(info.ID)
		if err != nil {
			continue // unreadable log: skip, do not fail the whole boot
		}
		history := sess.History()
		for i := range history {
			m := history[i]
			if s.isSeenLocked(info.ID, m.ID) {
				continue
			}
			s.markSeenLocked(info.ID, m.ID)
			s.emitDurableLocked(&Event{Type: evtMessage, SessionID: info.ID, Message: &m})
		}
	}
	return nil
}

// loadJournal parses an existing events.jsonl into memory: replay buffer,
// highest seq, and the seen-message index. A truncated final line (crash
// mid-write) is tolerated.
func (s *Server) loadJournal(data []byte) {
	lines := bytes.Split(data, []byte("\n"))
	for i, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			if i == len(lines)-1 {
				break // truncated final line: crash mid-write, ignore
			}
			continue
		}
		s.journal = append(s.journal, ev)
		if ev.Seq > s.seq {
			s.seq = ev.Seq
		}
		if ev.Type == evtMessage && ev.Message != nil {
			s.markSeenLocked(ev.SessionID, ev.Message.ID)
		}
	}
}
