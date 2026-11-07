package e2e

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/protocol"
)

type rewriteNormalizationTool struct {
	stopTool
	ran chan json.RawMessage
}

func (rewriteNormalizationTool) NormalizeInput(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"value":"normalized"}`), nil
}

func (p rewriteNormalizationTool) Run(_ context.Context, call harness.Call) (protocol.ToolResult, error) {
	p.ran <- call.Arguments
	return protocol.ToolResult{Text: "done"}, nil
}

func TestContractNormalizedInputBeforeRewrite(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			seen := make(chan json.RawMessage, 1)
			ran := make(chan json.RawMessage, 1)
			p := &plugin.Plugin{Name: "rewrite", ToolExecuteBefore: &plugin.ToolBefore{Run: func(_ context.Context, _ *plugin.Client, req *plugin.ToolExecuteBeforeRequest) (*plugin.ToolExecuteBeforeResponse, error) {
				seen <- req.Args
				return &plugin.ToolExecuteBeforeResponse{Args: json.RawMessage(`{"value":"rewritten"}`)}, nil
			}}}
			call := harnesstest.Step{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_probe", Name: "probe", Input: map[string]any{"value": "raw"}}}}}
			done := harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("probe"), Reply: harnesstest.Reply{Text: "done"}}
			s, fake := stopRuntimeOptions(t, provider, harness.Options{Tools: []harness.Tool{rewriteNormalizationTool{stopTool: stopTool{name: "probe"}, ran: ran}}, Plugins: []*plugin.Plugin{p}}, call, done)
			stopWait(t, stopPrompt(t, s, "a", "go"))
			check := func(label string, input json.RawMessage, want string) {
				t.Helper()
				var got map[string]any
				if err := json.Unmarshal(input, &got); err != nil || !reflect.DeepEqual(got, map[string]any{"value": want}) {
					t.Errorf("%s = %s, decode error %v; want %q", label, input, err, want)
				}
			}
			select {
			case args := <-seen:
				check("before input", args, "normalized")
			default:
				t.Error("before hook did not run")
			}
			select {
			case args := <-ran:
				check("execution input", args, "rewritten")
			default:
				t.Error("tool did not run")
			}
			page, err := s.Messages(t.Context(), 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			for _, m := range page.Messages {
				for _, part := range m.Parts {
					if part.Type == protocol.MessagePartToolCall && part.CallID == "call_probe" {
						calls++
						check("history input", part.Arguments, "normalized")
					}
				}
			}
			if calls != 1 {
				t.Errorf("history calls = %d, want 1", calls)
			}
			reqs := fake.Requests()
			if len(reqs) != 2 {
				t.Fatalf("model requests = %d, want 2", len(reqs))
			}
			calls = 0
			for _, m := range reqs[1].Messages {
				for _, part := range m.Parts {
					if part.Kind == "tool_use" && part.ToolUseID == "call_probe" {
						calls++
						if !reflect.DeepEqual(part.ToolInput, map[string]any{"value": "normalized"}) {
							t.Errorf("forwarded input = %#v", part.ToolInput)
						}
					}
				}
			}
			if calls != 1 {
				t.Errorf("forwarded calls = %d, want 1", calls)
			}
		})
	}
}
