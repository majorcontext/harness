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
// summary of the newest compaction, then each later message with its ID.
func (s *State) Transcript() []protocol.Message {
	out := make([]protocol.Message, 0, len(s.history)+1)
	if c, ok := s.Compaction(); ok {
		out = append(out, protocol.Message{ID: summaryID(s.compactedAt), Role: RoleUser,
			Parts: []protocol.MessagePart{{Type: protocol.MessagePartText, Text: c.Summary}}})
	}
	for _, e := range s.history {
		if e.pinned {
			continue
		}
		out = append(out, protocol.Message{ID: e.id, Role: e.msg.Role, ParentCallID: e.msg.ParentCallID, Parts: messageParts(e.msg.Parts)})
	}
	return out
}

func messageParts(parts []Part) []protocol.MessagePart {
	out := make([]protocol.MessagePart, 0, len(parts))
	for _, p := range parts {
		mp := protocol.MessagePart{Type: p.Type, Text: p.Text, CallID: p.CallID, Name: p.Name, Arguments: slices.Clone(p.Arguments),
			IsError: p.IsError, MediaType: p.MediaType, Bytes: p.Bytes}
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
