package eventlog

import "testing"

func TestForeignCountsOnlyMessagesAfterTheLastTurnOfTheProvider(t *testing.T) {
	model := func(m string) Event { return SettingsChanged{Model: &m} }
	onA := []Event{SessionCreated{Model: "a/x"}, admit("1"), start("t1", "1"), item("t1", "i1"), end("t1", StopCompleted, "")}
	for _, tc := range []struct {
		name   string
		events []Event
		model  string
		want   bool
	}{
		{"a session with no message", []Event{SessionCreated{Model: "a/x"}}, "a/x", false},
		{"the first turn of the session", []Event{SessionCreated{Model: "a/x"}, admit("1"), start("t1", "1")}, "a/x", false},
		{"a turn after turns of its own provider", with(onA, admit("2"), start("t2", "2")), "a/x", false},
		{"a turn of a provider with no earlier turn", with(onA, model("b/y"), admit("2"), start("t2", "2")), "b/y", true},
		{"a turn after a turn of another provider", with(onA, model("b/y"), admit("2"), start("t2", "2"), item("t2", "i2"), end("t2", StopCompleted, ""),
			model("a/x"), admit("3"), start("t3", "3")), "a/x", true},
		{"the next turn after that one", with(onA, model("b/y"), admit("2"), start("t2", "2"), item("t2", "i2"), end("t2", StopCompleted, ""),
			model("a/x"), admit("3"), start("t3", "3"), item("t3", "i3"), end("t3", StopCompleted, ""), admit("4"), start("t4", "4")), "a/x", false},
		{"another model of the same provider", with(onA, model("a/z"), admit("2"), start("t2", "2")), "a/z", false},
		{"a turn that a handoff resumes", with(onA, model("b/y"), admit("2"), start("t2", "2"), item("t2", "i2"), end("t2", StopCompleted, ""),
			model("a/x"), admit("3"), start("t3", "3"), item("t3", "i3"), suspend("t3", CauseHandoff), resume("t3", 1)), "a/x", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replay(t, tc.events).Foreign(tc.model); got != tc.want {
				t.Errorf("Foreign(%s) = %t, want %t", tc.model, got, tc.want)
			}
		})
	}
}

func item(turn, id string) Event {
	return ItemCompleted{ItemID: id, TurnID: turn, Message: Message{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: id}}}}
}
