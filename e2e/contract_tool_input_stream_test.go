package e2e

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

type normalizingStreamTool struct {
	stopTool
	ran  chan json.RawMessage
	seen chan json.RawMessage
}

func (n normalizingStreamTool) NormalizeInput(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	n.seen <- input
	var args map[string]any
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, err
	}
	args["normalized"] = true
	return json.Marshal(args)
}

func (n normalizingStreamTool) Run(_ context.Context, call harness.Call) (protocol.ToolResult, error) {
	n.ran <- call.Arguments
	return protocol.ToolResult{Text: "done"}, nil
}

func TestContractPromptStreamNormalizedToolInput(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			tool := normalizingStreamTool{stopTool: stopTool{name: "probe"}, ran: make(chan json.RawMessage, 1), seen: make(chan json.RawMessage, 2)}
			call := harnesstest.ToolCall{ID: "call_probe", Name: "probe", Input: map[string]any{"value": "original"}}
			s, _ := stopRuntimeOptions(t, provider, harness.Options{Tools: []harness.Tool{tool}, StopAfterTools: []string{"probe"}}, harnesstest.Step{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{call}}})
			stream, err := s.PromptStream(t.Context(), protocol.Input{ID: "input", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			var inputs []map[string]any
			for e, err := range stream.Events(t.Context()) {
				if err != nil {
					t.Fatal(err)
				}
				if e.Kind == "tool_block.available" {
					var frame struct {
						Input map[string]any `json:"input"`
					}
					if err := json.Unmarshal(e.Data, &frame); err != nil {
						t.Fatal(err)
					}
					inputs = append(inputs, frame.Input)
				}
			}
			want := map[string]any{"value": "original", "normalized": true}
			if len(inputs) != 1 || !reflect.DeepEqual(inputs[0], want) {
				t.Errorf("available inputs = %#v, want %#v", inputs, want)
			}
			if len(tool.seen) != 1 {
				t.Errorf("normalizer calls = %d, want 1", len(tool.seen))
			}
			select {
			case raw := <-tool.ran:
				var got map[string]any
				if err := json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got, want) {
					t.Errorf("tool input = %s, %v; want %#v", raw, err, want)
				}
			default:
				t.Error("tool did not run")
			}
		})
	}
}
