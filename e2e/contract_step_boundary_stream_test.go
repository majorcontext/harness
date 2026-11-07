package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

type stepBoundaryStream struct{}

func (stepBoundaryStream) run(t *testing.T, r *run) {
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
	var got []string
	for e, err := range stream.Events(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		switch e.Kind {
		case "step.started", "step.ended":
			var d struct {
				Index        int    `json:"index"`
				FinishReason string `json:"finish_reason"`
			}
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			if e.Ephemeral {
				t.Errorf("%s is ephemeral", e.Kind)
			}
			got = append(got, fmt.Sprintf("%s:%d:%s", e.Kind, d.Index, d.FinishReason))
		case protocol.KindItemStarted, protocol.KindItemDelta:
			f := decodeEvent[protocol.ItemFrame](t, e)
			got = append(got, e.Kind+":"+f.Text)
		case protocol.KindItemCompleted:
			item := decodeEvent[protocol.ItemCompleted](t, e)
			if item.Message.Role == "assistant" || item.Message.Role == "tool" {
				got = append(got, "item.completed:"+string(item.Message.Role))
			}
		}
	}
	want := []string{"step.started:0:", "item.started:", "item.delta:calling", "item.completed:assistant", "item.completed:tool", "step.ended:0:tool_use", "step.started:1:", "item.started:", "item.delta:done", "item.completed:assistant", "step.ended:1:end_turn"}
	if !slices.Equal(got, want) {
		t.Errorf("PromptStream order = %q, want %q", got, want)
	}
}
