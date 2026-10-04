package eventlog

import (
	"fmt"
	"slices"
	"strings"
)

type entry struct {
	seq uint64
	msg Message
	// promoted holds the inputs that one append promoted into a running turn
	// as this message, and last the seq of the newest of them.
	promoted [][]Part
	last     uint64
}

// SteerMessage is the message that the model reads for inputs that join a
// running turn at an item boundary: one numbered block that tells the model
// to address them and then to continue its task.
func SteerMessage(inputs [][]Part) Message {
	var b strings.Builder
	b.WriteString("OPERATOR MESSAGES (address these, then continue the task):\n")
	for i, parts := range inputs {
		var text []string
		for _, p := range parts {
			if p.Type == PartText {
				text = append(text, p.Text)
			}
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, strings.Join(text, "\n"))
	}
	return Message{Role: RoleUser, Parts: []Part{{Type: PartText, Text: b.String()}}}
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
		parts := s.inputs[e.InputID].event.Parts
		if n := len(s.history); n > 0 && s.history[n-1].promoted != nil && s.history[n-1].last+1 == env.Seq {
			h := s.history[n-1]
			h.promoted, h.last = append(slices.Clip(h.promoted), parts), env.Seq
			h.msg = SteerMessage(h.promoted)
			s.history = append(s.history[:n-1:n-1], h)
			break
		}
		s.history = append(s.history, entry{seq: env.Seq, msg: SteerMessage([][]Part{parts}), promoted: [][]Part{parts}, last: env.Seq})
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
	s.history = append(s.history, entry{seq: seq, msg: m})
}

func cloneParts(parts []Part) []Part {
	parts = slices.Clone(parts)
	for i := range parts {
		parts[i].Arguments = slices.Clone(parts[i].Arguments)
	}
	return parts
}

// Fold returns the history before the newest keep turns, and the seq before
// the first kept turn. A user message starts a turn. ok is false when keep
// turns or fewer exist, or when only the compaction summary would fold.
func (s *State) Fold(keep int) (folded []Message, toSeq uint64, ok bool) {
	h := s.History()
	lead := len(h) - len(s.history)
	var starts []int
	for i, m := range h {
		if m.Role == RoleUser {
			starts = append(starts, i)
		}
	}
	if len(starts) <= keep {
		return nil, 0, false
	}
	end := starts[len(starts)-keep]
	if end <= lead {
		return nil, 0, false
	}
	return h[:end], s.history[end-lead].seq - 1, true
}
