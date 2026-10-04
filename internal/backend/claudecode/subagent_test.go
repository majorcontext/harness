package claudecode_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/eventlog"
)

// items returns the messages of the item.completed records of s, in order.
func items(t *testing.T, s *harness.Session) []eventlog.Message {
	t.Helper()
	var out []eventlog.Message
	for e, err := range s.Events(bg, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "item.completed" {
			var it eventlog.ItemCompleted
			if err := json.Unmarshal(e.Data, &it); err != nil {
				t.Fatal(err)
			}
			out = append(out, it.Message)
		}
		if e.Kind == "turn.ended" {
			return out
		}
	}
	return out
}

func TestClaudeCodeSubagentFramesKeepTheirParent(t *testing.T) {
	argvLog := fakeClaude(t, "subagent")
	r := claudeRuntime(t, harness.NewMemStore(), nil, false)
	defer closeRuntime(t, r)
	s := createClaude(t, r, nil)
	turnOf(t, s, text("a", "run it"))
	var got []string
	for _, m := range items(t, s) {
		got = append(got, m.Role+" "+m.ParentCallID+" "+m.Parts[0].Text)
	}
	want := []string{"assistant  ", "assistant toolu_parent Working inside the subagent.", "tool toolu_parent subagent done", "assistant  All done."}
	if !slices.Equal(got, want) {
		t.Errorf("items (role, parent, text) =\n%q\nwant\n%q", got, want)
	}
	if argv := jsonLines[[]string](t, argvLog); len(argv) != 1 || !slices.Contains(argv[0], "--forward-subagent-text") {
		t.Errorf("argv = %q, want --forward-subagent-text", argv)
	}
}

func TestClaudeCodeSubagentAndMainThreadsKeepTheirWireOrder(t *testing.T) {
	fakeClaude(t, "parallel_tools_crossing")
	r := claudeRuntime(t, harness.NewMemStore(), nil, false)
	defer closeRuntime(t, r)
	s := createClaude(t, r, nil)
	turnOf(t, s, text("a", "cross"))
	var got []string
	for _, m := range items(t, s) {
		got = append(got, m.Role+" "+m.ParentCallID+" "+m.Parts[0].CallID)
	}
	want := []string{"assistant toolu_parent toolu_child", "assistant  toolu_main", "tool toolu_parent toolu_child", "assistant  ", "tool  toolu_main"}
	if !slices.Equal(got, want) {
		t.Errorf("items (role, parent, call) =\n%q\nwant\n%q", got, want)
	}
}
