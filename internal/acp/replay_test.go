package acp

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"log/slog"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

type fakeReader struct{ msgs []protocol.Message }

func (f fakeReader) Snapshot() protocol.Session { return protocol.Session{} }
func (f fakeReader) Messages(_ context.Context, before uint64, limit int) (protocol.MessagePage, error) {
	n := uint64(len(f.msgs))
	return protocol.MessagePage{Messages: f.msgs, FirstSeq: 1, LastSeq: n, Total: n}, nil
}
func (f fakeReader) Inputs(context.Context) ([]protocol.QueuedInput, error) { return nil, nil }
func (f fakeReader) Blob(context.Context, string) (harness.Blob, error) {
	return harness.Blob{}, harness.ErrBlobNotFound
}
func (f fakeReader) Events(context.Context, uint64) iter.Seq2[protocol.Event, error] { return nil }

func replayHeld(sent *[]map[string]any) *held {
	return newHeld("ses_x", heldConfig{cwd: "/work", log: slog.New(slog.DiscardHandler),
		send: func(_ string, params any) error {
			b, _ := json.Marshal(params)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			*sent = append(*sent, m["update"].(map[string]any))
			return nil
		},
		toolImage: func(context.Context, string) (io.ReadCloser, error) { return nil, harness.ErrBlobNotFound }})
}

func editCallMessages() []protocol.Message {
	args := json.RawMessage(`{"path":"a.txt","old_string":"one","new_string":"two"}`)
	return []protocol.Message{
		{ID: "msg_in1", Role: protocol.RoleUser, Parts: []protocol.MessagePart{{Type: protocol.MessagePartText, Text: "edit it"}}},
		{ID: "msg_item_1", Role: protocol.RoleAssistant, Parts: []protocol.MessagePart{
			{Type: protocol.MessagePartText, Text: "editing"},
			{Type: protocol.MessagePartToolCall, CallID: "c1", Name: "edit_file", Arguments: args}}},
	}
}

func TestReplayKeepsTheDiffOfACallThatHasNoResultYet(t *testing.T) {
	var sent []map[string]any
	h := replayHeld(&sent)
	st := newPumpState(protocol.Session{})
	if err := h.replay(t.Context(), fakeReader{msgs: editCallMessages()}, st); err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, u := range sent {
		kinds = append(kinds, u["sessionUpdate"].(string))
	}
	want := []string{"user_message_chunk", "agent_message_chunk", "tool_call"}
	if len(kinds) != len(want) || kinds[0] != want[0] || kinds[1] != want[1] || kinds[2] != want[2] {
		t.Fatalf("replayed updates = %v, want %v", kinds, want)
	}
	sent = nil
	result := protocol.LogMessage{Role: protocol.RoleTool, Parts: []protocol.LogPart{{Type: protocol.LogPartToolResult, CallID: "c1", Text: "edited"}}}
	if err := h.toolResult(t.Context(), result, st); err != nil {
		t.Fatal(err)
	}
	content, _ := sent[0]["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "diff" {
		t.Errorf("tool_call_update content = %v, want the diff of the call", sent[0]["content"])
	}
}

func TestResumeSeedsTheDiffOfACallThatHasNoResultYet(t *testing.T) {
	var sent []map[string]any
	h := replayHeld(&sent)
	st := newPumpState(protocol.Session{})
	if err := h.seedDiffs(t.Context(), fakeReader{msgs: editCallMessages()}, st); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 0 || len(st.diffs["c1"]) != 1 {
		t.Errorf("sent %d updates and seeded %d diffs, want 0 and 1", len(sent), len(st.diffs["c1"]))
	}
}
