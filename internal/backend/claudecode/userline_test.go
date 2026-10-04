package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
)

func TestUserLineSendsAnAttachmentAsAContentBlock(t *testing.T) {
	blob := func(key string) ([]byte, error) { return []byte(key), nil }
	text := eventlog.Part{Type: eventlog.PartText, Text: "look"}
	img := eventlog.Part{Type: eventlog.PartBlob, MediaType: "image/png", BlobKey: "png"}
	pdf := eventlog.Part{Type: eventlog.PartBlob, MediaType: "application/pdf", BlobKey: "pdf"}
	for _, tc := range []struct{ name, want string }{
		{"text only", `{"type":"user","message":{"role":"user","content":"look"}}`},
		{"text then attachments", `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"look"},` +
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"cG5n"}},` +
			`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"cGRm"}}]}}`},
		{"attachments only", `{"type":"user","message":{"role":"user","content":[` +
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"cG5n"}}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := map[string][]eventlog.Part{"text only": {text}, "text then attachments": {text, img, pdf}, "attachments only": {img}}[tc.name]
			line, err := userLine(eventlog.Message{Parts: parts}, blob)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := json.Marshal(line); string(got) != tc.want {
				t.Errorf("line = %s, want %s", got, tc.want)
			}
		})
	}
}
