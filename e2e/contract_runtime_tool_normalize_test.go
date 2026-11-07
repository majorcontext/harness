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

type inputProbe struct{ name string }

func (p inputProbe) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: p.name, Description: "input probe", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (inputProbe) Run(_ context.Context, call harness.Call) (protocol.ToolResult, error) {
	return protocol.ToolResult{Text: string(call.Arguments)}, nil
}

type normalizingProbe struct{ inputProbe }

func (normalizingProbe) NormalizeInput(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	var v map[string]string
	if err := json.Unmarshal(input, &v); err != nil {
		return nil, err
	}
	if v["value"] == "reject" {
		return nil, errors.New("input rejected")
	}
	v["value"] = strings.TrimSpace(v["value"])
	return json.Marshal(v)
}

func TestContractRuntimeToolInputNormalization(t *testing.T) {
	skipShort(t)
	for _, row := range []struct {
		name  string
		tool  harness.Tool
		input string
		want  string
	}{
		{"normalizes", normalizingProbe{inputProbe{"probe"}}, "  trimmed  ", "trimmed"},
		{"rejects", normalizingProbe{inputProbe{"probe"}}, "reject", "input rejected"},
		{"unchanged", inputProbe{"probe"}, "  unchanged  ", "  unchanged  "},
	} {
		t.Run(row.name, func(t *testing.T) {
			call := harnesstest.Step{Name: "call", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "probe", Input: map[string]any{"value": row.input}}}}}
			done := harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("probe"), Reply: harnesstest.Reply{Text: "done"}}
			s, fake := stopRuntime(t, "openai", []harness.Tool{row.tool}, nil, call, done)
			stopWait(t, stopPrompt(t, s, "a", "go"))
			reqs := fake.Requests()
			if len(reqs) != 2 {
				t.Fatalf("model requests = %d, want 2", len(reqs))
			}
			var result string
			for _, m := range reqs[1].Messages {
				for _, p := range m.Parts {
					if p.Kind == "tool_result" {
						result = p.Text
					}
				}
			}
			if row.name == "rejects" {
				if !strings.Contains(result, row.want) {
					t.Errorf("tool result = %q, want error %q", result, row.want)
				}
				return
			}
			var got map[string]string
			if err := json.Unmarshal([]byte(result), &got); err != nil || got["value"] != row.want {
				t.Errorf("tool result = %q, want value %q (decode: %v)", result, row.want, err)
			}
		})
	}
}

type durableInputProbe struct {
	inputProbe
	ran  chan json.RawMessage
	fail bool
}

func (p durableInputProbe) NormalizeInput(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	if p.fail {
		return nil, errors.New("input rejected")
	}
	return json.RawMessage(`{ "value" : "normalized <>&" }`), nil
}

func (p durableInputProbe) Run(_ context.Context, call harness.Call) (protocol.ToolResult, error) {
	p.ran <- call.Arguments
	return protocol.ToolResult{Text: "executed"}, nil
}

func TestContractRuntimeNormalizedInputIsDurableAndReplayed(t *testing.T) {
	skipShort(t)
	for _, row := range []struct {
		name, provider, input, want string
		fail                        bool
	}{
		{"normalized", "openai", `{"value":"original"}`, `{ "value" : "normalized <>&" }`, false},
		{"rejected", "anthropic", `{ "value" : "raw <>&" }`, `{ "value" : "raw <>&" }`, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			tool := durableInputProbe{inputProbe: inputProbe{"probe"}, ran: make(chan json.RawMessage, 1), fail: row.fail}
			call := harnesstest.Step{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "probe", Input: map[string]any{"value": "original"}, RawInput: row.input}}}}
			done := harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("probe"), Reply: harnesstest.Reply{Text: "done"}}
			s, fake := stopRuntime(t, row.provider, []harness.Tool{tool}, nil, call, done)
			stopWait(t, stopPrompt(t, s, "a", "go"))
			checkDurableInput(t, s, row.want)
			select {
			case got := <-tool.ran:
				if row.fail || string(got) != row.want {
					t.Errorf("executed input = %q, failure = %v, want %q", got, row.fail, row.want)
				}
			default:
				if !row.fail {
					t.Error("tool did not execute")
				}
			}
			reqs := fake.Requests()
			if len(reqs) != 2 {
				t.Fatalf("requests = %d, want 2", len(reqs))
			}
			checkReplayedInput(t, reqs[1], row.want, row.fail)
			if row.provider == "openai" {
				checkReplayedArguments(t, fake.RawRequests()[1], row.want)
			}
		})
	}
}

func checkDurableInput(t *testing.T, s *harness.Session, want string) {
	t.Helper()
	found := 0
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "item.completed" {
			var item protocol.ItemCompleted
			if err := json.Unmarshal(e.Data, &item); err != nil {
				t.Fatal(err)
			}
			for _, part := range item.Message.Parts {
				if part.Type != protocol.LogPartToolCall || part.CallID != "call_1" {
					continue
				}
				found++
				var gotValue, wantValue any
				if err := json.Unmarshal(part.Arguments, &gotValue); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(gotValue, wantValue) || part.ArgumentsText != want {
					t.Errorf("durable arguments = %s, arguments_text = %q; want %s and %q", part.Arguments, part.ArgumentsText, want, want)
				}
			}
		}
		if e.Kind == "turn.ended" {
			break
		}
	}
	if found != 1 {
		t.Errorf("durable calls = %d, want 1", found)
	}
}

func checkReplayedInput(t *testing.T, req harnesstest.Request, raw string, fail bool) {
	t.Helper()
	var want map[string]any
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		t.Fatal(err)
	}
	var calls, results int
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if p.Kind == "tool_use" && p.ToolName == "probe" {
				calls++
				if !reflect.DeepEqual(p.ToolInput, want) {
					t.Errorf("replayed input = %#v, want %#v", p.ToolInput, want)
				}
			}
			if p.Kind == "tool_result" {
				results++
				if fail && (!p.IsError || !strings.Contains(p.Text, "input rejected")) {
					t.Errorf("result = %+v, want normalization error", p)
				}
			}
		}
	}
	if calls != 1 || results != 1 {
		t.Errorf("replayed calls = %d, results = %d; want one each", calls, results)
	}
}

func checkReplayedArguments(t *testing.T, raw []byte, want string) {
	t.Helper()
	var body struct {
		Input []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Arguments string `json:"arguments"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	for _, item := range body.Input {
		if item.Type == "function_call" && item.CallID == "call_1" {
			if item.Arguments != want {
				t.Errorf("replayed arguments = %q, want %q", item.Arguments, want)
			}
			return
		}
	}
	t.Error("next request has no function call")
}
