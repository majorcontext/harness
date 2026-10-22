package eventlog

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/majorcontext/harness/protocol"
)

type provenance struct{ source, id, label string }

type entry struct {
	seq uint64
	// at is the time of the record that completed the message: the newest
	// input of a message of promoted inputs.
	at time.Time
	// id is the ID that a reader sees for the message.
	id  string
	msg protocol.LogMessage
	// by is the provider that ran the turn of the message, and turn counts
	// the turns that started before it.
	by   string
	turn int
	// promoted holds the inputs that one append promoted into a running turn
	// as this message, and last the seq of the newest of them.
	promoted []protocol.InputAdmitted
	last     uint64
	// from is the provenance of the one input that started a turn with this
	// message. A message of promoted inputs keeps each input's own.
	from provenance
	// pinned marks a segment that the model reads at this place in every
	// later call and that the history readers never see.
	pinned bool
	// readerOnly marks a message that the readers see and the model never
	// reads: the response that answered a question, which the model reads
	// once, in the result of the question.
	readerOnly bool
	// carried marks a user message of the running turn that a compaction
	// folded: the model reads it after the summary, and the readers see it
	// only where it was folded.
	carried bool
}

// ReportTrigger is the text of a turn that only reports of children start,
// before the segment that holds their task lines.
const ReportTrigger = "A background task you started has finished. " +
	"See the engine context below for its result, and continue accordingly."

// IsReport reports whether parts hold the report of a child. A caller cannot
// write a task report part, so this tells a report from a prompt that names
// source child.
func IsReport(parts []protocol.LogPart) bool {
	return slices.ContainsFunc(parts, func(p protocol.LogPart) bool { return p.Type == protocol.LogPartTaskReport })
}

// SteerMessages returns what the model reads for inputs that join a running
// turn at an item boundary: one numbered block that tells the model to
// address the inputs and then to continue its task, and then the pinned
// segment that holds the task lines of the reports. A report is not in the
// block; its task line is pinned.
func SteerMessages(inputs [][]protocol.LogPart) []protocol.LogMessage {
	var ops, reports [][]protocol.LogPart
	for _, parts := range inputs {
		if IsReport(parts) {
			reports = append(reports, parts)
		} else {
			ops = append(ops, parts)
		}
	}
	var out []protocol.LogMessage
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
func SteerMessage(inputs [][]protocol.LogPart) protocol.LogMessage {
	var b strings.Builder
	var blobs []protocol.LogPart
	for n, parts := range inputs {
		if n == 0 {
			b.WriteString("OPERATOR MESSAGES (address these, then continue the task):\n")
		}
		fmt.Fprintf(&b, "%d. %s\n", n+1, textOf(parts))
		for _, p := range parts {
			if p.Type == protocol.LogPartBlob {
				blobs = append(blobs, p)
			}
		}
	}
	return protocol.LogMessage{Role: protocol.RoleUser, Parts: append([]protocol.LogPart{{Type: protocol.LogPartText, Text: b.String()}}, blobs...)}
}

// textOf joins the text parts of an input, one to a line.
func textOf(parts []protocol.LogPart) string {
	var text []string
	for _, p := range parts {
		if p.Type == protocol.LogPartText {
			text = append(text, p.Text)
		}
	}
	return strings.Join(text, "\n")
}

func partsOf(inputs []protocol.InputAdmitted) [][]protocol.LogPart {
	out := make([][]protocol.LogPart, len(inputs))
	for i, in := range inputs {
		out[i] = in.Parts
	}
	return out
}

func pinMessage(reports [][]protocol.LogPart) protocol.LogMessage {
	var lines []string
	for _, parts := range reports {
		for _, p := range parts {
			if p.Type == protocol.LogPartTaskReport {
				lines = append(lines, p.Text)
			}
		}
	}
	return protocol.LogMessage{Role: protocol.RoleUser, Parts: []protocol.LogPart{{Type: protocol.LogPartEngineContext, Text: TaskSegment(lines)}}}
}

// TaskSegment is the segment that holds the task lines of
// reports: one line for each report between "[tasks:" and "]".
func TaskSegment(lines []string) string {
	return "[tasks:\n- " + strings.Join(lines, "\n- ") + "\n]"
}

// withoutTaskReports returns parts with the task lines left out: a turn that
// an input starts reads the text of the input.
func withoutTaskReports(parts []protocol.LogPart) []protocol.LogPart {
	return slices.DeleteFunc(slices.Clone(parts), func(p protocol.LogPart) bool { return p.Type == protocol.LogPartTaskReport })
}

// History returns the summary of the newest compaction as a user message, then
// each later message in log order, with no pinned segment. Readers see more:
// see Transcript.
func (s *State) History() []protocol.LogMessage { return s.messages(false) }

// ModelHistory returns History with each pinned segment at its place: the
// conversation that the model reads.
func (s *State) ModelHistory() []protocol.LogMessage { return s.messages(true) }

// SummaryBanner leads the summary that the model reads, so that the model does not take the
// summary for text that the user typed. The record holds the plain summary.
const SummaryBanner = "[compacted summary of earlier conversation]\n\n"

// modelSummary returns the text of the summary message that the model reads:
// the banner, the summary, and the index of the retained tool results.
func modelSummary(c protocol.CompactionApplied) string {
	text := SummaryBanner + c.Summary
	if c.Index != "" {
		text += "\n\n" + c.Index
	}
	return text
}

func (s *State) messages(pinned bool) []protocol.LogMessage {
	var out []protocol.LogMessage
	if c, ok := s.Compaction(); ok {
		out = append(out, protocol.LogMessage{Role: protocol.RoleUser, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: modelSummary(c)}}})
	}
	for _, e := range s.history {
		if e.pinned && !pinned || e.readerOnly {
			continue
		}
		out = append(out, protocol.LogMessage{Role: e.msg.Role, Parts: cloneParts(e.msg.Parts)})
	}
	if pinned {
		for _, e := range s.stranded {
			out = append(out, protocol.LogMessage{Role: e.msg.Role, Parts: cloneParts(e.msg.Parts)})
		}
	}
	return out
}

