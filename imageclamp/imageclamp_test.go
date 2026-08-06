package imageclamp

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// baseLimits mirrors a typical adapter's caps for tests: 8000px hard cap,
// 2576px target, the many-image rule, and a 5MB byte budget.
func baseLimits() Limits {
	return Limits{
		MaxDim: 8000, TargetDim: 2576,
		ManyImageThreshold: 20, ManyImageDim: 2000,
		MaxImageBytes:      5_000_000,
		RecurseToolResults: true,
	}
}

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

// noisePNG encodes an incompressible random-noise PNG — the worst case for byte
// size, used to exercise the byte budget.
func noisePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rand.New(rand.NewSource(1)).Read(img.Pix)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

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
		{100, 8500, 2576, 30, 2576},      // tall: height hits target
		{8500, 100, 2576, 2576, 30},      // wide: width hits target
		{16000, 16000, 2576, 2576, 2576}, // square
		{1, 40000, 2576, 1, 2576},        // extreme aspect: min side floored to 1
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
		{"exactly at cap", 8000, 8000, classKeep},
		{"one dim over", 100, 8500, classDownscale},
		{"absurd single dimension", 40000, 1, classDrop},
		{"absurd area under per-dim cap", 20000, 20000, classDrop},
	}
	for _, tc := range tests {
		if got := classify(tc.w, tc.h, 8000); got != tc.want {
			t.Errorf("classify(%d,%d) [%s]: got %v, want %v", tc.w, tc.h, tc.name, got, tc.want)
		}
	}
}

// --- dimension behavior ---------------------------------------------------

func TestClampDownscalesOversizedPNGToTarget(t *testing.T) {
	orig := pngBytes(t, 100, 8500)
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/png", Data: orig})}

	out := Clamp(in, baseLimits())

	blob := out[0].Parts[0].(*message.Blob)
	w, h := decodeDims(t, blob.Data)
	if h != 2576 || w != 30 {
		t.Errorf("clamped dims = %dx%d, want 30x2576 (target 2576, aspect preserved)", w, h)
	}
	// Original input must not be mutated.
	if len(in[0].Parts[0].(*message.Blob).Data) != len(orig) {
		t.Error("input blob was mutated")
	}
}

func TestClampDownscalesOversizedJPEGKeepsFormat(t *testing.T) {
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/jpeg", Data: jpegBytes(t, 8500, 100)})}
	out := Clamp(in, baseLimits())
	blob := out[0].Parts[0].(*message.Blob)
	if blob.MediaType != "image/jpeg" {
		t.Errorf("media type = %q, want image/jpeg (source format preserved)", blob.MediaType)
	}
	if w, h := decodeDims(t, blob.Data); w > 2576 || h > 2576 {
		t.Errorf("clamped dims %dx%d exceed 2576", w, h)
	}
}

