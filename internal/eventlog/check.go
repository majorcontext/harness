package eventlog

// Check reports the first of events that would break an invariant if it were
// appended after s. It leaves s unchanged.
func Check(s *State, events []Event) error {
	c := s.clone()
	for _, e := range events {
		if err := c.apply(Envelope{Seq: c.head + 1, Event: e}); err != nil {
			return err
		}
	}
	return nil
}