// remember records the messages of an applied event.
func (s *State) remember(env Envelope) {
	switch e := env.Event.(type) {
	case protocol.TurnStarted:
		var reports [][]protocol.LogPart
		var others int
		for _, id := range e.InputIDs {
			if in := s.inputs[id].event; IsReport(in.Parts) {
				reports = append(reports, in.Parts)
			} else {
				others++
			}
		}
		sayReport := others == 0
		for _, id := range e.InputIDs {
			in := s.inputs[id].event
			if IsReport(in.Parts) && !sayReport {
				continue
			}
			s.say(env.Seq, env.Time, "msg_"+id, protocol.LogMessage{Role: protocol.RoleUser, Parts: withoutTaskReports(in.Parts)})
			s.history[len(s.history)-1].from = provenance{in.Source, in.SourceID, in.SourceLabel}
			sayReport = false
		}
		if len(reports) > 0 {
			s.history = append(s.history, entry{seq: env.Seq, at: env.Time, id: "pin_" + e.TurnID, msg: pinMessage(reports), by: s.turnBy, turn: s.turnN, pinned: true})
		}
		s.settle(env.Seq)
	case protocol.InputPromoted:
		in := s.inputs[e.InputID].event
		pin := IsReport(in.Parts)
		build := SteerMessage
		if pin {
			build = pinMessage
		}
		if n := len(s.history); n > 0 && s.history[n-1].promoted != nil && s.history[n-1].pinned == pin && s.history[n-1].last+1 == env.Seq {
			h := s.history[n-1]
			h.promoted, h.last, h.at = append(slices.Clip(h.promoted), in), env.Seq, env.Time
			h.msg = build(partsOf(h.promoted))
			s.history = append(s.history[:n-1:n-1], h)
			break
		}
		s.history = append(s.history, entry{seq: env.Seq, at: env.Time, id: "msg_" + e.InputID, msg: build([][]protocol.LogPart{in.Parts}), by: s.turnBy, turn: s.turnN, promoted: []protocol.InputAdmitted{in}, last: env.Seq, pinned: pin})
	case protocol.ItemCompleted:
		s.say(env.Seq, env.Time, "msg_"+e.ItemID, e.Message)
	case protocol.CompactionApplied:
		i := slices.IndexFunc(s.history, func(h entry) bool { return h.seq > e.ToSeq })
		if i < 0 {
			i = len(s.history)
		}
		var carried []entry
		for _, h := range s.history[:i] {
			if h.pinned {
				continue
			}
			if !h.carried {
				s.folded = append(s.folded, readerMessage(h))
			}
			if !e.ByBackend && s.turn.ID != "" && h.turn == s.turnN && h.msg.Role == protocol.RoleUser && !h.readerOnly {
				h.carried = true
				carried = append(carried, h)
			}
		}
		folded := slices.DeleteFunc(slices.Clone(s.history[:i]), func(h entry) bool { return !h.pinned })
		kept, turnAt := movePins(s.history[i:], max(0, s.turnAt-i), env.Seq)
		s.history, s.turnAt = slices.Concat(carried, kept), turnAt
		s.stranded = slices.Concat(s.stranded, folded)
		if s.turn.ID != "" {
			s.settle(env.Seq)
		}
		s.compactedLen = len(s.messages(true))
	}
}

