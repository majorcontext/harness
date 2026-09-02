package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// testPNG returns the bytes of a small, structurally valid PNG — the
// stand-in for a console upload in every test below. Real bytes, not a
// fabricated prefix: the handler validates that a blob's data actually
// decodes as the image type it claims, so a fixture that only carried PNG
// magic bytes would be rejected for the wrong reason and prove nothing.
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testJPEG returns a small, valid JPEG — used to prove a REAL image of the
// wrong type is rejected, which is a different branch of verifyImageBytes
// than bytes that do not decode at all.
func testJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			img.Set(x, y, color.RGBA{B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testPDF returns a small, structurally valid PDF carrying one line of text
// — the document counterpart of testPNG. Real bytes for the same reason: the
// handler proves an attachment is the type it claims.
func testPDF(t *testing.T) []byte {
	t.Helper()
	content := []byte("BT /F1 24 Tf 72 700 Td (attachment test) Tj ET")
	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
		fmt.Appendf(nil, "<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, 0, len(objs))
	for i, o := range objs {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return out.Bytes()
}

// capturingProvider is scriptedProvider plus a copy of every request it was
// asked to stream, so a test can assert what the model actually RECEIVED —
// the only oracle that proves an uploaded image reached the provider rather
// than merely landing in the transcript.
type capturingProvider struct {
	scripted *scriptedProvider
	mu       sync.Mutex
	requests []*provider.Request
}

func newCapturingProvider(turns ...[]provider.Event) *capturingProvider {
	return &capturingProvider{scripted: &scriptedProvider{name: "test", turns: turns}}
}

func (p *capturingProvider) Name() string { return p.scripted.Name() }

func (p *capturingProvider) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return p.scripted.Stream(ctx, req)
}

// lastUserParts returns the parts of the last RoleUser message in the last
// request this provider was asked to stream.
func (p *capturingProvider) lastUserParts(t *testing.T) message.Parts {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		t.Fatal("provider was never called")
	}
	msgs := p.requests[len(p.requests)-1].Messages
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.RoleUser {
			return msgs[i].Parts
		}
	}
	t.Fatal("no user message in the provider request")
	return nil
}

// blobParts filters ps down to its Blob parts, in order.
func blobParts(ps message.Parts) []*message.Blob {
	var blobs []*message.Blob
	for _, p := range ps {
		if b, ok := p.(*message.Blob); ok {
			blobs = append(blobs, b)
		}
	}
	return blobs
}

// attachmentPart builds one prompt_async blob part body for data.
func attachmentPart(mediaType string, data []byte) map[string]any {
	return map[string]any{
		"type":       "blob",
		"media_type": mediaType,
		"data":       base64.StdEncoding.EncodeToString(data),
	}
}

// TestPromptAsyncAcceptsImageBlobPart is the RED test for the feature: a
// prompt_async body carrying a text part AND an image blob part is accepted,
// the blob survives into the durable transcript as a Blob part of the user
// message, and the provider request for that turn carries it too. Before
// this change the handler rejected every non-text part with 400 "v1 accepts
// text parts only".
func TestPromptAsyncAcceptsImageBlobPart(t *testing.T) {
	prov := newCapturingProvider(asstTurn("red"))
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	pngBytes := testPNG(t)

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []any{
			map[string]any{"type": "text", "text": "what color is this?"},
			attachmentPart("image/png", pngBytes),
		},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	users := h.userMessages(id)
	if len(users) != 1 {
		t.Fatalf("user messages = %d, want 1: %+v", len(users), users)
	}
	if got := users[0].Parts.Text(); got != "what color is this?" {
		t.Errorf("transcript user text = %q, want the prompt text", got)
	}
	blobs := blobParts(users[0].Parts)
	if len(blobs) != 1 {
		t.Fatalf("transcript user blobs = %d, want 1 (parts %+v)", len(blobs), users[0].Parts)
	}
	if blobs[0].MediaType != "image/png" {
		t.Errorf("blob media type = %q, want image/png", blobs[0].MediaType)
	}
	if !bytes.Equal(blobs[0].Data, pngBytes) {
		t.Errorf("blob data = %d bytes, want the %d uploaded bytes", len(blobs[0].Data), len(pngBytes))
	}

	sent := blobParts(prov.lastUserParts(t))
	if len(sent) != 1 || !bytes.Equal(sent[0].Data, pngBytes) {
		t.Fatalf("provider request carried %d blob parts, want the uploaded image", len(sent))
	}
}

// TestPromptAsyncAcceptsImageOnlyPrompt proves an attachment-only prompt (no
// text part at all) is accepted: a person can send a screenshot with nothing
// to say about it. The old handler joined text parts and would have run a
// turn on an empty string.
func TestPromptAsyncAcceptsImageOnlyPrompt(t *testing.T) {
	prov := newCapturingProvider(asstTurn("ok"))
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	pngBytes := testPNG(t)

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []any{attachmentPart("image/png", pngBytes)},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	users := h.userMessages(id)
	if len(users) != 1 {
		t.Fatalf("user messages = %d, want 1", len(users))
	}
	if len(blobParts(users[0].Parts)) != 1 {
		t.Fatalf("user parts = %+v, want exactly the image blob", users[0].Parts)
	}
	if got := users[0].Parts.Text(); got != "" {
		t.Errorf("user text = %q, want empty for an image-only prompt", got)
	}
}

// TestPromptAsyncAcceptsPDFAttachment proves the contract is attachments,
// not images: a PDF is accepted, kept whole in the durable transcript, and
// handed to the provider as its own blob part. The claude-code lane sends it
// as a document block (claudeCodeInputContent); the native adapters already
// transcode a non-image blob as a document/input_file.
func TestPromptAsyncAcceptsPDFAttachment(t *testing.T) {
	prov := newCapturingProvider(asstTurn("read it"))
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	pdfBytes := testPDF(t)

	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []any{
			map[string]any{"type": "text", "text": "what does this say?"},
			attachmentPart("application/pdf", pdfBytes),
		},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	h.waitIdle(id)

	users := h.userMessages(id)
	if len(users) != 1 {
		t.Fatalf("user messages = %d, want 1", len(users))
	}
	blobs := blobParts(users[0].Parts)
	if len(blobs) != 1 || blobs[0].MediaType != "application/pdf" {
		t.Fatalf("transcript blobs = %+v, want the pdf", blobs)
	}
	if !bytes.Equal(blobs[0].Data, pdfBytes) {
		t.Errorf("blob data = %d bytes, want the %d uploaded bytes", len(blobs[0].Data), len(pdfBytes))
	}
	sent := blobParts(prov.lastUserParts(t))
	if len(sent) != 1 || !bytes.Equal(sent[0].Data, pdfBytes) {
		t.Fatalf("provider request carried %d blob parts, want the pdf", len(sent))
	}
}

// TestPromptAsyncRejectsUnusableBlob proves each rejection the handler owns,
// and proves it rejects BEFORE anything runs: no user message is appended
// and the provider is never called. A blob the provider would reject later
// must fail here, synchronously, where the caller can still fix it — the
// wedge imageclamp exists to heal (an oversized image persisted into a
// durable transcript) starts exactly one accepted-but-unusable blob ago.
func TestPromptAsyncRejectsUnusableBlob(t *testing.T) {
	cases := []struct {
		name string
		part map[string]any
		want string
	}{
		{
			name: "unsupported media type",
			part: attachmentPart("application/zip", []byte("PK\x03\x04not a zip")),
			want: "unsupported blob media type",
		},
		{
			name: "document that is not a pdf",
			part: attachmentPart("application/pdf", []byte("just text, no header")),
			want: "%PDF- header",
		},
		{
			name: "data does not decode as the claimed type",
			part: attachmentPart("image/png", []byte("this is plain text, not a PNG")),
			want: "does not decode",
		},
		{
			// A REAL image, of the wrong type: this reaches
			// verifyImageBytes' format comparison rather than its decode
			// failure above, and the message must name what the data
			// actually is in the SAME units the caller claimed it in
			// ("image/jpeg", not the bare decoder format "jpeg") — the
			// comparison is between two media types, so a caller should
			// not have to guess that "jpeg" meant image/jpeg.
			name: "a real image of a different type than claimed",
			part: attachmentPart("image/png", testJPEG(t)),
			want: "the data is image/jpeg",
		},
		{
			name: "no data and no url",
			part: map[string]any{"type": "blob", "media_type": "image/png"},
			want: "neither data nor url",
		},
		{
			name: "url blob",
			part: map[string]any{"type": "blob", "media_type": "image/png", "url": "https://example.com/x.png"},
			want: "inline data",
		},
		{
			name: "unknown part type",
			part: map[string]any{"type": "reasoning", "text": "no"},
			want: "text and blob parts only",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := newCapturingProvider(asstTurn("never"))
			h := newHarness(t, prov)
			id := h.createSession("test/m1")

			resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
				"parts": []any{
					map[string]any{"type": "text", "text": "look"},
					tc.part,
				},
			})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("prompt_async status %d, want 400: %s", resp.StatusCode, data)
			}
			var errBody struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(data, &errBody); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains([]byte(errBody.Error), []byte(tc.want)) {
				t.Errorf("error = %q, want it to mention %q", errBody.Error, tc.want)
			}
			if users := h.userMessages(id); len(users) != 0 {
				t.Errorf("user messages = %d, want none: a rejected prompt must append nothing", len(users))
			}
			prov.mu.Lock()
			calls := len(prov.requests)
			prov.mu.Unlock()
			if calls != 0 {
				t.Errorf("provider calls = %d, want 0", calls)
			}
		})
	}
}

