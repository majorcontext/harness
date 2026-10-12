package eventlog

import "github.com/majorcontext/harness/protocol"

// Check reports the first of events that would break an invariant if it were
// appended after s. It leaves s unchanged.
func Check(s *State, events []protocol.Payload) error {
	c := s.clone()
	for _, e := range events {
		data, err := Envelope{Seq: c.head + 1, Event: e}.Encode()
		if err != nil {
			return err
		}
		env, err := Decode(data)
		if err != nil {
			return err
		}
		if err := c.apply(env); err != nil {
			return err
		}
	}
	return nil
}
