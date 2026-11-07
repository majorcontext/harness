package e2e

import (
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

type invalidToolBlockFrames struct{ available bool }

func (a invalidToolBlockFrames) run(t *testing.T, r *run) {
	t.Helper()
	var kinds []string
	for _, e := range collectToolBlockSSE(t, r) {
		if e.Kind != "tool_block.started" && e.Kind != "tool_block.available" {
			continue
		}
		kinds = append(kinds, e.Kind)
		var f protocol.ToolBlockFrame
		if err := json.Unmarshal(e.Data, &f); err != nil {
			t.Fatal(err)
		}
		if f.CallID == "" || f.Name == "" {
			t.Errorf("%s has incomplete identity: call_id=%q name=%q", e.Kind, f.CallID, f.Name)
		}
		if a.available && e.Kind == "tool_block.available" {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(e.Data, &fields); err != nil {
				t.Fatal(err)
			}
			if _, ok := fields["input"]; ok {
				t.Errorf("invalid input was not omitted: %s", fields["input"])
			}
		}
	}
	if !a.available && len(kinds) != 0 {
		t.Errorf("tool block events = %v, want none", kinds)
	}
	if a.available && (len(kinds) != 2 || kinds[0] != "tool_block.started" || kinds[1] != "tool_block.available") {
		t.Errorf("tool block events = %v, want started and available", kinds)
	}
}

func TestContractToolBlockInvalid(t *testing.T) {
	answer := harnesstest.Step{Name: "answer", Reply: harnesstest.Reply{Text: "done"}}
	rows := []struct {
		name      string
		call      harnesstest.ToolCall
		available bool
	}{
		{"tool_block_missing_id", harnesstest.ToolCall{Name: "missing"}, false},
		{"tool_block_missing_name", harnesstest.ToolCall{ID: "call_1"}, false},
		{"tool_block_invalid_input", harnesstest.ToolCall{ID: "call_1", Name: "missing", RawInput: `{"bad":`}, true},
	}
	var scenarios []scenario
	for _, row := range rows {
		model := []harnesstest.Step{{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{row.call}}}}
		if row.call.ID != "" {
			model = append(model, answer)
		}
		scenarios = append(scenarios, scenario{name: row.name, model: model, actions: []action{create{as: "a"}, invalidToolBlockFrames{available: row.available}}})
	}
	runScenarios(t, scenarios)
}
