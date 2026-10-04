package e2e

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/protocol"
)

// logPart, logInput, and logItem decode the payloads of the session log
// records that make up its transcript.
type logPart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	IsError   bool            `json:"is_error"`
}

type logInput struct {
	InputID string    `json:"input_id"`
	Parts   []logPart `json:"parts"`
}

type logItem struct {
	ItemID  string `json:"item_id"`
	Message struct {
		Role  string    `json:"role"`
		Parts []logPart `json:"parts"`
	} `json:"message"`
}

type logEntry struct {
	seq uint64
	msg transcriptMessage
}

// transcriptOfLog projects a session log as the model sees it: the summary
// of the newest compaction, then each later message. An input becomes a user
// message when a turn starts with it or promotes it. An input or item ID
// gets the msg_ prefix of a transcript ID. A tool result has no name, and an
// empty one reads as the model reads it.
func transcriptOfLog(t *testing.T, evs []protocol.Event) []transcriptMessage {
	t.Helper()
	inputs := map[string][]logPart{}
	var entries []logEntry
	var summary *transcriptMessage
	say := func(seq uint64, id, role string, parts []logPart) {
		entries = append(entries, logEntry{seq, transcriptMessage{ID: id, Role: role, Parts: transcriptParts(parts)}})
	}
	for _, ev := range evs {
		switch ev.Kind {
		case "input.admitted":
			in := decodeEvent[logInput](t, ev)
			inputs[in.InputID] = in.Parts
		case "turn.started":
			for _, id := range decodeEvent[struct {
				InputIDs []string `json:"input_ids"`
			}](t, ev).InputIDs {
				say(ev.Seq, "msg_"+id, "user", inputs[id])
			}
		case "input.promoted":
			id := decodeEvent[logInput](t, ev).InputID
			say(ev.Seq, "msg_"+id, "user", inputs[id])
		case "item.completed":
			it := decodeEvent[logItem](t, ev)
			say(ev.Seq, "msg_"+it.ItemID, it.Message.Role, it.Message.Parts)
		case "compaction.applied":
			c := decodeEvent[struct {
				ToSeq   uint64 `json:"to_seq"`
				Summary string `json:"summary"`
			}](t, ev)
			entries = slices.DeleteFunc(entries, func(e logEntry) bool { return e.seq <= c.ToSeq })
			summary = &transcriptMessage{ID: "cmpsum_" + strconv.FormatUint(ev.Seq, 10), Role: "user",
				Parts: []transcriptPart{{Type: "text", Text: c.Summary}}}
		}
	}
	var out []transcriptMessage
	if summary != nil {
		out = append(out, *summary)
	}
	for _, e := range entries {
		out = append(out, e.msg)
	}
	return out
}

func transcriptParts(parts []logPart) []transcriptPart {
	out := make([]transcriptPart, 0, len(parts))
	for _, p := range parts {
		tp := transcriptPart{Type: p.Type, Text: p.Text, CallID: p.CallID, Name: p.Name, IsError: p.IsError}
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments, &tp.Arguments)
		}
		if p.Type == "tool_result" {
			tp.Text, tp.Name, tp.Content = "", "", cmp.Or(p.Text, message.NoToolOutputText)
		}
		out = append(out, tp)
	}
	return out
}

// journalOfLog lists the durable events of one session log. An item is the
// journal entry of a message.
func journalOfLog(t *testing.T, evs []protocol.Event) []journalEntry {
	t.Helper()
	out := make([]journalEntry, len(evs))
	for i, ev := range evs {
		out[i] = journalEntry{Seq: int64(ev.Seq)}
		if ev.Kind == "item.completed" {
			out[i].IsMessage, out[i].MessageID = true, decodeEvent[logItem](t, ev).ItemID
		}
	}
	return out
}
