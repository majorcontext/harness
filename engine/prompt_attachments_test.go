package engine

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// tinyPNGBytes is a 1x1 PNG — real, decodable bytes, small enough to inline
// in a hand-authored journal line below.
var tinyPNGBytes = mustDecodeBase64("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

func mustDecodeBase64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func testBlob() *message.Blob {
	return &message.Blob{MediaType: "image/png", Data: tinyPNGBytes}
}

// TestPromptPartsPlacesAttachmentsAfterText pins the shape every prompt path
// shares: the typed text first, then one Blob part per attachment, in the
// caller's order.
func TestPromptPartsPlacesAttachmentsAfterText(t *testing.T) {
	a, b := testBlob(), &message.Blob{MediaType: "image/gif", Data: []byte("GIF89a")}
	parts := promptParts("look at these", []*message.Blob{a, b})
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want text + 2 blobs: %+v", len(parts), parts)
	}
	text, ok := parts[0].(*message.Text)
	if !ok || text.Text != "look at these" {
		t.Fatalf("parts[0] = %+v, want the text part first", parts[0])
	}
	if parts[1] != message.Part(a) || parts[2] != message.Part(b) {
		t.Fatalf("attachments = %+v, want them in caller order after the text", parts[1:])
	}
}

// TestPromptPartsOmitsEmptyTextWhenAttachedProves an image-only prompt
// carries no empty Text part: a leading empty text block is noise every
// transcoder would have to carry.
func TestPromptPartsOmitsEmptyTextWhenAttached(t *testing.T) {
	parts := promptParts("", []*message.Blob{testBlob()})
	if len(parts) != 1 {
		t.Fatalf("parts = %+v, want the blob alone", parts)
	}
	if _, ok := parts[0].(*message.Blob); !ok {
		t.Fatalf("parts[0] = %+v, want the blob", parts[0])
	}
}

// TestPromptPartsKeepsTextOnlyShape proves the no-attachment path is
// unchanged — one Text part, even for empty text, exactly as every caller
// predating attachments produced.
func TestPromptPartsKeepsTextOnlyShape(t *testing.T) {
	for _, text := range []string{"hello", ""} {
		parts := promptParts(text, nil)
		if len(parts) != 1 {
			t.Fatalf("parts for %q = %+v, want exactly one text part", text, parts)
		}
		got, ok := parts[0].(*message.Text)
		if !ok || got.Text != text {
			t.Fatalf("parts[0] for %q = %+v, want a Text part", text, parts[0])
		}
	}
}

// TestEnqueuePromptPersistsAttachments proves an enqueued prompt's
// attachments reach the journal on its prompt.queued record — the write half
// of surviving a restart.
func TestEnqueuePromptPersistsAttachments(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir, Model: message.ModelRef{Provider: "test", Model: "m1"}})
	if _, _, err := s.EnqueuePrompt("with a picture", "", testBlob()); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var queued *promptRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshaling session log line %q: %v", line, err)
		}
		if rec.Type == recPromptQueued {
			queued = rec.Prompt
		}
	}
	if queued == nil {
		t.Fatalf("no prompt.queued record was written; log:\n%s", data)
	}
	if len(queued.Blobs) != 1 || !bytes.Equal(queued.Blobs[0].Data, tinyPNGBytes) {
		t.Fatalf("record blobs = %+v, want the enqueued image", queued.Blobs)
	}
}

// TestEnqueuePromptAllowsAttachmentOnlyPrompt proves empty text is valid
// when an attachment carries the message — and still rejected when nothing
// does.
func TestEnqueuePromptAllowsAttachmentOnlyPrompt(t *testing.T) {
	s := NewSession(Config{SessionDir: t.TempDir(), Model: message.ModelRef{Provider: "test", Model: "m1"}})
	if _, _, err := s.EnqueuePrompt("  ", "", testBlob()); err != nil {
		t.Fatalf("attachment-only enqueue: %v, want it accepted", err)
	}
	if _, _, err := s.EnqueuePrompt("  ", ""); err != ErrEmptyPromptText {
		t.Fatalf("empty enqueue error = %v, want ErrEmptyPromptText", err)
	}
}

// TestLoadSessionRestoresQueuedAttachments is the replay half: a queued
// prompt's image survives a process restart, so a box that was rescheduled
// while a prompt waited still answers the picture that was sent.
func TestLoadSessionRestoresQueuedAttachments(t *testing.T) {
	dir := t.TempDir()
	const id = "ses_0000000000000042"
	blobJSON, err := json.Marshal([]*message.Blob{testBlob()})
	if err != nil {
		t.Fatal(err)
	}
	writeSessionLog(t, dir, id,
		`{"type":"session","id":"`+id+`","created_at":"2026-09-01T00:00:00Z"}`,
		`{"type":"model","model":"test/m1"}`,
		`{"type":"prompt.queued","prompt":{"id":1,"text":"what is this?","blobs":`+string(blobJSON)+`}}`,
	)
	s, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatal(err)
	}
	q := s.QueuedPrompts()
	if len(q) != 1 {
		t.Fatalf("queue len = %d, want 1", len(q))
	}
	if len(q[0].Blobs) != 1 || !bytes.Equal(q[0].Blobs[0].Data, tinyPNGBytes) {
		t.Fatalf("restored blobs = %+v, want the queued image", q[0].Blobs)
	}
}

