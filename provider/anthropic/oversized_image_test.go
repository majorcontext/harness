package anthropic

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// oversizedPNG builds a solid PNG whose height exceeds any provider's 8000px
// dimension cap — the shape of the full-page screenshot that wedged three
// boxes in incident 2026-07-30.
func oversizedPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 100, 8500))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// tinyPNG builds a real, compliant PNG well within any dimension cap. The
// clamp only rejects bytes that are not decodable images, so passthrough tests
// must use genuine image data rather than arbitrary bytes.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func decodeBase64PNGDims(t *testing.T, b64 string) (int, int) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode emitted image: %v", err)
	}
	return cfg.Width, cfg.Height
}

// TestTranscodePoisonedHistoryClampsOversizedImage proves the poisoned
// transcript heals at transcode: a stored history carrying an >8000px image
// (both as a top-level attachment and nested inside a tool result) produces a
// valid request whose image blocks are all within the downscale target,
// rather than the HTTP 400 that previously re-fired on every turn.
func TestTranscodePoisonedHistoryClampsOversizedImage(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{
			&message.Text{Text: "screenshot:"},
			&message.Blob{MediaType: "image/png", Data: oversizedPNG(t)},
		}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "toolu_1", Name: "screenshot"},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "toolu_1", Content: message.Parts{
				&message.Blob{MediaType: "image/png", Data: oversizedPNG(t)},
			}},
		}},
	))

	images := 0
	var walk func(blocks []apiBlock)
	walk = func(blocks []apiBlock) {
		for _, b := range blocks {
			if b.Type == "image" && b.Source != nil {
				images++
				w, h := decodeBase64PNGDims(t, b.Source.Data)
				if w > 7680 || h > 7680 {
					t.Errorf("emitted image %dx%d exceeds downscale target 7680", w, h)
				}
			}
			walk(b.Content)
		}
	}
	for _, m := range out.Messages {
		walk(m.Content)
	}
	if images != 2 {
		t.Fatalf("expected 2 clamped image blocks (top-level + tool result), found %d", images)
	}
}

// TestTranscodePoisonedHistoryDropsUndecodableImage proves the placeholder
// path also produces a valid request: an undecodable image that is the sole
// content of a tool result becomes text, so the tool_result block still
// transcodes (with text content, no image, no error) rather than shipping a
// blob the provider would reject.
func TestTranscodePoisonedHistoryDropsUndecodableImage(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{
			&message.Text{Text: "run it"},
		}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "toolu_1", Name: "screenshot"},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "toolu_1", Content: message.Parts{
				&message.Blob{MediaType: "image/png", Data: []byte("not a real png")},
			}},
		}},
	))

	// Find the tool_result block and assert its content is the text
	// placeholder, with no image block anywhere.
	var toolResult *apiBlock
	for i := range out.Messages {
		for j := range out.Messages[i].Content {
			b := &out.Messages[i].Content[j]
			if b.Type == "tool_result" {
				toolResult = b
			}
			if b.Type == "image" {
				t.Errorf("undecodable image survived as an image block: %+v", b)
			}
		}
	}
	if toolResult == nil {
		t.Fatal("no tool_result block in output")
	}
	if len(toolResult.Content) != 1 || toolResult.Content[0].Type != "text" ||
		!strings.Contains(toolResult.Content[0].Text, "image dropped") {
		t.Errorf("tool_result content = %+v, want a single text placeholder", toolResult.Content)
	}
}
