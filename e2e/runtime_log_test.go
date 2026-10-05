package e2e

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
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
	// promoted holds the inputs that one append promoted as this message.
	promoted [][]logPart
	last     uint64
}

// transcriptOfLog projects a session log as the model sees it: the summary
// of the newest compaction, then each later message. An input becomes a user
// message when a turn starts with it or promotes it. An input or item ID
// gets the msg_ prefix of a transcript ID. A tool result has no name, and an
// empty one reads as the model reads it.
func transcriptOfLog(t *testing.T, evs []protocol.Event) []transcriptMessage {
	t.Helper()
	inputs := map[string][]logPart{}
	callOf, itemOf := map[string]logPart{}, map[string]string{}
	var entries []logEntry
	var summary *transcriptMessage
	say := func(seq uint64, id, role string, parts []logPart) {
		entries = append(entries, logEntry{seq: seq, msg: transcriptMessage{ID: id, Role: role, Parts: transcriptParts(parts)}})
	}
	promote := func(seq uint64, id string, parts []logPart) {
		if n := len(entries); n > 0 && entries[n-1].promoted != nil && entries[n-1].last+1 == seq {
			e := &entries[n-1]
			e.promoted, e.last = append(e.promoted, parts), seq
			e.msg.Parts = steerParts(e.promoted)
			return
		}
		entries = append(entries, logEntry{seq: seq, msg: transcriptMessage{ID: "msg_" + id, Role: "user", Parts: steerParts([][]logPart{parts})},
			promoted: [][]logPart{parts}, last: seq})
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
				say(ev.Seq, "msg_"+id, "user", slices.DeleteFunc(slices.Clone(inputs[id]), func(p logPart) bool { return p.Type == eventlog.PartTaskReport }))
			}
		case "input.promoted":
			id := decodeEvent[logInput](t, ev).InputID
			promote(ev.Seq, id, inputs[id])
		case "item.completed":
			it := decodeEvent[logItem](t, ev)
			say(ev.Seq, "msg_"+it.ItemID, it.Message.Role, it.Message.Parts)
			for _, p := range it.Message.Parts {
				if p.Type == "tool_call" {
					callOf[it.ItemID] = p
				}
			}
		case "request.opened":
			o := decodeEvent[struct {
				RequestID string `json:"request_id"`
				ItemID    string `json:"item_id"`
			}](t, ev)
			itemOf[o.RequestID] = o.ItemID
		case "request.resolved":
			r := decodeEvent[struct {
				RequestID  string          `json:"request_id"`
				Resolution string          `json:"resolution"`
				Answer     json.RawMessage `json:"answer"`
			}](t, ev)
			if r.Resolution == "answered" {
				break
			}
			call := callOf[itemOf[r.RequestID]]
			part := logPart{Type: "tool_result", CallID: call.CallID, Name: call.Name, Text: dismissalText, IsError: true}
			say(ev.Seq, "msg_resolved_"+r.RequestID, "tool", []logPart{part})
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

// steerParts renders inputs that join a running turn as the model reads them.
func steerParts(inputs [][]logPart) []transcriptPart {
	var in [][]eventlog.Part
	for _, parts := range inputs {
		var ps []eventlog.Part
		for _, p := range parts {
			ps = append(ps, eventlog.Part{Type: p.Type, Text: p.Text})
		}
		in = append(in, ps)
	}
	var out []transcriptPart
	for _, p := range eventlog.SteerMessage(in).Parts {
		out = append(out, transcriptPart{Type: p.Type, Text: p.Text})
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

// dismissalText is the result that the model reads for a dismissed request.
const dismissalText = "The user dismissed this question without answering."