// TestOperatorMessagesBlockAnnouncesAttachments proves the rendered operator
// block tells the model which numbered message an attached image belongs to.
// The block is text; the bytes ride as separate Blob parts (queuedBlobs), so
// without this marker the model would find an unexplained picture at the end.
func TestOperatorMessagesBlockAnnouncesAttachments(t *testing.T) {
	prompts := []QueuedPrompt{
		{ID: 1, Text: "plain"},
		{ID: 2, Text: "with a shot", Blobs: []*message.Blob{testBlob()}},
	}
	block := operatorMessagesBlock(prompts, operatorContextTask)
	if !bytes.Contains([]byte(block), []byte("2. with a shot\n   [1 image attachment(s) attached below]")) {
		t.Fatalf("block = %q, want the attachment marker under its own message", block)
	}
	if bytes.Contains([]byte(block), []byte("1. plain\n   [")) {
		t.Fatalf("block = %q, want no marker on the text-only message", block)
	}
	if got := queuedBlobs(prompts); len(got) != 1 {
		t.Fatalf("queuedBlobs = %d, want the one attachment", len(got))
	}
}

// TestClaudeCodeInputContentCarriesImages proves the delegated lane's stdin
// line: a text-only turn keeps its historical bare-string content, and a turn
// with an attachment sends the CLI's own content-block array with a base64
// image source (verified against the real CLI's stream-json input).
func TestClaudeCodeInputContentCarriesImages(t *testing.T) {
	if got := claudeCodeInputContent("just text", nil); got != any("just text") {
		t.Fatalf("text-only content = %#v, want the bare string", got)
	}

	line, err := json.Marshal(claudeCodeInputMessage{
		Type:    "user",
		Message: claudeCodeInputInnerMessage{Role: "user", Content: claudeCodeInputContent("what is this?", []*message.Blob{testBlob()})},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Message struct {
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      []byte `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("unmarshal input line: %v (%s)", err, line)
	}
	blocks := got.Message.Content
	if len(blocks) != 2 {
		t.Fatalf("content blocks = %d, want text + image: %s", len(blocks), line)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "what is this?" {
		t.Errorf("first block = %+v, want the text block", blocks[0])
	}
	if blocks[1].Type != "image" || blocks[1].Source.Type != "base64" || blocks[1].Source.MediaType != "image/png" {
		t.Errorf("second block = %+v, want a base64 image source", blocks[1])
	}
	if !bytes.Equal(blocks[1].Source.Data, tinyPNGBytes) {
		t.Errorf("image data = %d bytes, want the attachment's own bytes", len(blocks[1].Source.Data))
	}
}

// TestClaudeCodeInputContentSkipsPayloadlessBlob proves a blob with no
// inline data is omitted rather than sent as an empty source: the CLI's
// input protocol has no URL image source, and announcing an image the model
// cannot see is worse than sending only the text.
func TestClaudeCodeInputContentSkipsPayloadlessBlob(t *testing.T) {
	got := claudeCodeInputContent("see this", []*message.Blob{{MediaType: "image/png", URL: "https://example.com/x.png"}})
	if got != any("see this") {
		t.Fatalf("content = %#v, want the bare text with the payloadless blob dropped", got)
	}
}

// TestLastUserMessageContentReturnsAttachments proves the delegated turn
// reads back BOTH halves of the pending user message. Parts.Text() drops
// non-text parts, so reading text alone silently stripped the upload.
func TestLastUserMessageContentReturnsAttachments(t *testing.T) {
	history := []message.Message{
		{Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "earlier"}}},
		{Role: message.RoleUser, Parts: promptParts("what is this?", []*message.Blob{testBlob()})},
	}
	text, blobs := lastUserMessageContent(history)
	if text != "what is this?" {
		t.Errorf("text = %q, want the pending prompt's text", text)
	}
	if len(blobs) != 1 || !bytes.Equal(blobs[0].Data, tinyPNGBytes) {
		t.Fatalf("blobs = %+v, want the pending prompt's attachment", blobs)
	}

	if text, blobs := lastUserMessageContent(history[:1]); text != "" || blobs != nil {
		t.Errorf("assistant tail = (%q, %+v), want empty", text, blobs)
	}
}
