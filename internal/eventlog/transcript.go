package eventlog

import (
	"cmp"
	"slices"
	"strconv"

	"github.com/majorcontext/harness/protocol"
)

// noToolOutput is the text of a tool result that has none.
const noToolOutput = "(no output)"

// lastMessageID returns the ID of the newest message, or "".
func (s *State) lastMessageID() string {
	for i := len(s.history) - 1; i >= 0; i-- {
		if !s.history[i].pinned {
			return s.history[i].id
		}
	}
	if s.compactedAt != 0 {
		return summaryID(s.compactedAt)
	}
	return ""
}

func summaryID(seq uint64) string { return "cmpsum_" + strconv.FormatUint(seq, 10) }

// Transcript returns the conversation as a reader sees it, oldest first: the
// messages that each compaction folded, the summary of each compaction as a
// divider between the messages it folded and the messages kept, then the
// messages after the newest summary. The model reads less: see ModelHistory.
func (s *State) Transcript() []protocol.Message {
	out := make([]protocol.Message, 0, len(s.folded)+len(s.history)+1)
	for _, m := range s.folded {
		out = append(out, cloneReaderMessage(m))
	}
	if _, ok := s.Compaction(); ok {
		out = append(out, s.summaryMessage())
	}
	for _, e := range s.history {
		if !e.pinned {
			out = append(out, readerMessage(e))
		}
	}
	return out
}

// summaryMessage is the divider message of the newest compaction.
func (s *State) summaryMessage() protocol.Message {
	return protocol.Message{ID: summaryID(s.compactedAt), Role: RoleUser, CreatedAt: s.compactedTime,
		Parts: []protocol.MessagePart{{Type: protocol.MessagePartText, Text: s.compaction.Summary}}}
}

func cloneReaderMessage(m protocol.Message) protocol.Message {
	m.Parts = slices.Clone(m.Parts)
	for i := range m.Parts {
		m.Parts[i].Arguments = slices.Clone(m.Parts[i].Arguments)
	}
	m.OperatorBatch = slices.Clone(m.OperatorBatch)
	return m
}

func readerMessage(e entry) protocol.Message {
	return protocol.Message{ID: e.id, Role: e.msg.Role, CreatedAt: e.at, ParentCallID: e.msg.ParentCallID, Parts: messageParts(e.msg.Parts),
		Source: e.from.source, SourceID: e.from.id, SourceLabel: e.from.label, OperatorBatch: operatorBatch(e.promoted)}
}

// operatorBatch returns one entry for each input that a steer message joined.
func operatorBatch(inputs []InputAdmitted) []protocol.OperatorBatchEntry {
	var out []protocol.OperatorBatchEntry
	for _, in := range inputs {
		out = append(out, protocol.OperatorBatchEntry{ID: in.InputID, Text: textOf(in.Parts), Source: in.Source, SourceID: in.SourceID, SourceLabel: in.SourceLabel})
	}
	return out
}

func messageParts(parts []Part) []protocol.MessagePart {
	out := make([]protocol.MessagePart, 0, len(parts))
	for _, p := range parts {
		mp := protocol.MessagePart{Type: p.Type, Text: p.Text, CallID: p.CallID, Name: p.Name, Arguments: slices.Clone(p.Arguments),
			IsError: p.IsError, MediaType: p.MediaType, Bytes: p.Bytes}
		if p.Type == PartBlob {
			mp.Key = p.BlobKey
		}
		if p.Type == PartToolResult {
			mp.Text, mp.Name, mp.Content = "", "", cmp.Or(p.Text, noToolOutput)
		}
		out = append(out, mp)
	}
	return out
}

// MessagePage returns the limit messages that precede seq before in the
// conversation of Transcript, numbered from 1. A before of 0 or past the end
// names the newest page. A limit of 0 is protocol.DefaultMessageLimit.
func (s *State) MessagePage(before uint64, limit int) protocol.MessagePage {
	msgs := s.Transcript()
	if limit <= 0 {
		limit = protocol.DefaultMessageLimit
	}
	total := uint64(len(msgs))
	hi := total
	if before > 0 && before-1 < hi {
		hi = before - 1
	}
	page := protocol.MessagePage{Messages: []protocol.Message{}, Total: total}
	if hi >= 1 {
		lo := uint64(1)
		if hi > uint64(limit) {
			lo = hi - uint64(limit) + 1
		}
		page.Messages = msgs[lo-1 : hi]
		page.FirstSeq, page.LastSeq, page.HasMore = lo, hi, lo > 1
	}
	page.Commands = s.commandsIn(page.Messages, page.FirstSeq == 1 || total == 0)
	return page
}

// commandsIn returns the commands that follow a message of window, in the
// order of their first records, and the commands that no message precedes
// when fromFirst is true.
func (s *State) commandsIn(window []protocol.Message, fromFirst bool) []protocol.MessageCommand {
	ids := make(map[string]bool, len(window))
	for _, m := range window {
		ids[m.ID] = true
	}
	var in []command
	for _, c := range s.commands {
		if (c.after == "" && fromFirst) || ids[c.after] {
			in = append(in, c)
		}
	}
	slices.SortFunc(in, func(a, b command) int { return cmp.Compare(a.seq, b.seq) })
	out := make([]protocol.MessageCommand, 0, len(in))
	for _, c := range in {
		r := c.rec
		out = append(out, protocol.MessageCommand{InputID: r.InputID, Line: r.Line, Name: r.Name, Args: r.Args, Status: r.Status,
			Text: r.Text, Result: r.Result, ResultTruncated: r.ResultTruncated, AfterMessageID: c.after, Seq: c.seq})
	}
	return out
}

// QueuedInputs returns the queued inputs as a reader sees them, oldest first.
func (s *State) QueuedInputs() []protocol.QueuedInput {
	out := make([]protocol.QueuedInput, len(s.queue))
	for i, in := range s.queue {
		out[i] = protocol.QueuedInput{ID: in.InputID, Parts: messageParts(in.Parts), Delivery: string(in.Delivery),
			Source: in.Source, SourceID: in.SourceID, SourceLabel: in.SourceLabel}
	}
	return out
}

// Attachment returns the media type and the recorded size of the blob part that
// an input.admitted record of the session names by key.
func (s *State) Attachment(key string) (mediaType string, size int, ok bool) {
	for _, in := range s.inputs {
		for _, p := range in.event.Parts {
			if p.Type == PartBlob && p.BlobKey == key {
				return p.MediaType, p.Bytes, true
			}
		}
	}
	return "", 0, false
}
