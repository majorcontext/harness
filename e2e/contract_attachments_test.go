package e2e

import (
	"encoding/base64"
	"errors"
	"net/http"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// onePixelPNG is a 1x1 PNG.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// rowAttachments are the files that submitAttachments sends: an image and a PDF.
func rowAttachments() []attachment {
	png, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	return []attachment{{"image/png", png}, {"application/pdf", []byte("%PDF-1.4\n%%EOF\n")}}
}

func TestContractAttachments(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:    "bifrost_prompt_attachments",
			chat:    true,
			model:   []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: "seen"}}},
			actions: []action{create{as: "a"}, submitAttachments{as: "a", text: "look"}, waitIdle{as: "a"}},
		},
		{
			name:    "claudecode_prompt_attachments",
			driver:  claudeLaneDriver("normal"),
			actions: []action{create{as: "a"}, submitAttachments{as: "a", text: "look"}, waitIdle{as: "a"}, claudeInputs{as: "a"}},
		},
	})
}

func TestContractCheckInputEqualsSubmit(t *testing.T) {
	skipShort(t)
	png, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	blob := func(mediaType string, data []byte) []protocol.Part {
		return []protocol.Part{{Type: protocol.PartBlob, MediaType: mediaType, Data: data}}
	}
	const noImages = "openai/o3-mini"
	image := protocol.Input{ID: "in_img", Parts: blob("image/png", png)}
	rows := []struct {
		name string
		in   protocol.Input
	}{
		{"unsupported_media_type", protocol.Input{ID: "in_type", Parts: blob("application/zip", []byte("PK"))}},
		{"image_bytes_that_do_not_decode", protocol.Input{ID: "in_bad", Parts: blob("image/png", []byte("not a png"))}},
		{"too_large", protocol.Input{ID: "in_big", Parts: blob("application/pdf", make([]byte, 21<<20))}},
		{"image_for_a_model_without_image_input", image},
	}
	if err := harness.CheckInput("anthropic/claude-sonnet-4-5", image); err != nil {
		t.Errorf("CheckInput of a valid image = %v, want nil", err)
	}
	if err := harness.CheckInput("", image); err != nil {
		t.Errorf("CheckInput with no model = %v, want nil", err)
	}
	onHosts(t, func(t *testing.T, h host) {
		extra := map[string]any{"model": noImages, "providers": map[string]any{"openai": map[string]any{"api_key_env": "ANTHROPIC_API_KEY"}}}
		d, _ := startOn(t, h, runtimeWorkdir(t, nil), extra, replyText("ok"))
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				id := d.Create(t)
				before := len(d.events(t, id))
				res := d.call(t, http.MethodPost, "/sessions/"+id+"/inputs", row.in)
				if after := len(d.events(t, id)); after != before {
					t.Errorf("a rejected input appended %d records", after-before)
				}
				want, _ := bodyOf(t, res)["error"].(map[string]any)
				var got *protocol.Error
				if !errors.As(harness.CheckInput(noImages, row.in), &got) {
					t.Fatalf("CheckInput = %v, want a protocol.Error", got)
				}
				details, _ := want["details"].(map[string]any)
				if res.Status != http.StatusBadRequest || string(got.Code) != want["code"] || got.Message != want["message"] || got.Details["input_id"] != details["input_id"] {
					t.Errorf("CheckInput = %+v, Submit = %d %v", got, res.Status, want)
				}
			})
		}
	})
}
