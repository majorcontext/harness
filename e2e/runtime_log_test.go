package e2e

import (
	"encoding/json"
	"testing"

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
		Role         string    `json:"role"`
		Parts        []logPart `json:"parts"`
		ParentCallID string    `json:"parent_call_id"`
	} `json:"message"`
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
