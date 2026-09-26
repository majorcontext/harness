package engine

import (
	"errors"

	"github.com/majorcontext/harness/message"
)

// commandRecord carries the durable payload of a recCommand record (see
// store.go). Resolved slash commands are journaled beside history, never
// inside it: a command never becomes a recMessage and never reaches a
// provider request (see message.CommandRecord's own doc comment and
// docs/design/slash-commands.md's "Serve-mode resolution" section). Seq is
// set only on the first record of a command whose dispatch was accepted via
// RecordCommandDurable (an /enqueue command) — zero/omitted on every other
// record, including a later status update for the same ID.
type commandRecord struct {
	message.CommandRecord
	Seq int64 `json:"seq,omitempty"`
}

// NewCommandID mints a fresh, time-sortable ID for a new CommandRecord.
func NewCommandID() string { return newID("cmd") }

// foldCommandInto applies the by-ID and torn-seq fold rule shared by every
// command fold: Session's own full-record fold (foldCommand, below) and a
// message page read's lighter head fold (engine/messagepage.go), which folds
// a recCommand record's identity and anchor without its line, args, text, or
// result. One rule, two callers — see foldCommand's own doc comment for what
// the rule means. id and copyAnchor let each caller supply its own type's
// field access; the decision they implement is identical for both.
//
// An existing ID is replaced in place, after copyAnchor moves the existing
// entry's CreatedAt/AfterMessageID onto c: a status update (accepted ->
// succeeded/failed/...) must not move the anchor a reader already resolved
// the command against.
//
// A NEW id carrying seq > 0 that matches an earlier folded entry's own seq
// (via seqs) replaces that entry instead — the same torn-write
// last-writer-wins rule promptQueueFold.queued applies to the prompt queue:
// a failed fsync can leave a torn record on disk whose write reported
// failure, followed by its successful retry under a fresh ID, and live
// memory only ever held the retry's entry. seqs maps a folded command's ID
// to the durable seq its first record carried; each caller owns its own map
// (Session.commandSeqs, and each page-read fold's own local map).
//
// Otherwise c is appended, in first-appearance order.
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

// foldCommand folds one command record into cmds, by ID. See
// foldCommandInto for the rule.
func foldCommand(cmds []message.CommandRecord, seqs map[string]int64, c message.CommandRecord, seq int64) []message.CommandRecord {
	return foldCommandInto(cmds, seqs, c, seq,
		func(r message.CommandRecord) string { return r.ID },
		func(dst *message.CommandRecord, existing message.CommandRecord) {
			dst.CreatedAt = existing.CreatedAt
			dst.AfterMessageID = existing.AfterMessageID
		})
}

// reanchorCommands rewrites every record in cmds whose AfterMessageID names a
// message in history[start:end+1] (the range a fold removes) to summaryID
// instead, in place — UNLESS that same message ID also occurs outside the
// range, in history[:start] or history[end+1:]. Message IDs are not
// guaranteed unique: engine.ResolveMessageID accepts a caller-minted id
// verbatim, so a client retry with the same id can append a second message
// carrying it. A command anchored to a surviving occurrence of an id must
// keep that anchor — the message it names still exists after the fold —
// even though an earlier, folded occurrence of the same id also matches.
// Called at every point that removes a range of durable messages from a
// fold — live compaction (compact.go's Compact), replay (store.go's
// recCompact case), and the message-page fold's own recCompact case
// (messagepage.go) — so a command's anchor stays a message id the
// corresponding fold can still resolve, however many compactions later. A
// record whose AfterMessageID is empty (before every message) never
// matches, since no folded message carries an empty id. Caller passes the
// full pre-splice history; this never mutates it.
func reanchorCommands(cmds []message.CommandRecord, history []message.Message, start, end int, summaryID string) {
	reanchorCommandsInto(cmds, history, start, end, summaryID,
		func(c message.CommandRecord) string { return c.AfterMessageID },
		func(c *message.CommandRecord, v string) { c.AfterMessageID = v })
}

// reanchorCommandsInto is reanchorCommands' rule, shared with a message page
// read's head fold (engine/messagepage.go) the same way foldCommandInto
// shares the fold rule: afterID and setAfterID let each caller supply its
// own type's field access to the identical decision.
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

// findCommandLocked returns id's already-folded record, if any —
// recordCommandLocked's own test for "is this the first record of this
// ID" (CreatedAt/AfterMessageID are minted only then; every later record
// for the same ID copies them from the record this returns, so the
// persisted record, the emitted event, and the fold agree). Caller holds
// s.mu.
func (s *Session) findCommandLocked(id string) (message.CommandRecord, bool) {
	for _, c := range s.commands {
		if c.ID == id {
			return c, true
		}
	}
	return message.CommandRecord{}, false
}