// TestPromptAsyncOversizeBlobRejected proves the per-blob byte cap: a blob
// past promptAttachmentMaxBytes is refused with a message naming the limit, so a
// caller learns to downscale instead of silently poisoning the session.
func TestPromptAsyncOversizeBlobRejected(t *testing.T) {
	prov := newCapturingProvider(asstTurn("never"))
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	// A valid PNG header followed by enough bytes to pass the cap. The size
	// check must run BEFORE the decode, so the padding never has to be a
	// real image.
	oversize := append(testPNG(t), bytes.Repeat([]byte("x"), promptAttachmentMaxBytes)...)
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []any{attachmentPart("image/png", oversize)},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("prompt_async status %d, want 400: %s", resp.StatusCode, data)
	}
	if !bytes.Contains(data, []byte("exceeds")) {
		t.Errorf("error = %s, want it to name the size limit", data)
	}
}

// TestPromptAsyncOversizeBodyRejectedBeforeDecode proves the WHOLE-body
// bound, which is a different guarantee from the per-blob cap above.
//
// promptAttachmentMaxBytes is checked per attachment, but only after
// encoding/json has already base64-decoded that attachment into a []byte,
// and nothing caps how many attachments one body may carry -- so without
// this bound a caller could make the server allocate an unbounded amount
// before the first size check ran, then reject what it had already paid
// for. http.MaxBytesReader stops the read at promptRequestMaxBytes, so the
// body below is never fully decoded and the answer is 413, not 400.
func TestPromptAsyncOversizeBodyRejectedBeforeDecode(t *testing.T) {
	prov := newCapturingProvider(asstTurn("never"))
	h := newHarness(t, prov)
	id := h.createSession("test/m1")

	// Two attachments, each UNDER the per-attachment cap, that together
	// exceed the request bound: exactly the case the per-blob check alone
	// cannot catch.
	half := append(testPNG(t), bytes.Repeat([]byte("x"), promptRequestMaxBytes*2/3)...)
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []any{
			attachmentPart("image/png", half),
			attachmentPart("image/png", half),
		},
	})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("prompt_async status %d, want 413: %s", resp.StatusCode, data)
	}
	if !bytes.Contains(data, []byte("limit")) {
		t.Errorf("error = %s, want it to name the request limit", data)
	}
	if users := h.userMessages(id); len(users) != 0 {
		t.Errorf("user messages = %d, want none", len(users))
	}
}

