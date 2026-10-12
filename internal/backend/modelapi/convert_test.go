package modelapi

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"

	"github.com/majorcontext/harness/protocol"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/turn"
)

func TestRequestMapsABlobPartToItsBytes(t *testing.T) {
	blobs := map[string][]byte{"k": []byte("png")}
	read := func(_ context.Context, key string) ([]byte, error) {
		if b, ok := blobs[key]; ok {
			return b, nil
		}
		return nil, errors.New("no blob " + key)
	}
	history := []protocol.LogMessage{{Role: protocol.RoleUser, Parts: []protocol.LogPart{
		{Type: protocol.LogPartText, Text: "look"}, {Type: protocol.LogPartBlob, MediaType: "image/png", BlobKey: "k", Bytes: 3}}}}
	got, err := (&Backend{log: slog.Default()}).request(context.Background(), turn.Request{Model: "codex/gpt-5", History: history, Blob: read})
	if err != nil {
		t.Fatal(err)
	}
	want := message.Parts{&message.Text{Text: "look"}, &message.Blob{MediaType: "image/png", Data: []byte("png")}}
	if !reflect.DeepEqual(got.Messages[0].Parts, want) {
		t.Errorf("parts = %+v, want %+v", got.Messages[0].Parts, want)
	}
	history[0].Parts[1].BlobKey = "gone"
	if _, err := (&Backend{log: slog.Default()}).request(context.Background(), turn.Request{Model: "codex/gpt-5", History: history, Blob: read}); err == nil {
		t.Error("request with a missing blob = nil, want the read error")
	}
}

func TestFromMessageDropsToolArgumentsThatAreNotJSON(t *testing.T) {
	m := fromMessage(&message.Message{Role: message.RoleAssistant, Parts: message.Parts{
		&message.ToolCall{CallID: "c1", Name: "x", Arguments: []byte(`{"a":"b`)}}})
	if got := m.Parts[0].Arguments; got != nil {
		t.Errorf("arguments = %s, want none", got)
	}
}
