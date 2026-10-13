package plugin_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/plugin"
)

func sampleMessage(t *testing.T) message.Message {
	t.Helper()
	ref, err := modelref.ParseModelRef("anthropic/claude-sonnet")
	if err != nil {
		t.Fatal(err)
	}
	return message.Message{
		ID:   "m1",
		Role: message.RoleAssistant,
		Parts: message.Parts{
			&message.Text{Text: "hello"},
			&message.Blob{MediaType: "image/png", Data: []byte{0, 1, 2, 255}},
			&message.Blob{MediaType: "image/png", URL: "https://example.test/a.png"},
			&message.Reasoning{Text: "think", ProviderData: message.ProviderData{"anthropic": json.RawMessage(`{"signature":"abc"}`)}},
			&message.ToolCall{CallID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
			&message.ToolResult{CallID: "c1", Content: message.Parts{&message.Text{Text: "out"}}, IsError: true},
		},
		Model:           ref,
		CreatedAt:       time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC),
		Origin:          "summary",
		ParentToolUseID: "t1",
	}
}

func TestPartJSONEqualsMessagePartsJSON(t *testing.T) {
	m := sampleMessage(t)
	want, err := json.Marshal(m.Parts)
	if err != nil {
		t.Fatal(err)
	}
	var parts []plugin.Part
	if err := json.Unmarshal(want, &parts); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("plugin parts = %s\nwant %s", got, want)
	}
	var back message.Parts
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, want) {
		t.Errorf("message parts = %s\nwant %s", again, want)
	}
}

func TestPartTypesTextAndBlob(t *testing.T) {
	var parts []plugin.Part
	raw := `[{"type":"text","text":"hi"},{"type":"blob","media_type":"image/png","data":"AAEC"}]`
	if err := json.Unmarshal([]byte(raw), &parts); err != nil {
		t.Fatal(err)
	}
	if parts[0].Text != "hi" || parts[1].MediaType != "image/png" || !bytes.Equal(parts[1].Data, []byte{0, 1, 2}) {
		t.Errorf("parts = %+v", parts)
	}
}

func TestMessageJSONEqualsMessageJSON(t *testing.T) {
	msgs := []message.Message{sampleMessage(t), {ID: "m2", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "x"}}}}
	want, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	var got []plugin.Message
	if err := json.Unmarshal(want, &got); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Errorf("plugin messages = %s\nwant %s", out, want)
	}
}
