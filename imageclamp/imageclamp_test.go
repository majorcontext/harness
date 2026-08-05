package imageclamp

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

const cap8000 = 8000

// pngBytes encodes a solid-color RGBA image of the given size as PNG.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// jpegBytes encodes a solid-color image of the given size as JPEG.
func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// gifBytes encodes a small paletted image of the given size as GIF.
func gifBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White})
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatalf("gif.Encode: %v", err)
	}
	return buf.Bytes()
}

func decodeDims(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeConfig on clamp output: %v", err)
	}
	return cfg.Width, cfg.Height
}

func userMsg(parts ...message.Part) message.Message {
	return message.Message{ID: "m1", Role: message.RoleUser, Parts: parts}
}

// --- pure decision-logic tests -------------------------------------------

func TestFitWithinPreservesAspectAndHitsTarget(t *testing.T) {
	tests := []struct {
		w, h, target int
		wantW, wantH int
	}{
		{100, 8500, 7680, 90, 7680},      // tall: height hits target
		{8500, 100, 7680, 7680, 90},      // wide: width hits target
		{16000, 16000, 7680, 7680, 7680}, // square
		{1, 40000, 7680, 1, 7680},        // extreme aspect: min side floored to 1
	}
	for _, tc := range tests {
		gotW, gotH := fitWithin(tc.w, tc.h, tc.target)
		if gotW != tc.wantW || gotH != tc.wantH {
			t.Errorf("fitWithin(%d,%d,%d) = %dx%d, want %dx%d",
				tc.w, tc.h, tc.target, gotW, gotH, tc.wantW, tc.wantH)
		}
		if gotW < 1 || gotH < 1 {
			t.Errorf("fitWithin(%d,%d,%d) produced a zero dimension: %dx%d",
				tc.w, tc.h, tc.target, gotW, gotH)
		}
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		w, h int
		want dimClass
	}{
		{"within cap", 100, 100, classKeep},
		{"exactly at cap", cap8000, cap8000, classKeep},
		{"one dim over", 100, 8500, classDownscale},
		{"absurd single dimension", 40000, 1, classDrop},
		{"absurd area under per-dim cap", 20000, 20000, classDrop},
	}
	for _, tc := range tests {
		if got := classify(tc.w, tc.h, cap8000); got != tc.want {
			t.Errorf("classify(%d,%d) [%s]: got %v, want %v", tc.w, tc.h, tc.name, got, tc.want)
		}
	}
}

// --- behavior tests -------------------------------------------------------

func TestClampDownscalesOversizedPNG(t *testing.T) {
	orig := pngBytes(t, 100, 8500)
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/png", Data: orig})}

	out := Clamp(in, cap8000)

	blob, ok := out[0].Parts[0].(*message.Blob)
	if !ok {
		t.Fatalf("part is %T, want *message.Blob", out[0].Parts[0])
	}
	w, h := decodeDims(t, blob.Data)
	if w > 7680 || h > 7680 {
		t.Errorf("clamped dims %dx%d exceed 7680", w, h)
	}
	if h != 7680 {
		t.Errorf("expected long side downscaled to 7680, got height %d", h)
	}
	// Aspect ratio preserved: 100/8500 == w/h within rounding.
	if w != 90 {
		t.Errorf("aspect not preserved: width %d, want 90", w)
	}
	// Original input must not be mutated (stateless transcoding invariant).
	if in[0].Parts[0].(*message.Blob).Data == nil || len(in[0].Parts[0].(*message.Blob).Data) != len(orig) {
		t.Error("input blob was mutated")
	}
}

func TestClampDownscalesOversizedJPEGKeepsFormat(t *testing.T) {
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/jpeg", Data: jpegBytes(t, 8500, 100)})}
	out := Clamp(in, cap8000)
	blob := out[0].Parts[0].(*message.Blob)
	if blob.MediaType != "image/jpeg" {
		t.Errorf("media type = %q, want image/jpeg (source format preserved)", blob.MediaType)
	}
	if _, _, err := image.Decode(bytes.NewReader(blob.Data)); err != nil {
		t.Errorf("clamped jpeg not decodable: %v", err)
	}
	w, h := decodeDims(t, blob.Data)
	if w > 7680 || h > 7680 {
		t.Errorf("clamped dims %dx%d exceed 7680", w, h)
	}
}

func TestClampReEncodesOversizedGIFAsPNG(t *testing.T) {
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/gif", Data: gifBytes(t, 8500, 40)})}
	out := Clamp(in, cap8000)
	blob := out[0].Parts[0].(*message.Blob)
	if blob.MediaType != "image/png" {
		t.Errorf("media type = %q, want image/png (gif re-encoded as png)", blob.MediaType)
	}
	if _, format, err := image.Decode(bytes.NewReader(blob.Data)); err != nil || format != "png" {
		t.Errorf("clamped gif not valid png: format=%q err=%v", format, err)
	}
}

func TestClampDownscalesOversizedWebP(t *testing.T) {
	data, err := os.ReadFile("testdata/oversized_8500x64.webp")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// Sanity: fixture really is oversized.
	if w, _ := decodeDims(t, data); w <= cap8000 {
		t.Fatalf("fixture width %d not oversized", w)
	}
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/webp", Data: data})}
	out := Clamp(in, cap8000)
	blob := out[0].Parts[0].(*message.Blob)
	// No webp encoder in x/image, so webp is re-encoded as png.
	if blob.MediaType != "image/png" {
		t.Errorf("media type = %q, want image/png (webp re-encoded)", blob.MediaType)
	}
	w, h := decodeDims(t, blob.Data)
	if w > 7680 || h > 7680 {
		t.Errorf("clamped webp dims %dx%d exceed 7680", w, h)
	}
}

