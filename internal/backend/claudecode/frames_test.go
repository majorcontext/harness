package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
)

func TestUserLineSendsAnEngineContextPartAsRawText(t *testing.T) {
	m := eventlog.Message{Parts: []eventlog.Part{
		{Type: eventlog.PartText, Text: "OPERATOR MESSAGES (address these, then continue the task):\n1. go on\n"},
		{Type: eventlog.PartEngineContext, Text: "[tasks:\n- kid (agent=explore) done: found it (usage: 1 in / 2 out)\n]"},
	}}
	line, err := userLine(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"user","message":{"role":"user","content":"OPERATOR MESSAGES (address these, then continue the task):\n1. go on\n\n\n[tasks:\n- kid (agent=explore) done: found it (usage: 1 in / 2 out)\n]"}}`
	if got, _ := json.Marshal(line); string(got) != want {
		t.Errorf("line = %s, want %s", got, want)
	}
}
