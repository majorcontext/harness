package openaicompat

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// oversizedPNG builds a PNG whose height exceeds the 8000px provider cap.
func oversizedPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 100, 8500))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// tinyPNG builds a real, compliant PNG within any cap. The clamp drops
// non-decodable image bytes, so passthrough tests need genuine image data.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func dataURLDims(t *testing.T, dataURL string) (int, int) {
	t.Helper()
	i := strings.Index(dataURL, ";base64,")
	if i < 0 {
		t.Fatalf("not a base64 data URL")
	}
	raw, err := base64.StdEncoding.DecodeString(dataURL[i+len(";base64,"):])
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode emitted image: %v", err)
	}
	return cfg.Width, cfg.Height
}

// TestTranscodePoisonedHistoryClampsOversizedImage proves an oversized inline
// image in a stored history is downscaled at transcode time so the emitted
// chat-completions request is within the dimension cap.
func TestTranscodePoisonedHistoryClampsOversizedImage(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{
			&message.Text{Text: "what is this"},
			&message.Blob{MediaType: "image/png", Data: oversizedPNG(t)},
		}},
	))

	last := out.Messages[len(out.Messages)-1]
	p := probeMessage(t, marshalRaw(t, &last))
	contentRaw, err := json.Marshal(p.Content)
	if err != nil {
		t.Fatal(err)
	}
	parts := contentParts(t, contentRaw)
	found := false
	for _, p := range parts {
		if p.Type == "image_url" && strings.HasPrefix(p.ImageURL.URL, "data:image/") {
			found = true
			w, h := dataURLDims(t, p.ImageURL.URL)
			if w > 7680 || h > 7680 {
				t.Errorf("emitted image %dx%d exceeds downscale target 7680", w, h)
			}
		}
	}
	if !found {
		t.Fatal("no inline image in transcoded output")
	}
}
