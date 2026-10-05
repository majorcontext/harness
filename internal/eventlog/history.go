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
	// pinned marks a segment that the model reads at this place in every
	// later call and that the history readers never see.
	pinned bool
}

// IsReport reports whether parts hold the report of a child. A caller cannot
// write a task report part, so this tells a report from a prompt that names
// source child.
func IsReport(parts []Part) bool {
	return slices.ContainsFunc(parts, func(p Part) bool { return p.Type == PartTaskReport })
}

// SteerMessages returns what the model reads for inputs that join a running
// turn at an item boundary: one numbered block that tells the model to
// address the inputs and then to continue its task, and then the pinned
// segment that holds the task lines of the reports. A report is not in the
// block, as the engine pinned its task lines.
func SteerMessages(inputs [][]Part) []Message {
	var ops, reports [][]Part
	for _, parts := range inputs {
		if IsReport(parts) {
			reports = append(reports, parts)
		} else {
			ops = append(ops, parts)
		}
	}
	var out []Message
	if len(ops) > 0 {
		out = append(out, SteerMessage(ops))
	}
	if len(reports) > 0 {
		out = append(out, pinMessage(reports))
	}
	return out
}

// SteerMessage is the message for the inputs that join a running turn, with
// no report among them.
func SteerMessage(inputs [][]Part) Message {
	var b strings.Builder
	var blobs []Part
	for n, parts := range inputs {
		var text []string
		for _, p := range parts {
			switch p.Type {
			case PartText:
				text = append(text, p.Text)
			case PartBlob:
				blobs = append(blobs, p)
			}
		}
		if n == 0 {
			b.WriteString("OPERATOR MESSAGES (address these, then continue the task):\n")
		}
		fmt.Fprintf(&b, "%d. %s\n", n+1, strings.Join(text, "\n"))
	}
	return Message{Role: RoleUser, Parts: append([]Part{{Type: PartText, Text: b.String()}}, blobs...)}
}

func pinMessage(reports [][]Part) Message {
	var lines []string
	for _, parts := range reports {
		for _, p := range parts {
			if p.Type == PartTaskReport {
				lines = append(lines, p.Text)
			}
		}
	}
	return Message{Role: RoleUser, Parts: []Part{{Type: PartEngineContext, Text: TaskSegment(lines)}}}
}

// TaskSegment is the segment of the engine that holds the task lines of
// reports: one line for each report between "[tasks:" and "]".
func TaskSegment(lines []string) string {
	return "[tasks:\n- " + strings.Join(lines, "\n- ") + "\n]"
}

// withoutTaskReports returns parts with the task lines left out: a turn that
// an input starts reads the text of the input.
func withoutTaskReports(parts []Part) []Part {
	return slices.DeleteFunc(slices.Clone(parts), func(p Part) bool { return p.Type == PartTaskReport })
}

// History returns the conversation that readers see: the summary of the
// newest compaction as a user message, then each later message in log order.
// A pinned segment is not in it.
func (s *State) History() []Message { return s.messages(false) }

// ModelHistory returns History with each pinned segment at its place: the
// conversation that the model reads.
func (s *State) ModelHistory() []Message { return s.messages(true) }

func (s *State) messages(pinned bool) []Message {
	var out []Message
	if c, ok := s.Compaction(); ok {
		out = append(out, Message{Role: RoleUser, Parts: []Part{{Type: PartText, Text: c.Summary}}})
	}
	for _, e := range s.history {
		if e.pinned && !pinned {
			continue
		}
		out = append(out, Message{Role: e.msg.Role, Parts: cloneParts(e.msg.Parts), ParentCallID: e.msg.ParentCallID})
	}
	if pinned {
		for _, e := range s.stranded {
			out = append(out, Message{Role: e.msg.Role, Parts: cloneParts(e.msg.Parts), ParentCallID: e.msg.ParentCallID})
		}
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
		s.settle(env.Seq)
	case InputPromoted:
		parts := s.inputs[e.InputID].event.Parts
		pin := IsReport(parts)
		build := SteerMessage
		if pin {
			build = pinMessage
		}
		if n := len(s.history); n > 0 && s.history[n-1].promoted != nil && s.history[n-1].pinned == pin && s.history[n-1].last+1 == env.Seq {
			h := s.history[n-1]
			h.promoted, h.last = append(slices.Clip(h.promoted), parts), env.Seq
			h.msg = build(h.promoted)
			s.history = append(s.history[:n-1:n-1], h)
			break
		}
		s.history = append(s.history, entry{seq: env.Seq, msg: build([][]Part{parts}), by: s.turnBy, turn: s.turnN, promoted: [][]Part{parts}, last: env.Seq, pinned: pin})
	case ItemCompleted:
		s.say(env.Seq, e.Message)
	case CompactionApplied:
		i := slices.IndexFunc(s.history, func(h entry) bool { return h.seq > e.ToSeq })
		if i < 0 {
			i = len(s.history)
		}
		folded := slices.DeleteFunc(slices.Clone(s.history[:i]), func(h entry) bool { return !h.pinned })
		s.history = slices.Clone(s.history[i:])
		s.turnAt = max(0, s.turnAt-i)
		s.stranded = slices.Concat(s.stranded, folded)
	}
}

// settle puts the stranded pinned segments at the end of the history.
func (s *State) settle(seq uint64) {
	for _, e := range s.stranded {
		e.seq, e.by, e.turn = seq, s.turnBy, s.turnN
		s.history = append(s.history, e)
	}
	s.stranded = nil
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
	var seqs []uint64
	for _, e := range s.history {
		if !e.pinned {
			seqs = append(seqs, e.seq)
		}
	}
	lead := len(h) - len(seqs)
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
	return h[:end], seqs[end-lead] - 1, true
}

// dismissal is the result of the tool call that a dismissed request held
// open: what the model reads in the history.
func dismissal(c OpenToolCall) Message {
	return Message{Role: RoleTool, Parts: []Part{{Type: PartToolResult, CallID: c.CallID, Name: c.Name,
		Text: "The user dismissed this question without answering.", IsError: true}}}
}
