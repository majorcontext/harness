package eventlog

import (
	"fmt"
	"slices"
	"strings"
)

type entry struct {
	seq uint64
	msg Message
	// by is the provider that ran the turn of the message, and turn counts
	// the turns that started before it.
	by   string
	turn int
	// promoted holds the inputs that one append promoted into a running turn
	// as this message, and last the seq of the newest of them.
	promoted [][]Part
	last     uint64
}

// SteerMessage is the message that the model reads for inputs that join a
// running turn at an item boundary: one numbered block that tells the model
// to address them and then to continue its task. An input that reports a
// child is not in the block; the model reads its task lines in one engine
// context part, as the engine pinned them.
func SteerMessage(inputs [][]Part) Message {
	var b strings.Builder
	var blobs []Part
	var tasks []string
	n := 0
	for _, parts := range inputs {
		var text []string
		for _, p := range parts {
			switch p.Type {
			case PartText:
				text = append(text, p.Text)
			case PartBlob:
				blobs = append(blobs, p)
			case PartTaskReport:
				tasks = append(tasks, p.Text)
			}
		}
		if slices.ContainsFunc(parts, func(p Part) bool { return p.Type == PartTaskReport }) {
			continue
		}
		if n++; n == 1 {
			b.WriteString("OPERATOR MESSAGES (address these, then continue the task):\n")
		}
		fmt.Fprintf(&b, "%d. %s\n", n, strings.Join(text, "\n"))
	}
	var out []Part
	if n > 0 {
		out = append(out, Part{Type: PartText, Text: b.String()})
	}
	out = append(out, blobs...)
	if len(tasks) > 0 {
		out = append(out, Part{Type: PartEngineContext, Text: "[tasks:\n- " + strings.Join(tasks, "\n- ") + "\n]"})
	}
	return Message{Role: RoleUser, Parts: out}
}

// withoutTaskReports returns parts with the task lines left out: a turn that
// an input starts reads the text of the input.
func withoutTaskReports(parts []Part) []Part {
	return slices.DeleteFunc(slices.Clone(parts), func(p Part) bool { return p.Type == PartTaskReport })
}

// History returns the conversation that the model sees: the summary of the
// newest compaction as a user message, then each later message in log order.
func (s *State) History() []Message {
	var out []Message
	if c, ok := s.Compaction(); ok {
		out = append(out, Message{Role: RoleUser, Parts: []Part{{Type: PartText, Text: c.Summary}}})
	}
	for _, e := range s.history {
		out = append(out, Message{Role: e.msg.Role, Parts: cloneParts(e.msg.Parts), ParentCallID: e.msg.ParentCallID})
	}
	return out
}

// remember records the messages of an applied event.
func (s *State) remember(env Envelope) {
	switch e := env.Event.(type) {
	case TurnStarted:
		for _, id := range e.InputIDs {
			s.say(env.Seq, Message{Role: RoleUser, Parts: withoutTaskReports(s.inputs[id].event.Parts)})
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
		s.history = append(s.history, entry{seq: env.Seq, msg: SteerMessage([][]Part{parts}), by: s.turnBy, turn: s.turnN, promoted: [][]Part{parts}, last: env.Seq})
	case ItemCompleted:
		s.say(env.Seq, e.Message)
	case CompactionApplied:
		i := slices.IndexFunc(s.history, func(h entry) bool { return h.seq > e.ToSeq })
		if i < 0 {
			i = len(s.history)
		}
		s.history = slices.Clone(s.history[i:])
		s.turnAt = max(0, s.turnAt-i)
	}
}

func (s *State) say(seq uint64, m Message) {
	s.history = append(s.history, entry{seq: seq, msg: m, by: s.turnBy, turn: s.turnN})
}

// ProviderOf returns the provider of a model reference.
func ProviderOf(model string) string {
	p, _, _ := strings.Cut(model, "/")
	return p
}

// Foreign reports whether the history holds a message that another provider
// recorded after the newest message that the provider of model recorded
// before the current turn. A turn belongs to the provider of the model at its
// start, whatever a settings change does while it runs. A turn that recorded
// no item of its own, as one whose backend never started, did not show its
// backend anything, so it counts as foreign to every provider. A backend that
// keeps its own session has not seen such a message. The current turn is left
// out, as it brings its own input.
func (s *State) Foreign(model string) bool {
	by, end := ProviderOf(model), len(s.history)
	if s.turn.ID != "" {
		end = s.turnAt
	}
	foreign := func(e entry) bool { return e.by != by || slices.Contains(s.unran, e.turn) }
	last := end - 1
	for last >= 0 && foreign(s.history[last]) {
		last--
	}
	return last < end-1
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

// dismissal is the result of the tool call that a dismissed request held
// open: what the model reads in the history.
func dismissal(c OpenToolCall) Message {
	return Message{Role: RoleTool, Parts: []Part{{Type: PartToolResult, CallID: c.CallID, Name: c.Name,
		Text: "The user dismissed this question without answering.", IsError: true}}}
}
