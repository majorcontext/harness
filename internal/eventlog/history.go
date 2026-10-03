package eventlog

import "slices"

type entry struct {
	seq uint64
	msg Message
}

// History returns the conversation that the model sees: the summary of the
// newest compaction as a user message, then each later message in log order.
func (s *State) History() []Message {
	var out []Message
	if c, ok := s.Compaction(); ok {
		out = append(out, Message{Role: RoleUser, Parts: []Part{{Type: PartText, Text: c.Summary}}})
	}
	for _, e := range s.history {
		out = append(out, Message{Role: e.msg.Role, Parts: cloneParts(e.msg.Parts)})
	}
	return out
}

// remember records the messages of an applied event.
func (s *State) remember(env Envelope) {
	switch e := env.Event.(type) {
	case TurnStarted:
		for _, id := range e.InputIDs {
			s.say(env.Seq, Message{Role: RoleUser, Parts: s.inputs[id].event.Parts})
		}
	case InputPromoted:
		s.say(env.Seq, Message{Role: RoleUser, Parts: s.inputs[e.InputID].event.Parts})
	case ItemCompleted:
		s.say(env.Seq, e.Message)
	case CompactionApplied:
		i := slices.IndexFunc(s.history, func(h entry) bool { return h.seq > e.ToSeq })
		if i < 0 {
			i = len(s.history)
		}
		s.history = slices.Clone(s.history[i:])
	}
}

func (s *State) say(seq uint64, m Message) {
	s.history = append(s.history, entry{seq, m})
}

func cloneParts(parts []Part) []Part {
	parts = slices.Clone(parts)
	for i := range parts {
		parts[i].Arguments = slices.Clone(parts[i].Arguments)
	}
	return parts
}
