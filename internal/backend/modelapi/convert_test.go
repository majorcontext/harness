package modelapi

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/message"
)

func TestRequestMapsABlobPartToItsBytes(t *testing.T) {
	blobs := map[string][]byte{"k": []byte("png")}
	read := func(_ context.Context, key string) ([]byte, error) {
		if b, ok := blobs[key]; ok {
			return b, nil
		}
		return nil, errors.New("no blob " + key)
	}
	history := []eventlog.Message{{Role: eventlog.RoleUser, Parts: []eventlog.Part{
		{Type: eventlog.PartText, Text: "look"}, {Type: eventlog.PartBlob, MediaType: "image/png", BlobKey: "k", Bytes: 3}}}}
	got, err := request(context.Background(), turn.Request{Model: "codex/gpt-5", History: history, Blob: read})
	if err != nil {
		t.Fatal(err)
	}
	want := message.Parts{&message.Text{Text: "look"}, &message.Blob{MediaType: "image/png", Data: []byte("png")}}
	if !reflect.DeepEqual(got.Messages[0].Parts, want) {
		t.Errorf("parts = %+v, want %+v", got.Messages[0].Parts, want)
	}
	history[0].Parts[1].BlobKey = "gone"
	if _, err := request(context.Background(), turn.Request{Model: "codex/gpt-5", History: history, Blob: read}); err == nil {
		t.Error("request with a missing blob = nil, want the read error")
	}
}