// movePins returns h with each pinned segment after the other messages, in
// their order, and the start of the running turn less the pins that moved
// from before it. A pin that a compaction keeps follows the last message of
// the kept history; its slot is clamped to the end.
func movePins(h []entry, turnAt int, seq uint64) ([]entry, int) {
	out := make([]entry, 0, len(h))
	var pins []entry
	for i, e := range h {
		if !e.pinned {
			out = append(out, e)
			continue
		}
		e.seq = seq
		pins = append(pins, e)
		if i < turnAt {
			turnAt--
		}
	}
	return append(out, pins...), turnAt
}

// settle puts the stranded pinned segments at the end of the history. A
// compaction in a running turn settles at once, where the turn loop already
// reads them.
func (s *State) settle(seq uint64) {
	for _, e := range s.stranded {
		e.seq, e.by, e.turn = seq, s.turnBy, s.turnN
		s.history = append(s.history, e)
	}
	s.stranded = nil
}

func (s *State) say(seq uint64, at time.Time, id string, m protocol.LogMessage) {
	s.history = append(s.history, entry{seq: seq, at: at, id: id, msg: m, by: s.turnBy, turn: s.turnN})
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

func cloneParts(parts []protocol.LogPart) []protocol.LogPart {
	parts = slices.Clone(parts)
	for i := range parts {
		parts[i].Arguments = slices.Clone(parts[i].Arguments)
		parts[i].Results, parts[i].Citations = slices.Clone(parts[i].Results), slices.Clone(parts[i].Citations)
	}
	return parts
}

// Fold returns the history before the newest keep turns, and the seq before
// the first kept turn. A user message starts a turn. When no turn before the
// newest keep folds and a turn runs, it returns the history of the running
// turn before its newest round trip. ok is false when nothing folds, or when
// only the compaction summary would fold.
func (s *State) Fold(keep int) (folded []protocol.LogMessage, toSeq uint64, ok bool) {
	h := s.History()
	var es []entry
	for _, e := range s.history {
		if !e.pinned && !e.readerOnly {
			es = append(es, e)
		}
	}
	lead := len(h) - len(es)
	var starts []int
	for i, m := range h {
		if m.Role == protocol.RoleUser && (i < lead || !es[i-lead].carried) {
			starts = append(starts, i)
		}
	}
	if len(starts) > keep {
		if end := starts[len(starts)-keep]; end > lead {
			return h[:end], es[end-lead].seq - 1, true
		}
	}
	return s.foldRunning(h, es, lead)
}

// foldRunning returns the history of the running turn before its newest round
// trip: an assistant message and the messages after it. h holds lead messages
// before es, the messages that the log holds.
func (s *State) foldRunning(h []protocol.LogMessage, es []entry, lead int) ([]protocol.LogMessage, uint64, bool) {
	if s.turn.ID == "" {
		return nil, 0, false
	}
	var starts []int
	for i, e := range es {
		if e.turn == s.turnN && e.msg.Role == protocol.RoleAssistant && (i == 0 || es[i-1].msg.Role != protocol.RoleAssistant) {
			starts = append(starts, i)
		}
	}
	if len(starts) < 2 {
		return nil, 0, false
	}
	end := starts[len(starts)-1]
	return h[:lead+end], es[end].seq - 1, true
}

// dismissal is the result of the tool call that a dismissed request held
// open: what the model reads in the history. It names label when label is set.
func dismissal(c OpenToolCall, label string) protocol.LogMessage {
	return protocol.LogMessage{Role: protocol.RoleTool, Parts: []protocol.LogPart{{Type: protocol.LogPartToolResult, CallID: c.CallID, Name: c.Name,
		Text: protocol.DismissalText(label), IsError: true}}}
}