func TestClampReplacesUndecodableWithPlaceholder(t *testing.T) {
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/png", Data: []byte("this is not a png")})}
	out := Clamp(in, cap8000)
	txt, ok := out[0].Parts[0].(*message.Text)
	if !ok {
		t.Fatalf("part is %T, want *message.Text placeholder", out[0].Parts[0])
	}
	if !strings.Contains(txt.Text, "image dropped") || !strings.Contains(txt.Text, "undecodable") {
		t.Errorf("placeholder text %q missing expected wording", txt.Text)
	}
}

func TestClampDropsAbsurdDimensionAsPlaceholder(t *testing.T) {
	// 40000x1 is tiny to build but its declared dimension exceeds the absurd
	// per-dimension guard, so pixels must never be decoded.
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/png", Data: pngBytes(t, 40000, 1)})}
	out := Clamp(in, cap8000)
	txt, ok := out[0].Parts[0].(*message.Text)
	if !ok {
		t.Fatalf("part is %T, want *message.Text placeholder", out[0].Parts[0])
	}
	if !strings.Contains(txt.Text, "image dropped") || !strings.Contains(txt.Text, "40000") {
		t.Errorf("placeholder text %q missing dimensions", txt.Text)
	}
}

func TestClampPassesCompliantImageByteIdentical(t *testing.T) {
	orig := pngBytes(t, 100, 100)
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/png", Data: orig})}
	out := Clamp(in, cap8000)
	blob, ok := out[0].Parts[0].(*message.Blob)
	if !ok {
		t.Fatalf("part is %T, want *message.Blob", out[0].Parts[0])
	}
	if !bytes.Equal(blob.Data, orig) {
		t.Error("compliant image was not passed through byte-identical")
	}
}

func TestClampLeavesNonImageBlobUntouched(t *testing.T) {
	pdf := []byte("%PDF-1.4\nfake pdf body")
	in := []message.Message{userMsg(&message.Blob{MediaType: "application/pdf", Data: pdf})}
	out := Clamp(in, cap8000)
	blob, ok := out[0].Parts[0].(*message.Blob)
	if !ok {
		t.Fatalf("part is %T, want *message.Blob", out[0].Parts[0])
	}
	if blob.MediaType != "application/pdf" || !bytes.Equal(blob.Data, pdf) {
		t.Error("non-image blob was modified")
	}
}

func TestClampLeavesURLBlobUntouched(t *testing.T) {
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/png", URL: "https://example.com/x.png"})}
	out := Clamp(in, cap8000)
	blob := out[0].Parts[0].(*message.Blob)
	if blob.URL != "https://example.com/x.png" || blob.Data != nil {
		t.Error("URL-referenced blob was modified")
	}
}

func TestClampRecursesIntoToolResult(t *testing.T) {
	tr := &message.ToolResult{
		CallID:  "call_1",
		Content: message.Parts{&message.Blob{MediaType: "image/png", Data: pngBytes(t, 100, 8500)}},
	}
	in := []message.Message{{ID: "m1", Role: message.RoleTool, Parts: message.Parts{tr}}}
	out := Clamp(in, cap8000)
	gotTR, ok := out[0].Parts[0].(*message.ToolResult)
	if !ok {
		t.Fatalf("part is %T, want *message.ToolResult", out[0].Parts[0])
	}
	blob, ok := gotTR.Content[0].(*message.Blob)
	if !ok {
		t.Fatalf("nested part is %T, want *message.Blob", gotTR.Content[0])
	}
	w, h := decodeDims(t, blob.Data)
	if w > 7680 || h > 7680 {
		t.Errorf("nested oversized image not clamped: %dx%d", w, h)
	}
	// Original tool result must be untouched.
	if len(tr.Content[0].(*message.Blob).Data) == len(blob.Data) {
		t.Error("input tool-result blob appears mutated")
	}
}

func TestClampTopLevelClampsTopLevelButSkipsToolResult(t *testing.T) {
	toolResultBlob := &message.Blob{MediaType: "image/png", Data: pngBytes(t, 100, 8500)}
	in := []message.Message{
		userMsg(&message.Blob{MediaType: "image/png", Data: pngBytes(t, 100, 8500)}),
		{ID: "m2", Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "c1", Content: message.Parts{toolResultBlob}},
		}},
	}

	out := ClampTopLevel(in, cap8000)

	// Top-level oversized image is still downscaled.
	top := out[0].Parts[0].(*message.Blob)
	if w, _ := decodeDims(t, top.Data); w > 7680 {
		t.Errorf("top-level image not clamped: width %d", w)
	}
	// Tool-result image is left byte-identical — those adapters omit it on the
	// wire, so clamping it would be wasted work.
	gotTR := out[1].Parts[0].(*message.ToolResult)
	if !bytes.Equal(gotTR.Content[0].(*message.Blob).Data, toolResultBlob.Data) {
		t.Error("ClampTopLevel should not touch tool-result content")
	}
}

func TestClampReturnsInputUnchangedWhenNothingOversized(t *testing.T) {
	in := []message.Message{userMsg(
		&message.Text{Text: "hello"},
		&message.Blob{MediaType: "image/png", Data: pngBytes(t, 50, 50)},
	)}
	out := Clamp(in, cap8000)
	// Copy-on-write: same backing slice when no change was needed.
	if &out[0] != &in[0] {
		t.Error("Clamp allocated a new message when nothing needed clamping")
	}
}
