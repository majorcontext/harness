package engine

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	if !bytes.Contains([]byte(block), []byte("2. with a shot\n   [1 attachment(s) attached below]")) {
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

// TestClaudeCodeInputContentSendsDocumentBlockForNonImage proves the block
// TYPE follows the media type, matching the native anthropic adapter: a PDF
// is a "document" block, not an "image" one. Verified against the real CLI,
// which read a PDF's text back through this same stream-json shape.
func TestClaudeCodeInputContentSendsDocumentBlockForNonImage(t *testing.T) {
	pdf := &message.Blob{MediaType: "application/pdf", Data: []byte("%PDF-1.4 fake body")}
	content := claudeCodeInputContent("read this", []*message.Blob{pdf, testBlob()})
	blocks, ok := content.([]claudeCodeInputBlock)
	if !ok {
		t.Fatalf("content = %#v, want a content-block array", content)
	}
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want text + document + image", len(blocks))
	}
	if blocks[1].Type != "document" || blocks[1].Source.MediaType != "application/pdf" {
		t.Errorf("second block = %+v, want a pdf document block", blocks[1])
	}
	if blocks[2].Type != "image" || blocks[2].Source.MediaType != "image/png" {
		t.Errorf("third block = %+v, want an image block", blocks[2])
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

// TestEnqueuePromptDropsUnusableBlobs: a blob that cannot be delivered must
// not make an empty prompt valid, and must not be counted in the operator
// block's attachment marker.
//
// EnqueuePrompt used to check len(blobs) directly, so a caller passing a
// single nil satisfied "empty text is fine when a blob came with it". The
// queued prompt then persisted an unusable Blobs slice, and
// operatorMessagesBlock announced "[1 attachment(s) attached below]" to the
// model while promptParts skipped the nil on delivery — the marker
// promising a file that never arrives, the same defect the claude-code
// mid-turn drain had.
func TestEnqueuePromptDropsUnusableBlobs(t *testing.T) {
	s := NewSession(Config{SessionDir: t.TempDir()})

	// Empty text plus only unusable blobs is an EMPTY prompt.
	for _, blobs := range [][]*message.Blob{
		{nil},
		{{MediaType: "image/png"}},            // neither Data nor URL
		{nil, {MediaType: "application/pdf"}}, // several, all unusable
	} {
		if _, _, err := s.EnqueuePrompt("", "", blobs...); !errors.Is(err, ErrEmptyPromptText) {
			t.Errorf("EnqueuePrompt(%v) error = %v, want ErrEmptyPromptText", blobs, err)
		}
	}
	if q := s.QueuedPrompts(); len(q) != 0 {
		t.Fatalf("queue = %+v, want nothing enqueued", q)
	}

	// A real blob beside an unusable one enqueues, carrying only the real
	// one — so the marker counts what will actually be delivered.
	real := &message.Blob{MediaType: "image/png", Data: []byte("\x89PNG\r\n\x1a\n")}
	if _, _, err := s.EnqueuePrompt("look", "", nil, real); err != nil {
		t.Fatalf("EnqueuePrompt: %v", err)
	}
	q := s.QueuedPrompts()
	if len(q) != 1 {
		t.Fatalf("queue = %d prompts, want 1", len(q))
	}
	if len(q[0].Blobs) != 1 || q[0].Blobs[0] != real {
		t.Errorf("queued blobs = %+v, want only the deliverable one", q[0].Blobs)
	}
	if block := operatorMessagesBlock(q, operatorContextTask); !strings.Contains(block, "[1 attachment(s) attached below]") {
		t.Errorf("operator block = %q, want it to count ONE attachment", block)
	}
}

// TestEnqueuePromptDurablePersistsAttachments is EnqueuePromptDurable's
// counterpart to TestEnqueuePromptPersistsAttachments: the durable,
// caller-seq-idempotent primitive POST /session/{id}/enqueue calls
// (docs/plans/2026-07-21-durable-enqueue.md) must carry a blob onto its own
// prompt.queued record exactly like the plain queue already does — the
// write half of a box's enqueued screenshot surviving a restart.
func TestEnqueuePromptDurablePersistsAttachments(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir, Model: message.ModelRef{Provider: "test", Model: "m1"}})
	if _, dup, err := s.EnqueuePromptDurable("with a picture", 1, testBlob()); err != nil || dup {
		t.Fatalf("EnqueuePromptDurable: dup=%v err=%v", dup, err)
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
	if queued.Seq != 1 {
		t.Errorf("record seq = %d, want 1 (the caller's idempotency seq must ride with the blob)", queued.Seq)
	}
	if len(queued.Blobs) != 1 || !bytes.Equal(queued.Blobs[0].Data, tinyPNGBytes) {
		t.Fatalf("record blobs = %+v, want the enqueued image", queued.Blobs)
	}

	q := s.QueuedPrompts()
	if len(q) != 1 || len(q[0].Blobs) != 1 || !bytes.Equal(q[0].Blobs[0].Data, tinyPNGBytes) {
		t.Fatalf("in-memory queue = %+v, want the same attachment", q)
	}
}

// TestEnqueuePromptDurableAllowsAttachmentOnlyPrompt mirrors
// TestEnqueuePromptAllowsAttachmentOnlyPrompt for the durable path: an
// uploaded screenshot with nothing typed beside it is a real prompt, not an
// empty one, whether it arrives via the best-effort queue or the durable one.
func TestEnqueuePromptDurableAllowsAttachmentOnlyPrompt(t *testing.T) {
	s := NewSession(Config{SessionDir: t.TempDir(), Model: message.ModelRef{Provider: "test", Model: "m1"}})
	if _, dup, err := s.EnqueuePromptDurable("  ", 1, testBlob()); err != nil || dup {
		t.Fatalf("attachment-only durable enqueue: dup=%v err=%v, want it accepted", dup, err)
	}
	if _, _, err := s.EnqueuePromptDurable("  ", 2); err != ErrEmptyPromptText {
		t.Fatalf("empty durable enqueue error = %v, want ErrEmptyPromptText", err)
	}
}

// TestEnqueuePromptDurableDropsUnusableBlobs mirrors
// TestEnqueuePromptDropsUnusableBlobs for the durable path: a blob nothing
// can deliver (nil, or carrying neither Data nor URL) must not make an
// otherwise-empty prompt valid, and must not survive into the persisted
// record — the same wedge usablePromptBlobs exists to prevent for the plain
// queue.
func TestEnqueuePromptDurableDropsUnusableBlobs(t *testing.T) {
	s := NewSession(Config{SessionDir: t.TempDir()})

	if _, _, err := s.EnqueuePromptDurable("", 1, nil, &message.Blob{MediaType: "image/png"}); !errors.Is(err, ErrEmptyPromptText) {
		t.Fatalf("EnqueuePromptDurable with only unusable blobs: err = %v, want ErrEmptyPromptText", err)
	}
	if q := s.QueuedPrompts(); len(q) != 0 {
		t.Fatalf("queue = %+v, want nothing enqueued", q)
	}

	real := &message.Blob{MediaType: "image/png", Data: []byte("\x89PNG\r\n\x1a\n")}
	if _, dup, err := s.EnqueuePromptDurable("look", 1, nil, real); err != nil || dup {
		t.Fatalf("EnqueuePromptDurable: dup=%v err=%v", dup, err)
	}
	q := s.QueuedPrompts()
	if len(q) != 1 || len(q[0].Blobs) != 1 || q[0].Blobs[0] != real {
		t.Fatalf("queued blobs = %+v, want only the deliverable one", q)
	}
}
