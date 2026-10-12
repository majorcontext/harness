package eventlog

import (
	"testing"

	"github.com/majorcontext/harness/protocol"
)

func TestForeignCountsOnlyMessagesAfterTheLastTurnOfTheProvider(t *testing.T) {
	model := func(m string) protocol.Payload { return protocol.SettingsChanged{Model: &m} }
	onA := []protocol.Payload{protocol.SessionCreated{Model: "a/x"}, admit("1"), start("t1", "1"), item("t1", "i1"), end("t1", protocol.StopCompleted, "")}
	for _, tc := range []struct {
		name   string
		events []protocol.Payload
		model  string
		want   bool
	}{
		{"a session with no message", []protocol.Payload{protocol.SessionCreated{Model: "a/x"}}, "a/x", false},
		{"the first turn of the session", []protocol.Payload{protocol.SessionCreated{Model: "a/x"}, admit("1"), start("t1", "1")}, "a/x", false},
		{"a turn after turns of its own provider", with(onA, admit("2"), start("t2", "2")), "a/x", false},
		{"a turn of a provider with no earlier turn", with(onA, model("b/y"), admit("2"), start("t2", "2")), "b/y", true},
		{"a turn after a turn of another provider", with(onA, model("b/y"), admit("2"), start("t2", "2"), item("t2", "i2"), end("t2", protocol.StopCompleted, ""),
			model("a/x"), admit("3"), start("t3", "3")), "a/x", true},
		{"the next turn after that one", with(onA, model("b/y"), admit("2"), start("t2", "2"), item("t2", "i2"), end("t2", protocol.StopCompleted, ""),
			model("a/x"), admit("3"), start("t3", "3"), item("t3", "i3"), end("t3", protocol.StopCompleted, ""), admit("4"), start("t4", "4")), "a/x", false},
		{"another model of the same provider", with(onA, model("a/z"), admit("2"), start("t2", "2")), "a/z", false},
		{"a turn whose model changes in the middle stays with its own provider", with(onA, admit("2"), start("t2", "2"), item("t2", "i2"), model("b/y"), item("t2", "i3"), end("t2", protocol.StopCompleted, ""),
			admit("3"), start("t3", "3")), "b/y", true},
		{"a turn that ended with no item never reached its backend", with(onA, model("b/y"), admit("2"), start("t2", "2"), item("t2", "i2"), end("t2", protocol.StopCompleted, ""),
			model("a/x"), admit("3"), start("t3", "3"), end("t3", protocol.StopFailed, ""), admit("4"), start("t4", "4")), "a/x", true},
		{"a crashed turn that holds only its marker never reached its backend", with(onA, model("b/y"), admit("2"), start("t2", "2"), item("t2", "i2"), end("t2", protocol.StopCompleted, ""),
			model("a/x"), admit("3"), start("t3", "3"), item("t3", "marker"), end("t3", protocol.StopInterrupted, protocol.CauseCrashed), admit("4"), start("t4", "4")), "a/x", true},
		{"a crashed turn that wrote an item reached its backend", with(onA, model("b/y"), admit("2"), start("t2", "2"), item("t2", "i2"), end("t2", protocol.StopCompleted, ""),
			model("a/x"), admit("3"), start("t3", "3"), item("t3", "i3"), item("t3", "marker"), end("t3", protocol.StopInterrupted, protocol.CauseCrashed), admit("4"), start("t4", "4")), "a/x", false},
		{"a turn that a handoff resumes", with(onA, model("b/y"), admit("2"), start("t2", "2"), item("t2", "i2"), end("t2", protocol.StopCompleted, ""),
			model("a/x"), admit("3"), start("t3", "3"), item("t3", "i3"), suspend("t3", protocol.CauseHandoff), resume("t3", 1)), "a/x", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replay(t, tc.events).Foreign(tc.model); got != tc.want {
				t.Errorf("Foreign(%s) = %t, want %t", tc.model, got, tc.want)
			}
		})
	}
}

func item(turn, id string) protocol.Payload {
	return protocol.ItemCompleted{ItemID: id, TurnID: turn, Message: protocol.LogMessage{Role: protocol.RoleAssistant, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: id}}}}
}
