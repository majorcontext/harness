package e2e

import (
	"encoding/base64"
	"net/http"
	"testing"

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

func TestContractImageForAModelWithoutImageInput(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("image_for_a_model_without_image_input_is_invalid_request", func(t *testing.T) {
			t.Parallel()
			extra := map[string]any{"model": "openai/o3-mini", "providers": map[string]any{"openai": map[string]any{"api_key_env": "ANTHROPIC_API_KEY"}}}
			d, _ := startOn(t, h, runtimeWorkdir(t, nil), extra, replyText("ok"))
			id := d.Create(t)
			before := len(d.events(t, id))
			png, _ := base64.StdEncoding.DecodeString(onePixelPNG)
			body := map[string]any{"id": "in_img", "parts": []protocol.Part{{Type: protocol.PartBlob, MediaType: "image/png", Data: png}}}
			res := d.call(t, http.MethodPost, "/sessions/"+id+"/inputs", body)
			errBody, _ := bodyOf(t, res)["error"].(map[string]any)
			msg, _ := errBody["message"].(string)
			if res.Status != http.StatusBadRequest || errorCode(t, res) != "invalid_request" || msg != "the model openai/o3-mini does not accept image input" {
				t.Errorf("response = %d %v, want 400 invalid_request with the reason as the message", res.Status, res.Body)
			}
			if after := len(d.events(t, id)); after != before {
				t.Errorf("a rejected input appended %d records", after-before)
			}
		})
	})
}
