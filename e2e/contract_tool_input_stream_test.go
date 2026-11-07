package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
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
						Error string         `json:"error"`
					}
					if err := json.Unmarshal(e.Data, &frame); err != nil {
						t.Fatal(err)
					}
					if frame.Error != "" {
						t.Errorf("successful frame error = %q", frame.Error)
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

type rejectingStreamTool struct {
	recordingInputProbe
	message string
}

func (p rejectingStreamTool) NormalizeInput(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New(p.message)
}

func TestContractPromptStreamRejectedToolInput(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		for _, row := range []struct{ name, message string }{{"normal", "input rejected"}, {"empty", ""}, {"bearer", "Bearer hiddencredential"}, {"basic", "Authorization: Basic hiddencredential"}, {"large", "api_key=12345678 " + strings.Repeat("x", 20000)}} {
			message := row.message
			t.Run(provider+"/"+row.name, func(t *testing.T) {
				probe := recordingInputProbe{inputProbe: inputProbe{"probe"}, ran: make(chan json.RawMessage, 1)}
				tool := rejectingStreamTool{recordingInputProbe: probe, message: message}
				call := harnesstest.Step{Name: "call", Reply: harnesstest.Reply{Text: "before", TextAfterTools: "after", ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "probe", Input: map[string]any{"value": "raw"}}}}}
				done := harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("probe"), Reply: harnesstest.Reply{Text: "done"}}
				s, _ := stopRuntime(t, provider, []harness.Tool{tool}, nil, call, done)
				stream, err := s.PromptStream(t.Context(), protocol.Input{ID: "input", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}})
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				var kinds []string
				for e, err := range stream.Events(t.Context()) {
					if err != nil {
						t.Fatal(err)
					}
					if e.Kind == "item.delta" {
						var f protocol.ItemFrame
						if err := json.Unmarshal(e.Data, &f); err != nil {
							t.Fatal(err)
						}
						if f.Text == "before" || f.Text == "after" {
							kinds = append(kinds, "text:"+f.Text)
						}
					}
					if e.Kind == "tool_block.started" || e.Kind == "tool_block.available" {
						kinds = append(kinds, e.Kind)
					}
					if e.Kind == "tool_block.available" {
						var frame struct {
							Input map[string]any `json:"input"`
							Error string         `json:"error"`
						}
						if err := json.Unmarshal(e.Data, &frame); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(frame.Input, map[string]any{"value": "raw"}) || (frame.Error == "" || (message == "input rejected" && frame.Error != message) || (strings.Contains(frame.Error, "12345678") || strings.Contains(frame.Error, "hiddencredential")) || len(frame.Error) > 16384) {
							t.Errorf("frame = %+v, want raw input and input rejected", frame)
						}
					}
				}
				want := []string{"text:before", "tool_block.started", "tool_block.available"}
				if provider == "anthropic" {
					want = append(want, "text:after")
				}
				if !reflect.DeepEqual(kinds, want) {
					t.Errorf("frames = %v", kinds)
				}
				select {
				case <-probe.ran:
					t.Error("rejected tool ran")
				default:
				}
			})
		}
	}
}
