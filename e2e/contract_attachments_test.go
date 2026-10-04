package e2e

import (
	"encoding/base64"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
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
