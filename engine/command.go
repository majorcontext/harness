package engine

import (
	"errors"

	"github.com/majorcontext/harness/message"
)

// commandRecord is the durable payload of a recCommand record.
type commandRecord struct {
	message.CommandRecord
	Seq int64 `json:"seq,omitempty"`
}

func NewCommandID() string { return newID("cmd") }

// foldCommandInto is the by-ID and torn-seq fold rule shared by foldCommand
// and the message-page head fold. A new ID carrying seq > 0 matching an
// earlier entry's own seq replaces that entry, so a fsync-failed record's
// retry under a fresh ID does not leave both in the fold.
func foldCommandInto[T any](cmds []T, seqs map[string]int64, c T, seq int64, id func(T) string, copyAnchor func(dst *T, existing T)) []T {
	newID := id(c)
	for i, existing := range cmds {
		if id(existing) == newID {
			copyAnchor(&c, existing)
			cmds[i] = c
			if seq > 0 {
				seqs[newID] = seq
			}
			return cmds
		}
	}
	if seq > 0 {
		for i, existing := range cmds {
			if seqs[id(existing)] == seq {
				cmds = append(cmds[:i], cmds[i+1:]...)
				delete(seqs, id(existing))
				break
			}
		}
		seqs[newID] = seq
	}
	return append(cmds, c)
}

func foldCommand(cmds []message.CommandRecord, seqs map[string]int64, c message.CommandRecord, seq int64) []message.CommandRecord {
	return foldCommandInto(cmds, seqs, c, seq,
		func(r message.CommandRecord) string { return r.ID },
		func(dst *message.CommandRecord, existing message.CommandRecord) {
			dst.CreatedAt = existing.CreatedAt
			dst.AfterMessageID = existing.AfterMessageID
		})
}

// reanchorCommands rewrites every record in cmds whose AfterMessageID names
// a message in history[start:end+1] to summaryID, in place — unless that
// same ID also occurs outside the range (message IDs are not unique).
func reanchorCommands(cmds []message.CommandRecord, history []message.Message, start, end int, summaryID string) {
	reanchorCommandsInto(cmds, history, start, end, summaryID,
		func(c message.CommandRecord) string { return c.AfterMessageID },
		func(c *message.CommandRecord, v string) { c.AfterMessageID = v })
}

func reanchorCommandsInto[T any](cmds []T, history []message.Message, start, end int, summaryID string, afterID func(T) string, setAfterID func(dst *T, v string)) {
	folded := make(map[string]bool, end-start+1)
	for _, m := range history[start : end+1] {
		folded[m.ID] = true
	}
	for _, m := range history[:start] {
		delete(folded, m.ID)
	}
	for _, m := range history[end+1:] {
		delete(folded, m.ID)
	}
	for i := range cmds {
		if folded[afterID(cmds[i])] {
			setAfterID(&cmds[i], summaryID)
		}
	}
}

// Caller holds s.mu.
func (s *Session) findCommandLocked(id string) (message.CommandRecord, bool) {
	for _, c := range s.commands {
		if c.ID == id {
			return c, true
		}
	}
	return message.CommandRecord{}, false
}

// Caller holds s.mu.
func (s *Session) lastDurableMessageIDLocked() string {
	for i := len(s.history) - 1; i >= 0; i-- {
		if !message.IsSyntheticOrphanID(s.history[i].ID) {
			return s.history[i].ID
		}
	}
	return ""
}

// Caller holds s.mu.
func (s *Session) recordCommandLocked(c message.CommandRecord, seq int64, emit bool) error {
	now := s.cfg.Now()
	c.UpdatedAt = now
	if existing, ok := s.findCommandLocked(c.ID); ok {
		// A status update inherits the original CreatedAt/AfterMessageID/ClientRef
		// regardless of what the caller set on c.
		c.CreatedAt = existing.CreatedAt
		c.AfterMessageID = existing.AfterMessageID
		c.ClientRef = existing.ClientRef
	} else {
		c.CreatedAt = now
		c.AfterMessageID = s.lastDurableMessageIDLocked()
	}
	if s.cfg.SessionDir != "" {
		if err := s.ensureLog(); err != nil {
			s.lastPersistErr = err
			return err
		}
		s.flushQueueRecordsLocked()
		rec := record{Type: recCommand, Command: &commandRecord{CommandRecord: c, Seq: seq}}
		if err := s.writeRecord(rec); err != nil {
			s.lastPersistErr = err
			return err
		}
		if !s.volumeSync() {
			if err := s.logFile.Sync(); err != nil {
				s.lastPersistErr = err
				return err
			}
		}
	}
	s.commands = foldCommand(s.commands, s.commandSeqs, c, seq)
	if emit {
		cp := c
		// Emit while still holding s.mu: OnEvent must not call back into
		// this Session, or it deadlocks.
		s.emit(Event{Type: EventCommand, Command: &cp})
	}
	return nil
}

// RecordCommand journals c beside history and folds it into Session.Commands().
func (s *Session) RecordCommand(c message.CommandRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recordCommandLocked(c, 0, true); err != nil {
		return err
	}
	s.maybeSnapshotLocked()
	return nil
}

// RecordCommandDurable is RecordCommand's durable, idempotent-by-seq
// sibling: seq at or below the current high-water mark is a clean duplicate
// no-op. seq < 1 and Config.SessionDir == "" are errors.
func (s *Session) RecordCommandDurable(c message.CommandRecord, seq int64) (duplicate bool, err error) {
	if seq < 1 {
		return false, errors.New("engine: RecordCommandDurable requires seq >= 1")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq <= s.enqueueSeq {
		return true, nil
	}
	if s.cfg.SessionDir == "" {
		return false, errors.New("engine: RecordCommandDurable requires Config.SessionDir")
	}
	if err := s.recordCommandLocked(c, seq, true); err != nil {
		return false, err
	}
	s.enqueueSeq = seq
	s.maybeSnapshotLocked()
	return false, nil
}

func (s *Session) Commands() []message.CommandRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]message.CommandRecord(nil), s.commands...)
}

// RepairInterruptedCommands rewrites every folded command still
// CommandAccepted as CommandInterrupted, a boot-time repair for a dispatch
// that never reached a terminal status. text renders the per-name
// interrupted message. It does not emit an EventCommand: reconcile's own
// backfill loop journals each repaired record instead.
func (s *Session) RepairInterruptedCommands(text func(name string) string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending []message.CommandRecord
	for _, c := range s.commands {
		if c.Status == message.CommandAccepted {
			pending = append(pending, c)
		}
	}
	n := 0
	for _, c := range pending {
		c.Status = message.CommandInterrupted
		c.Text = text(c.Name)
		if err := s.recordCommandLocked(c, 0, false); err != nil {
			return n, err
		}
		s.maybeSnapshotLocked()
		n++
	}
	return n, nil
}
