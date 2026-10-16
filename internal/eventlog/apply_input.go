package eventlog

import (
	"maps"
	"slices"
	"time"

	"github.com/majorcontext/harness/protocol"
)

func (s *State) inputIs(id string, want inputState) error {
	got := s.inputs[id].state
	if got == want {
		return nil
	}
	if got == "" {
		return illegal("input %s is unknown", id)
	}
	return illegal("input %s is %s", id, got)
}

func (s *State) applyAdmitted(e protocol.InputAdmitted, seq uint64) error {
	if got, ok := s.inputs[e.InputID]; ok {
		return illegal("input %s is %s", e.InputID, got.state)
	}
	if e.InputID == "" {
		return illegal("input.admitted has an empty input_id")
	}
	if _, ok := s.commands[e.InputID]; ok {
		return illegal("input %s is a command", e.InputID)
	}
	if e.Delivery != protocol.DeliveryQueue && e.Delivery != protocol.DeliverySteer {
		return illegal("input %s has delivery %q", e.InputID, e.Delivery)
	}
	s.inputs[e.InputID] = input{state: inputAdmitted, seq: seq, event: e}
	s.continueAt = time.Time{}
	if IsUsageReset(e) {
		s.continued++
	} else {
		s.continued = 0
	}
	return nil
}

// IsUsageReset reports whether in is the input that the runtime admits when a
// usage limit resets. A caller cannot write an engine_context part, so this
// tells it from a prompt that names the source usage_reset.
func IsUsageReset(in protocol.InputAdmitted) bool {
	return in.Source == protocol.SourceUsageReset && slices.ContainsFunc(in.Parts, func(p protocol.LogPart) bool { return p.Type == protocol.LogPartEngineContext })
}

func (s *State) applyPromoted(e protocol.InputPromoted) error {
	if err := s.inputIs(e.InputID, inputAdmitted); err != nil {
		return err
	}
	if err := s.runningTurn(e.TurnID); err != nil {
		return err
	}
	if s.inputs[e.InputID].event.Delivery != protocol.DeliverySteer {
		return illegal("input %s waits for the next turn", e.InputID)
	}
	s.takeInput(e.InputID, inputPromoted)
	s.setInputTurn(e.InputID, e.TurnID)
	return nil
}

func (s *State) applyWithdrawn(e protocol.InputWithdrawn) error {
	if err := s.inputIs(e.InputID, inputAdmitted); err != nil {
		return err
	}
	s.takeInput(e.InputID, inputWithdrawn)
	return nil
}

func (s *State) setInputTurn(id, turnID string) {
	in := s.inputs[id]
	in.turnID = turnID
	s.inputs[id] = in
}

func (s *State) takeInput(id string, to inputState) {
	in := s.inputs[id]
	in.state = to
	s.inputs[id] = in
}

func (s *State) applyCommand(e protocol.CommandRecorded, seq uint64) error {
	if e.InputID == "" || e.Status == "" {
		return illegal("command.recorded has an empty input_id or status")
	}
	if _, ok := s.inputs[e.InputID]; ok {
		return illegal("command %s is an input", e.InputID)
	}
	prior, ok := s.commands[e.InputID]
	if ok && (prior.rec.Status != commandAccepted || e.Status == commandAccepted) {
		return illegal("command %s is %s", e.InputID, prior.rec.Status)
	}
	if !ok {
		prior.seq, prior.after = seq, s.lastMessageID()
	}
	if s.commands == nil {
		s.commands = map[string]command{}
	}
	s.commands[e.InputID] = command{e, prior.seq, prior.after}
	return nil
}

type command struct {
	rec protocol.CommandRecorded
	seq uint64
	// after is the ID of the newest message when the first record was appended.
	after string
}

const commandAccepted = "accepted"

// Command returns the newest record of the command of input id, and the
// seq of its first record.
func (s *State) Command(id string) (protocol.CommandRecorded, uint64, bool) {
	c, ok := s.commands[id]
	return c.rec, c.seq, ok
}

// Unfinished returns each command whose newest status is accepted, by input ID.
func (s *State) Unfinished() []protocol.CommandRecorded {
	var out []protocol.CommandRecorded
	for _, id := range slices.Sorted(maps.Keys(s.commands)) {
		if c := s.commands[id]; c.rec.Status == commandAccepted {
			out = append(out, c.rec)
		}
	}
	return out
}
