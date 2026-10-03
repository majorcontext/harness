package eventlog

// Check reports the first of events that would break an invariant if it were
// appended after s. It leaves s unchanged.
func Check(s *State, events []Event) error {
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
