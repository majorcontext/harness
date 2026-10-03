package eventlog

import (
	"reflect"
	"testing"
)

func says(id string, d Delivery, text string) Event {
	return InputAdmitted{InputID: id, Delivery: d, Source: "user", Parts: []Part{{Type: PartText, Text: text}}}
}

func userText(text string) Message {
	return Message{Role: RoleUser, Parts: []Part{{Type: PartText, Text: text}}}
}

func TestHistory(t *testing.T) {
	calling := Message{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, CallID: "c1", Name: "bash", Arguments: []byte(`{"cmd":"ls"}`)}}}
	answered := Message{Role: RoleTool, Parts: []Part{{Type: PartToolResult, CallID: "c1", Text: "ok"}}}
	firstTurn := with(base, says("a", DeliveryQueue, "one"), start("t1", "a"), call("t1", "i1", "c1"), result("t1", "i2", "c1"), end("t1", StopCompleted, ""))
	for _, tc := range []struct {
		name   string
		events []Event
		want   []Message
	}{
		{"a queued input is not history until a turn takes it", with(base, says("a", DeliveryQueue, "one")), nil},
		{"the inputs of a turn lead its items", firstTurn, []Message{userText("one"), calling, answered}},
		{"a promoted steer input joins at its place", with(base, says("a", DeliveryQueue, "one"), start("t1", "a"), says("s", DeliverySteer, "two"), call("t1", "i1", "c1"), promote("s", "t1")),
			[]Message{userText("one"), calling, userText("two")}},
		{"a withdrawn input never joins", with(base, says("a", DeliveryQueue, "one"), says("b", DeliveryQueue, "two"), withdraw("b"), start("t1", "a")), []Message{userText("one")}},
		{"a compaction summary leads the items after it", with(firstTurn, says("b", DeliveryQueue, "two"), start("t2", "b"), CompactionApplied{FromSeq: 1, ToSeq: 7, Summary: "sum"}),
			[]Message{userText("sum"), userText("two")}},
		{"the newest compaction wins", with(firstTurn, CompactionApplied{FromSeq: 1, ToSeq: 4, Summary: "old"}, says("b", DeliveryQueue, "two"), start("t2", "b"), CompactionApplied{FromSeq: 1, ToSeq: 6, Summary: "new"}),
			[]Message{userText("new"), userText("two")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replay(t, tc.events).History(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("History = %+v\nwant %+v", got, tc.want)
			}
		})
	}
}
