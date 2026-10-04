package migrate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// A converted session runs in the runtime. Its next retained result gets a
// handle above each converted handle, though the converted handles skip a number.
func TestRuntimeRunsAConvertedSession(t *testing.T) {
	ctx := context.Background()
	st := harness.NewDiskStore(t.TempDir())
	if _, err := Dir(ctx, copyDir(t, "testdata/journals"), st); err != nil {
		t.Fatal(err)
	}
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{},
		harnesstest.Step{Name: "bash", Match: harnesstest.LastUserText("again"),
			Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "bash", Input: map[string]any{"command": "seq 1 5000"}}}}},
		harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "done"}})
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	cfg := config.Config{Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
		BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}}}
	r, err := harness.New(harness.Options{Store: st, WorkDir: t.TempDir(), Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(ctx) }()
	sess, err := r.Open(ctx, "ses_0000000000000011")
	if err != nil {
		t.Fatal(err)
	}
	after := sess.View().HeadSeq
	if _, err := sess.Submit(ctx, protocol.Input{ID: "again", Parts: []protocol.Part{{Type: protocol.PartText, Text: "again"}}}); err != nil {
		t.Fatal(err)
	}
	var result string
	for e, err := range sess.Events(ctx, after) {
		if err != nil || e.Kind == "turn.ended" {
			break
		}
		var item struct {
			Message struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		}
		if e.Kind == "item.completed" && json.Unmarshal(e.Data, &item) == nil && item.Message.Role == "tool" {
			result = item.Message.Parts[0].Text
		}
	}
	if !strings.HasPrefix(result, "[tool result retained: handle=trh_3 tool=bash") {
		t.Errorf("tool result = %.200q, want a preview of handle trh_3", result)
	}
}
