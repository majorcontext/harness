package migrate

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/message"
)

// old is one session as the old format holds it.
type old struct {
	created eventlog.SessionCreated
	at      time.Time
	// history is the transcript that the old reader shows, with no open tool call.
	history []message.Message
	// commands are the slash commands, each in its newest status.
	commands []message.CommandRecord
	// compacted reports that the first message of history is a compaction summary.
	compacted bool
	// end ends the last turn. The builder sets its turn ID.
	end eventlog.TurnEnded
	// tail follows the last turn.
	tail  []eventlog.Event
	blobs map[string][]byte
}

type builder struct {
	st      eventlog.State
	records [][]byte
	at      time.Time
	turn    string
	names   map[string]string
	ids     map[string]bool
}

// build returns the records of o and the message count of their history.
// It applies each record as replay does, and fails when the replayed
// history is not the history of o.
func build(o old) ([][]byte, int, error) {
	b := &builder{at: o.at, names: map[string]string{}, ids: map[string]bool{}}
	lead, after := b.commands(o)
	if err := b.add(o.at, o.created); err != nil {
		return nil, 0, err
	}
	h := o.history
	var want []eventlog.Message
	if o.compacted && len(h) > 0 && h[0].Role == message.RoleUser {
		summary := summaryText(h[0])
		if err := b.add(h[0].CreatedAt, eventlog.CompactionApplied{FromSeq: 1, ToSeq: 1, Summary: summary}); err != nil {
			return nil, 0, err
		}
		want = append(want, eventlog.Message{Role: eventlog.RoleUser, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: summary}}})
		lead = append(lead, after[h[0].ID]...)
		delete(after, h[0].ID)
		h = h[1:]
	}
	if err := b.add(b.at, lead...); err != nil {
		return nil, 0, err
	}
	for i, m := range h {
		msg := convertMessage(m, b.names)
		want = append(want, msg)
		if err := b.message(i, m, msg); err != nil {
			return nil, 0, err
		}
		if err := b.add(b.at, after[m.ID]...); err != nil {
			return nil, 0, err
		}
		delete(after, m.ID)
	}
	if b.turn != "" {
		o.end.TurnID = b.turn
		if err := b.add(b.at, o.end); err != nil {
			return nil, 0, err
		}
	}
	for _, e := range o.tail {
		if err := b.add(b.at, e); err != nil {
			return nil, 0, err
		}
	}
	return b.records, len(want), same(b.st.History(), want)
}

// commands returns the command records of o: lead holds each command whose
// message is not in the history, and after holds the others by the ID of
// the message that they follow. It reserves each command ID, which a
// command record shares with no input.
func (b *builder) commands(o old) (lead []eventlog.Event, after map[string][]eventlog.Event) {
	after = map[string][]eventlog.Event{}
	in := map[string]bool{}
	for _, m := range o.history {
		in[m.ID] = true
	}
	for i, c := range o.commands {
		id := cmp.Or(c.ID, fmt.Sprintf("migrated_command_%d", i))
		b.ids[id] = true
		e := eventlog.CommandRecorded{InputID: id, Line: c.Line, Name: c.Name, Args: c.Args, Status: string(c.Status),
			Text: c.Text, Result: c.Result, ResultTruncated: c.ResultTruncated}
		if len(e.Args) == 0 {
			e.Args = nil
		}
		if c.AfterMessageID != "" && in[c.AfterMessageID] {
			after[c.AfterMessageID] = append(after[c.AfterMessageID], e)
		} else {
			lead = append(lead, e)
		}
	}
	return lead, after
}

// message adds m. A user message starts a turn, unless a tool call of the
// running turn has no result yet; then it joins that turn as an item.
func (b *builder) message(i int, m message.Message, msg eventlog.Message) error {
	id := m.ID
	if id == "" || b.ids[id] {
		id = fmt.Sprintf("migrated_%d", i)
	}
	b.ids[id] = true
	at := cmp.Or(m.CreatedAt, b.at)
	if m.Role == message.RoleUser && len(b.st.OpenToolCalls()) == 0 {
		if err := b.endTurn(at); err != nil {
			return err
		}
		b.turn = "turn_" + id
		return b.add(at,
			eventlog.InputAdmitted{InputID: id, Delivery: eventlog.DeliveryQueue, Source: cmp.Or(string(m.Source), m.Origin, "user"), Parts: msg.Parts},
			eventlog.TurnStarted{TurnID: b.turn, InputIDs: []string{id}})
	}
	if b.turn == "" {
		b.turn = "turn_" + id
		if err := b.add(at, eventlog.TurnStarted{TurnID: b.turn}); err != nil {
			return err
		}
	}
	return b.add(at, eventlog.ItemCompleted{ItemID: id, TurnID: b.turn, Message: msg})
}

