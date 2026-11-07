package e2e

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

type toolBlockStream struct {
	want   []string
	inputs map[string]map[string]any
}

func (a toolBlockStream) run(t *testing.T, r *run) {
	t.Helper()
	s, err := r.drv.(*runtimeDriver).rt.Open(t.Context(), r.id(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := s.PromptStream(t.Context(), protocol.Input{ID: "stream", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	var events []protocol.Event
	for e, err := range stream.Events(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	a.check(t, events)
}

func (a toolBlockStream) check(t *testing.T, events []protocol.Event) {
	t.Helper()
	var got []string
	itemID := ""
	for _, e := range events {
		if e.Kind == protocol.KindItemCompleted {
			item := decodeEvent[protocol.ItemCompleted](t, e)
			if item.Message.Role == "assistant" {
				itemID = item.ItemID
				break
			}
		}
	}
	for _, e := range events {
		switch e.Kind {
		case protocol.KindItemDelta:
			f := decodeEvent[protocol.ItemFrame](t, e)
			if f.ItemID == itemID {
				got = append(got, "text:"+f.Text)
			}
		case "tool_block.started", "tool_block.available":
			var block struct {
				ItemID string         `json:"item_id"`
				CallID string         `json:"call_id"`
				Name   string         `json:"name"`
				Input  map[string]any `json:"input"`
			}
			if err := json.Unmarshal(e.Data, &block); err != nil {
				t.Fatal(err)
			}
			if block.ItemID == "" || block.ItemID != itemID {
				t.Errorf("%s item_id = %q, want %q", e.Kind, block.ItemID, itemID)
			}
			if e.Kind == "tool_block.available" && !reflect.DeepEqual(block.Input, a.inputs[block.CallID]) {
				t.Errorf("%s input = %#v, want %#v", block.CallID, block.Input, a.inputs[block.CallID])
			}
			got = append(got, fmt.Sprintf("%s:%s:%s", e.Kind, block.CallID, block.Name))
		}
	}
	if !reflect.DeepEqual(got, a.want) {
		t.Errorf("PromptStream blocks = %q, want %q", got, a.want)
	}
}

func TestContractToolBlockStream(t *testing.T) {
	first := harnesstest.ToolCall{ID: "call_1", Name: "no_such_tool", Input: map[string]any{"nested": map[string]any{"value": "first"}}}
	second := harnesstest.ToolCall{ID: "call_2", Name: "another_missing_tool", Input: map[string]any{"list": []any{"a", "b"}}}
	answer := harnesstest.Step{Name: "answer", Reply: harnesstest.Reply{Text: "done"}}
	rows := []scenario{
		{name: "tool_block_text_then_tool", model: []harnesstest.Step{{Name: "call", Reply: harnesstest.Reply{Text: "before", ToolCalls: []harnesstest.ToolCall{first}}}, answer}, actions: []action{create{as: "a"}, toolBlockStream{want: []string{"text:before", "tool_block.started:call_1:no_such_tool", "tool_block.available:call_1:no_such_tool"}, inputs: map[string]map[string]any{"call_1": first.Input}}}},
		{name: "tool_block_two_tools", model: []harnesstest.Step{{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{first, second}}}, answer}, actions: []action{create{as: "a"}, toolBlockStream{want: []string{"tool_block.started:call_1:no_such_tool", "tool_block.available:call_1:no_such_tool", "tool_block.started:call_2:another_missing_tool", "tool_block.available:call_2:another_missing_tool"}, inputs: map[string]map[string]any{"call_1": first.Input, "call_2": second.Input}}}},
	}

	for _, row := range append([]scenario(nil), rows...) {
		row.name += "_sse"
		row.actions = []action{create{as: "a"}, toolBlockSSE{row.actions[1].(toolBlockStream)}}
		rows = append(rows, row)
	}
	for _, before := range []string{"", "before"} {
		name := "tool_block_tool_then_text_sse"
		want := []string{"tool_block.started:call_1:no_such_tool", "tool_block.available:call_1:no_such_tool", "text:after"}
		if before != "" {
			name = "tool_block_interleaved_sse"
			want = append([]string{"text:before"}, want...)
		}
		rows = append(rows, scenario{name: name, model: []harnesstest.Step{{Name: "call", Reply: harnesstest.Reply{Text: before, TextAfterTools: "after", ToolCalls: []harnesstest.ToolCall{first}}}, answer}, actions: []action{create{as: "a"}, toolBlockSSE{toolBlockStream{want: want, inputs: map[string]map[string]any{"call_1": first.Input}}}}})
	}
	runScenarios(t, rows)
}

type toolBlockSSE struct{ toolBlockStream }

func (a toolBlockSSE) run(t *testing.T, r *run) {
	t.Helper()
	a.check(t, collectToolBlockSSE(t, r))
}

func collectToolBlockSSE(t *testing.T, r *run) []protocol.Event {
	t.Helper()
	d := r.drv.(*runtimeDriver)
	ready := make(chan struct{})
	done := make(chan []protocol.Event, 1)
	go func() {
		var events []protocol.Event
		d.stream(t, r.id(t, "a"), 0, false, func(_ string, e protocol.Event) bool {
			select {
			case <-ready:
			default:
				close(ready)
			}
			events = append(events, e)
			if e.Kind == "turn.ended" {
				done <- events
				return true
			}
			return false
		})
	}()
	<-ready
	r.drv.Submit(t, r.id(t, "a"), "go")
	return <-done
}
