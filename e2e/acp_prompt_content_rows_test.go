package e2e

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

var onePixelPDF = base64.StdEncoding.EncodeToString([]byte("%PDF-1.4\n%%EOF\n"))

func acpResource(body map[string]any) map[string]any {
	return map[string]any{"type": "resource", "resource": body}
}

func acpImageBlock(mime, data string) map[string]any {
	return map[string]any{"type": "image", "data": data, "mimeType": mime}
}

// acpModelSaw fails unless the only model request holds a part of the kind whose Text satisfies ok.
func acpModelSaw(kind string, ok func(string) bool) acpStep {
	return acpCheck(func(t *testing.T, c *acpConn) {
		t.Helper()
		reqs := acpRequests(t, c, 1)
		for _, m := range reqs[0].Messages {
			for _, p := range m.Parts {
				if p.Kind == kind && ok(p.Text) {
					return
				}
			}
		}
		t.Errorf("the model request holds no %s part that matches: %+v", kind, reqs[0].Messages)
	})
}

var acpPromptContentScenarios = []acpScenario{
	{
		name:  "initialize_advertises_image_and_embedded_context",
		cite:  "`promptCapabilities` has `image` true and `embeddedContext` true, and no `audio`",
		steps: []acpStep{acpInit{version: 1, want: 1}},
	},
	{
		name:  "image_block_reaches_the_model_as_an_image",
		cite:  "An `image` block is an attachment of the media type that it names: png, jpeg, gif, or webp, and no other type",
		model: []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: "seen"}}},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPromptStart{as: "a", blocks: []any{
				map[string]any{"type": "text", "text": "what is this"},
				acpImageBlock("image/png", onePixelPNG),
			}},
			acpResult{id: "a", stop: "end_turn"},
			acpModelSaw("image", func(s string) bool { return s == "data:image/png;base64,"+onePixelPNG }),
		},
	},
	{
		name:  "text_resource_reaches_the_model_with_its_uri",
		cite:  "A `resource` block with `text` is a text part: a `<resource uri=\"…\">` line, the text, and a `</resource>` line",
		model: []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: "seen"}}},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPromptStart{as: "a", blocks: []any{
				map[string]any{"type": "text", "text": "explain"},
				acpResource(map[string]any{"uri": "file:///src/main.go#L3:4", "mimeType": "text/x-go", "text": "func main() {}"}),
			}},
			acpResult{id: "a", stop: "end_turn"},
			acpModelSaw("text", func(s string) bool {
				return s == "<resource uri=\"file:///src/main.go#L3:4\">\nfunc main() {}\n</resource>"
			}),
		},
	},
	{
		name:  "pdf_resource_blob_reaches_the_model",
		cite:  "A `resource` block with `blob` is an attachment of the media type that it names: png, jpeg, gif, webp, or pdf.",
		model: []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: "seen"}}},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPromptStart{as: "a", blocks: []any{
				map[string]any{"type": "text", "text": "summarize"},
				acpResource(map[string]any{"uri": "file:///doc.pdf", "mimeType": "application/pdf", "blob": onePixelPDF}),
			}},
			acpResult{id: "a", stop: "end_turn"},
			acpModelSaw("document", func(s string) bool { return strings.HasPrefix(s, "data:application/pdf;base64,") }),
		},
	},
	{
		name: "unsupported_content_is_invalid_params",
		cite: "An `audio` block, an attachment of another media type, and an attachment that is not valid base64 are `-32602` with `data.reason`",
		steps: []acpStep{
			acpNew{as: "a"},
			acpPromptStart{as: "a", id: "svg", blocks: []any{acpImageBlock("image/svg+xml", "PHN2Zy8+")}},
			acpResult{id: "svg", code: acpInvalidParams},
			acpPromptStart{as: "a", id: "audio", blocks: []any{map[string]any{"type": "audio", "data": "AAAA", "mimeType": "audio/wav"}}},
			acpResult{id: "audio", code: acpInvalidParams},
			acpPromptStart{as: "a", id: "zip", blocks: []any{acpResource(map[string]any{"uri": "file:///a.zip", "mimeType": "application/zip", "blob": "AAAA"})}},
			acpResult{id: "zip", code: acpInvalidParams},
			acpPromptStart{as: "a", id: "base64", blocks: []any{acpImageBlock("image/png", "not base64!")}},
			acpResult{id: "base64", code: acpInvalidParams},
			acpPromptStart{as: "a", id: "fake", blocks: []any{acpImageBlock("image/png", "AAAA")}},
			acpResult{id: "fake", code: acpInvalidParams, dataCode: "invalid_request"},
			acpPromptStart{as: "a", id: "pdf", blocks: []any{acpImageBlock("application/pdf", onePixelPDF)}},
			acpResult{id: "pdf", code: acpInvalidParams},
		},
	},
	{
		name:   "attachment_for_a_model_without_image_input_is_invalid_params",
		cite:   "A prompt that the runtime refuses with `invalid_request`, an attachment of the wrong content or size and an image for a model without image input among them, is `-32602` through the one error mapping (Error mapping): the message is the text of the runtime error, which names the model in the last case",
		config: map[string]any{"model": "openai/o3-mini", "providers": map[string]any{"openai": map[string]any{"api_key_env": "ANTHROPIC_API_KEY"}}},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPromptStart{as: "a", blocks: []any{acpImageBlock("image/png", onePixelPNG)}},
			acpCheck(func(t *testing.T, c *acpConn) {
				t.Helper()
				f := c.response(c.pending["a"])
				c.mustBeError(f, acpInvalidParams)
				e, _ := f["error"].(map[string]any)
				data, _ := e["data"].(map[string]any)
				if msg, _ := e["message"].(string); !strings.Contains(msg, "openai/o3-mini") || data["code"] != "invalid_request" || data["reason"] == nil {
					t.Errorf("the error does not name the model in its message, or lacks data.code or data.reason: %v", e)
				}
			}),
		},
	},
}
