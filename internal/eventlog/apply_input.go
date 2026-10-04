package eventlog

import (
	"maps"
	"slices"
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

func (s *State) applyAdmitted(e InputAdmitted, seq uint64) error {
	if got, ok := s.inputs[e.InputID]; ok {
		return illegal("input %s is %s", e.InputID, got.state)
	}
	if e.InputID == "" {
		return illegal("input.admitted has an empty input_id")
	}
	if _, ok := s.commands[e.InputID]; ok {
		return illegal("input %s is a command", e.InputID)
	}
	if e.Delivery != DeliveryQueue && e.Delivery != DeliverySteer {
		return illegal("input %s has delivery %q", e.InputID, e.Delivery)
	}
	if len(s.requests) > 0 {
		return illegal("request %s is open", s.requests[0].RequestID)
	}
	s.inputs[e.InputID] = input{inputAdmitted, seq, e}
	s.queue = append(s.queue, e)
	return nil
}

func (s *State) applyPromoted(e InputPromoted) error {
	if err := s.inputIs(e.InputID, inputAdmitted); err != nil {
		return err
	}
	if err := s.runningTurn(e.TurnID); err != nil {
		return err
	}
	i := slices.IndexFunc(s.queue, func(in InputAdmitted) bool { return in.InputID == e.InputID })
	if s.queue[i].Delivery != DeliverySteer {
		return illegal("input %s waits for the next turn", e.InputID)
	}
	s.takeInput(e.InputID, inputPromoted)
	return nil
}

func (s *State) applyWithdrawn(e InputWithdrawn) error {
	if err := s.inputIs(e.InputID, inputAdmitted); err != nil {
		return err
	}
	s.takeInput(e.InputID, inputWithdrawn)
	return nil
}

func (s *State) takeInput(id string, to inputState) {
	in := s.inputs[id]
	in.state = to
	s.inputs[id] = in
	s.queue = slices.DeleteFunc(s.queue, func(in InputAdmitted) bool { return in.InputID == id })
}

func (s *State) applyCommand(e CommandRecorded, seq uint64) error {
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
		prior.seq = seq
	}
	if s.commands == nil {
		s.commands = map[string]command{}
	}
	s.commands[e.InputID] = command{e, prior.seq}
	return nil
}

type command struct {
	rec CommandRecorded
	seq uint64
}

const commandAccepted = "accepted"

// Command returns the newest record of the command of input id, and the
// seq of its first record.
func (s *State) Command(id string) (CommandRecorded, uint64, bool) {
	c, ok := s.commands[id]
	return c.rec, c.seq, ok
}

// Unfinished returns each command whose newest status is accepted, by input ID.
func (s *State) Unfinished() []CommandRecorded {
	var out []CommandRecorded
	for _, id := range slices.Sorted(maps.Keys(s.commands)) {
		if c := s.commands[id]; c.rec.Status == commandAccepted {
			out = append(out, c.rec)
		}
	}
	return out
}
