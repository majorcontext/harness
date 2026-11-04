package eventlog

import (
	"errors"

	"github.com/majorcontext/harness/protocol"
)

func (l *LightState) validateCheckpoint() error {
	s := &l.state
	if l.attachments == nil || l.commands == nil {
		return errors.New("invalid checkpoint indexes")
	}
	if s.Head() == 0 {
		if s.created {
			return errors.New("invalid empty checkpoint")
		}
		return nil
	}
	if !s.created || s.model == "" || s.inputs == nil || s.requests == nil || s.turnIDs == nil || s.children == nil || s.backends == nil {
		return errors.New("invalid checkpoint state")
	}
	if s.turnAt < 0 || s.turnAt > len(s.history) {
		return errors.New("invalid checkpoint turn position")
	}
	if err := l.validateProgress(); err != nil {
		return err
	}
	return l.validateSources()
}

func (l *LightState) validateProgress() error {
	s := &l.state
	p := s.prog
	if p.Status != statusOf(p) {
		return errors.New("invalid checkpoint status")
	}
	if s.turn.Suspended && s.turn.ID == "" {
		return errors.New("invalid checkpoint suspended turn")
	}
	turn := s.turn.ID
	if s.turn.Suspended {
		turn = ""
	}
	if p.TurnID != turn || s.turn.ID != "" && !s.turnIDs[s.turn.ID] {
		return errors.New("invalid checkpoint turn")
	}
	if (p.LastTurn == nil) != (s.lastEnded.TurnID == "") {
		return errors.New("invalid checkpoint last turn")
	}
	if p.LastTurn != nil && (p.LastTurn.TurnID != s.lastEnded.TurnID || p.LastTurn.StopReason != s.lastEnded.StopReason || p.LastTurn.Cause != s.lastEnded.Cause || p.LastTurn.Error != s.lastEnded.Error) {
		return errors.New("invalid checkpoint last turn")
	}
	queued := make(map[string]bool, len(p.Queued))
	for _, id := range p.Queued {
		in, ok := s.inputs[id]
		if !ok || queued[id] || in.state != inputAdmitted || in.event.InputID != id {
			return errors.New("invalid checkpoint queue")
		}
		queued[id] = true
	}
	for id, in := range s.inputs {
		if in.state == inputAdmitted && !queued[id] {
			return errors.New("invalid checkpoint input")
		}
		if in.state != inputAdmitted && in.state != inputPromoted && in.state != inputWithdrawn {
			return errors.New("invalid checkpoint input state")
		}
	}
	requests := make(map[string]bool, len(p.Requests))
	for _, id := range p.Requests {
		r, ok := s.requests[id]
		if !ok || requests[id] || r.RequestID != id {
			return errors.New("invalid checkpoint request")
		}
		requests[id] = true
	}
	if len(requests) != len(s.requests) {
		return errors.New("invalid checkpoint requests")
	}
	return nil
}

func (l *LightState) validateSources() error {
	s := &l.state
	valid := func(seq uint64) bool { return seq > 0 && seq <= s.Head() }
	for _, in := range s.inputs {
		if !valid(in.seq) {
			return errors.New("invalid checkpoint input source")
		}
	}
	for _, seq := range l.commands {
		if !valid(seq) {
			return errors.New("invalid checkpoint index source")
		}
	}
	for id, c := range s.commands {
		if !valid(c.seq) || !valid(l.commands[id]) || l.commands[id] < c.seq {
			return errors.New("invalid checkpoint command")
		}
	}
	if len(l.commands) != len(s.commands) {
		return errors.New("invalid checkpoint source index")
	}
	if s.compactedAt != 0 && (!valid(s.compactedAt) || s.compaction.ToSeq == 0 || s.compaction.ToSeq >= s.compactedAt) {
		return errors.New("invalid checkpoint compaction")
	}
	for _, h := range s.history {
		if !validCheckpointEntry(h, s.Head()) {
			return errors.New("invalid checkpoint history")
		}
	}
	for _, h := range s.stranded {
		if !validCheckpointEntry(h, s.Head()) {
			return errors.New("invalid checkpoint stranded history")
		}
	}
	for _, h := range l.folded {
		if h.Summary != 0 {
			if !valid(h.Summary) {
				return errors.New("invalid checkpoint summary")
			}
		} else if !validCheckpointEntry(h.Entry, s.Head()) {
			return errors.New("invalid checkpoint folded history")
		}
	}
	return nil
}

func validCheckpointEntry(e entry, head uint64) bool {
	if e.seq == 0 || e.seq > head || len(e.sources) == 0 || e.msg.Role != protocol.RoleUser && e.msg.Role != protocol.RoleAssistant && e.msg.Role != protocol.RoleTool {
		return false
	}
	for _, seq := range e.sources {
		if seq == 0 || seq > head {
			return false
		}
	}
	return true
}