func (b *builder) endTurn(at time.Time) error {
	if b.turn == "" {
		return nil
	}
	err := b.add(at, eventlog.TurnEnded{TurnID: b.turn, StopReason: eventlog.StopCompleted})
	b.turn = ""
	return err
}

func (b *builder) add(at time.Time, events ...eventlog.Event) error {
	b.at = cmp.Or(at, b.at)
	for _, e := range events {
		seq := b.st.Head() + 1
		data, err := eventlog.Envelope{Seq: seq, Time: b.at, Event: e}.Encode()
		if err != nil {
			return err
		}
		if err := b.st.Apply(eventlog.Record{Seq: seq, Data: data}); err != nil {
			return err
		}
		b.records = append(b.records, data)
	}
	return nil
}

// same compares two histories by their record encoding, which is the form
// that a replay reads.
func same(got, want []eventlog.Message) error {
	if len(got) != len(want) {
		return fmt.Errorf("replay has %d messages, want %d", len(got), len(want))
	}
	for i := range got {
		g, err := json.Marshal(got[i])
		if err != nil {
			return err
		}
		w, err := json.Marshal(want[i])
		if err != nil {
			return err
		}
		if !bytes.Equal(g, w) {
			return fmt.Errorf("replay message %d is %s, want %s", i, g, w)
		}
	}
	return nil
}

// summaryText joins the text parts of a compaction summary as a compaction
// joins its summary and its index of retained results.
func summaryText(m message.Message) string {
	var out string
	for _, p := range m.Parts {
		if t, ok := p.(*message.Text); ok {
			if out != "" {
				out += "\n\n"
			}
			out += t.Text
		}
	}
	return out
}

// convertMessage returns m as the event log holds it. names maps each call
// ID that it has seen to its tool name. An engine-context part is
// request-only and is dropped. An attachment becomes a note, because a log
// message holds text only.
func convertMessage(m message.Message, names map[string]string) eventlog.Message {
	out := eventlog.Message{Role: string(m.Role), Parts: []eventlog.Part{}, ParentCallID: m.ParentToolUseID}
	for _, p := range m.Parts {
		switch p := p.(type) {
		case *message.Text:
			out.Parts = append(out.Parts, eventlog.Part{Type: eventlog.PartText, Text: p.Text})
		case *message.Blob:
			out.Parts = append(out.Parts, eventlog.Part{Type: eventlog.PartText, Text: blobText(p)})
		case *message.Reasoning:
			out.Parts = append(out.Parts, eventlog.Part{Type: eventlog.PartReasoning, Text: p.Text, ProviderData: p.ProviderData})
		case *message.ToolCall:
			names[p.CallID] = p.Name
			args := p.Arguments
			if !json.Valid(args) {
				args = nil
			}
			out.Parts = append(out.Parts, eventlog.Part{Type: eventlog.PartToolCall, CallID: p.CallID, Name: p.Name, Arguments: args})
		case *message.ToolResult:
			out.Parts = append(out.Parts, eventlog.Part{Type: eventlog.PartToolResult, CallID: p.CallID, Name: names[p.CallID],
				Text: resultText(p.SafeContent()), IsError: p.IsError})
		}
	}
	return out
}

func resultText(parts message.Parts) string {
	var out []byte
	for _, p := range parts {
		var s string
		switch p := p.(type) {
		case *message.Text:
			s = p.Text
		case *message.Blob:
			s = blobText(p)
		default:
			continue
		}
		if len(out) > 0 {
			out = append(out, '\n')
		}
		out = append(out, s...)
	}
	return string(out)
}

func blobText(b *message.Blob) string {
	return fmt.Sprintf("[%s attachment: the migration to the event log did not keep it]", cmp.Or(b.MediaType, "binary"))
}

func sortedKeys(m map[string][]byte) []string { return slices.Sorted(maps.Keys(m)) }