// TestQueuedPromptKeepsItsImage is the durability half of the feature: an
// image sent while the session is BUSY is queued, and the queued prompt
// still carries its blob when the queue drains at the turn boundary. A queue
// that dropped attachments would lose the upload silently — the exact
// failure the text-only v1 queue contract used to guarantee.
func TestQueuedPromptKeepsItsImage(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	id := h.createSession("test/m1")
	pngBytes := testPNG(t)

	// First prompt claims the run slot and parks inside the provider.
	resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []any{map[string]any{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first prompt status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []any{
			map[string]any{"type": "text", "text": "and this screenshot"},
			attachmentPart("image/png", pngBytes),
		},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("queued prompt status %d: %s", resp.StatusCode, data)
	}
	var pr promptAsyncResponse
	if err := json.Unmarshal(data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Status != "queued" {
		t.Fatalf("status = %q, want queued", pr.Status)
	}

	prov.releaseAll()
	h.waitIdle(id)

	users := h.userMessages(id)
	var withBlob int
	for _, m := range users {
		for _, b := range blobParts(m.Parts) {
			if bytes.Equal(b.Data, pngBytes) {
				withBlob++
			}
		}
	}
	if withBlob != 1 {
		t.Fatalf("user messages carrying the queued image = %d, want 1: %+v", withBlob, users)
	}
}
