package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
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
	}
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
// recorded but never fatal. Caller holds s.mu.
func (s *Server) writeJournalLocked(ev Event) {
	if s.jf == nil {
		return
	}
	b, err := json.Marshal(ev)
	if err != nil {
		s.lastErr = err
		return
	}
	if _, err := s.jf.Write(append(b, '\n')); err != nil {
		s.lastErr = err
	}
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