// lastDurableMessageIDLocked is the anchor RecordCommand/RecordCommandDurable
// compute for a command's first record: the ID of the last element of
// s.history for which !message.IsSyntheticOrphanID(id), or "" when history
// holds nothing else. Caller holds s.mu.
func (s *Session) lastDurableMessageIDLocked() string {
	for i := len(s.history) - 1; i >= 0; i-- {
		if !message.IsSyntheticOrphanID(s.history[i].ID) {
			return s.history[i].ID
		}
	}
	return ""
}

// recordCommandLocked is RecordCommand/RecordCommandDurable/
// RepairInterruptedCommands' shared write: stamp timestamps and the anchor,
// persist (unless Config.SessionDir is empty), fold, and — when emit is true
// — emit, all under s.mu, mirroring EnqueuePromptDurable's own persist-then-
// fold-then-emit shape (queue.go). emit is false only for
// RepairInterruptedCommands' boot-time call (see its own doc comment for
// why). Caller holds s.mu.
func (s *Session) recordCommandLocked(c message.CommandRecord, seq int64, emit bool) error {
	now := s.cfg.Now()
	c.UpdatedAt = now
	if existing, ok := s.findCommandLocked(c.ID); ok {
		// A status update for an already-folded ID: copy the original
		// CreatedAt/AfterMessageID/ClientRef onto c before the write and the
		// emit below, so the persisted record, the emitted event, and the
		// fold all agree. ClientRef in particular lets a caller that builds
		// a terminal CommandRecord without it (RepairInterruptedCommands
		// does not carry it forward itself) still inherit the value the
		// accepted record set.
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
		// Emit while still holding s.mu (see EnqueuePrompt in queue.go): keeps
		// event order matching log order under a concurrent read. OnEvent must
		// not call back into this Session — that would deadlock on s.mu, held
		// here.
		s.emit(Event{Type: EventCommand, Command: &cp})
	}
	return nil
}

// RecordCommand journals c beside history (never inside it — see this
// file's own doc comment) and folds it into Session.Commands(), all under
// s.mu. With Config.SessionDir == "" the disk write is skipped; the fold and
// EventCommand emission still happen, matching Session's memory-only mode
// elsewhere.
func (s *Session) RecordCommand(c message.CommandRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordCommandLocked(c, 0, true)
}

// RecordCommandDurable is RecordCommand's durable, idempotent-by-seq
// sibling, for an /enqueue-dispatched command whose caller's own upstream
// ack rides on this call's success — see EnqueuePromptDurable's own doc
// comment (queue.go) for the identical contract this mirrors:
//
//   - seq is a caller-issued, session-monotonic idempotency sequence, drawn
//     from the SAME watermark (Session.enqueueSeq) EnqueuePromptDurable
//     shares with the durable prompt queue. At or below the current
//     high-water mark the call is a clean duplicate no-op: nothing
//     persisted, folded, or emitted.
//   - seq < 1 is a caller error.
//   - Config.SessionDir == "" is an error, unlike RecordCommand's silent
//     memory-only skip: a durable caller's ack contract requires an actual
//     durable write.
//   - On success, the high-water mark advances to seq.
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
	return false, nil
}

// Commands returns a copy of the session's folded command records, in
// first-appearance order (see foldCommand).
func (s *Session) Commands() []message.CommandRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]message.CommandRecord(nil), s.commands...)
}

// RepairInterruptedCommands rewrites every folded command still in the
// CommandAccepted state as CommandInterrupted: a boot-time repair for a
// command whose dispatch never reached a terminal status because the
// process restarted or stopped mid-flight. text renders the per-name
// interrupted message; the caller supplies the boot-vs-drain wording (see
// docs/design/slash-commands.md's "Status and text" table). Returns the
// number of commands repaired. Only boot reconcile calls this.
//
// It persists and folds each repaired record WITHOUT emitting an
// EventCommand: reconcile calls this from inside server.New, a window where
// production's OnEvent closure (cmd/harness/main.go's mkCfg) relies on
// nothing emitting before the server it closes over is assigned. Reconcile's
// own backfill loop journals each folded record it has not already seen, so
// the repair still reaches the durable server journal exactly once.
//
// Holds s.mu across the whole scan-and-repair pass (not just the read), so
// no caller can observe or fold a command between the read and its repair.
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
		n++
	}
	return n, nil
}