func TestClampReEncodesOversizedGIFAsPNG(t *testing.T) {
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/gif", Data: gifBytes(t, 8500, 40)})}
	out := Clamp(in, baseLimits())
	blob := out[0].Parts[0].(*message.Blob)
	if blob.MediaType != "image/png" {
		t.Errorf("media type = %q, want image/png (gif re-encoded)", blob.MediaType)
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
	if w, _ := decodeDims(t, data); w <= 8000 {
		t.Fatalf("fixture width %d not oversized", w)
	}
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/webp", Data: data})}
	out := Clamp(in, baseLimits())
	blob := out[0].Parts[0].(*message.Blob)
	if blob.MediaType != "image/png" {
		t.Errorf("media type = %q, want image/png (webp re-encoded)", blob.MediaType)
	}
	if w, h := decodeDims(t, blob.Data); w > 2576 || h > 2576 {
		t.Errorf("clamped webp dims %dx%d exceed 2576", w, h)
	}
}

func TestClampReplacesUndecodableWithPlaceholder(t *testing.T) {
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/png", Data: []byte("this is not a png")})}
	out := Clamp(in, baseLimits())
	txt, ok := out[0].Parts[0].(*message.Text)
	if !ok {
		t.Fatalf("part is %T, want *message.Text placeholder", out[0].Parts[0])
	}
	if !strings.Contains(txt.Text, "image dropped") || !strings.Contains(txt.Text, "undecodable") {
		t.Errorf("placeholder text %q missing expected wording", txt.Text)
	}
}

func TestClampDropsAbsurdDimensionAsPlaceholder(t *testing.T) {
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/png", Data: pngBytes(t, 40000, 1)})}
	out := Clamp(in, baseLimits())
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
	out := Clamp(in, baseLimits())
	blob := out[0].Parts[0].(*message.Blob)
	if !bytes.Equal(blob.Data, orig) {
		t.Error("compliant image was not passed through byte-identical")
	}
}

func TestClampLeavesNonImageBlobUntouched(t *testing.T) {
	pdf := []byte("%PDF-1.4\nfake pdf body")
	in := []message.Message{userMsg(&message.Blob{MediaType: "application/pdf", Data: pdf})}
	out := Clamp(in, baseLimits())
	blob := out[0].Parts[0].(*message.Blob)
	if blob.MediaType != "application/pdf" || !bytes.Equal(blob.Data, pdf) {
		t.Error("non-image blob was modified")
	}
}

func TestClampLeavesURLBlobUntouched(t *testing.T) {
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/png", URL: "https://example.com/x.png"})}
	out := Clamp(in, baseLimits())
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
	out := Clamp(in, baseLimits())
	gotTR := out[0].Parts[0].(*message.ToolResult)
	blob := gotTR.Content[0].(*message.Blob)
	if w, h := decodeDims(t, blob.Data); w > 2576 || h > 2576 {
		t.Errorf("nested oversized image not clamped: %dx%d", w, h)
	}
	if len(tr.Content[0].(*message.Blob).Data) == len(blob.Data) {
		t.Error("input tool-result blob appears mutated")
	}
}

func TestClampSkipsToolResultWhenRecursionOff(t *testing.T) {
	toolResultBlob := &message.Blob{MediaType: "image/png", Data: pngBytes(t, 100, 8500)}
	lim := baseLimits()
	lim.RecurseToolResults = false
	in := []message.Message{
		userMsg(&message.Blob{MediaType: "image/png", Data: pngBytes(t, 100, 8500)}),
		{ID: "m2", Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "c1", Content: message.Parts{toolResultBlob}},
		}},
	}
	out := Clamp(in, lim)
	// Top-level oversized image is downscaled...
	if w, _ := decodeDims(t, out[0].Parts[0].(*message.Blob).Data); w > 2576 {
		t.Errorf("top-level image not clamped: width %d", w)
	}
	// ...but the tool-result image is left byte-identical.
	gotBlob := out[1].Parts[0].(*message.ToolResult).Content[0].(*message.Blob)
	if !bytes.Equal(gotBlob.Data, toolResultBlob.Data) {
		t.Error("RecurseToolResults=false should not touch tool-result content")
	}
}

func TestClampReturnsInputUnchangedWhenNothingOversized(t *testing.T) {
	in := []message.Message{userMsg(
		&message.Text{Text: "hello"},
		&message.Blob{MediaType: "image/png", Data: pngBytes(t, 50, 50)},
	)}
	out := Clamp(in, baseLimits())
	if &out[0] != &in[0] {
		t.Error("Clamp allocated a new message when nothing needed clamping")
	}
}

// --- byte budget ----------------------------------------------------------

func TestClampReducesImageOverByteBudget(t *testing.T) {
	// Dimensions are well under the 8000px cap, so this is purely a byte-size
	// reduction: an incompressible PNG far over budget must be shrunk to fit.
	orig := noisePNG(t, 1200, 1000)
	budget := 1_000_000
	if base64.StdEncoding.EncodedLen(len(orig)) <= budget {
		t.Fatalf("fixture base64 %d already under budget %d", base64.StdEncoding.EncodedLen(len(orig)), budget)
	}
	lim := baseLimits()
	lim.MaxImageBytes = budget
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/png", Data: orig})}

	out := Clamp(in, lim)

	blob, ok := out[0].Parts[0].(*message.Blob)
	if !ok {
		t.Fatalf("part is %T, want a reduced *message.Blob", out[0].Parts[0])
	}
	if got := base64.StdEncoding.EncodedLen(len(blob.Data)); got > budget {
		t.Errorf("reduced image base64 %d still over budget %d", got, budget)
	}
	if _, _, err := image.Decode(bytes.NewReader(blob.Data)); err != nil {
		t.Errorf("reduced image not decodable: %v", err)
	}
	if w, h := decodeDims(t, blob.Data); w > 1200 || h > 1000 {
		t.Errorf("reduced image grew: %dx%d", w, h)
	}
}

func TestClampByteBudgetDisabledLeavesLargeImage(t *testing.T) {
	orig := noisePNG(t, 800, 800)
	lim := baseLimits()
	lim.MaxImageBytes = 0 // disabled
	in := []message.Message{userMsg(&message.Blob{MediaType: "image/png", Data: orig})}
	out := Clamp(in, lim)
	blob := out[0].Parts[0].(*message.Blob)
	if !bytes.Equal(blob.Data, orig) {
		t.Error("byte budget disabled but image was still altered")
	}
}

// --- many-image stricter cap ---------------------------------------------

func manyImages(t *testing.T, n, w, h int) message.Message {
	t.Helper()
	parts := make(message.Parts, n)
	for i := range parts {
		parts[i] = &message.Blob{MediaType: "image/png", Data: pngBytes(t, w, h)}
	}
	return userMsg(parts...)
}

func TestClampManyImagesAppliesStricterCap(t *testing.T) {
	// 21 images (> threshold 20), each 3000px — under the normal 8000 cap but
	// over the stricter 2000 many-image cap, so each must be downscaled.
	in := []message.Message{manyImages(t, 21, 3000, 100)}
	out := Clamp(in, baseLimits())
	for i, p := range out[0].Parts {
		if w, _ := decodeDims(t, p.(*message.Blob).Data); w > 2000 {
			t.Errorf("image %d width %d exceeds many-image cap 2000", i, w)
		}
	}
}

func TestClampAtOrUnderThresholdKeepsNormalCap(t *testing.T) {
	// Exactly 20 images (not > threshold): the stricter cap does NOT apply, so
	// a 3000px image (under 8000) passes through unchanged.
	in := []message.Message{manyImages(t, 20, 3000, 100)}
	out := Clamp(in, baseLimits())
	if w, h := decodeDims(t, out[0].Parts[0].(*message.Blob).Data); w != 3000 || h != 100 {
		t.Errorf("image was clamped at threshold boundary: %dx%d, want 3000x100", w, h)
	}
}

func TestClampManyImageCountExcludesToolResultsWhenNotRecursing(t *testing.T) {
	// With RecurseToolResults=false (openai/openaicompat), tool-result images
	// are omitted on the wire, so they must NOT count toward the >20 threshold.
	// Here the wire carries exactly one image (the top-level 3000px one), far
	// under the threshold, so it must not be downscaled to the 2000 cap.
	lim := baseLimits()
	lim.RecurseToolResults = false
	trContent := make(message.Parts, 21)
	for i := range trContent {
		trContent[i] = &message.Blob{MediaType: "image/png", Data: pngBytes(t, 100, 100)}
	}
	in := []message.Message{
		userMsg(&message.Blob{MediaType: "image/png", Data: pngBytes(t, 3000, 100)}),
		{ID: "m2", Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "c1", Content: trContent},
		}},
	}
	out := Clamp(in, lim)
	if w, _ := decodeDims(t, out[0].Parts[0].(*message.Blob).Data); w != 3000 {
		t.Errorf("top-level image width %d (want 3000): omitted tool-result images must not trip the many-image cap", w)
	}
}

func TestClampManyImagesCountsPDFsTowardThreshold(t *testing.T) {
	// 19 images + 2 PDFs = 21 blocks > threshold 20; on Bedrock/Vertex PDFs
	// count too, so the stricter cap applies to the images.
	parts := make(message.Parts, 0, 21)
	for i := 0; i < 19; i++ {
		parts = append(parts, &message.Blob{MediaType: "image/png", Data: pngBytes(t, 3000, 100)})
	}
	parts = append(parts,
		&message.Blob{MediaType: "application/pdf", Data: []byte("%PDF-1.4 a")},
		&message.Blob{MediaType: "application/pdf", Data: []byte("%PDF-1.4 b")},
	)
	in := []message.Message{userMsg(parts...)}
	out := Clamp(in, baseLimits())
	if w, _ := decodeDims(t, out[0].Parts[0].(*message.Blob).Data); w > 2000 {
		t.Errorf("image width %d not reduced under many-image cap when PDFs counted", w)
	}
}
