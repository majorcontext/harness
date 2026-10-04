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

const operator = "OPERATOR MESSAGES (address these, then continue the task):\n"

func TestHistory(t *testing.T) {
	calling := Message{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, CallID: "c1", Name: "bash", Arguments: []byte(`{"cmd":"ls"}`)}}}
	answered := Message{Role: RoleTool, Parts: []Part{{Type: PartToolResult, CallID: "c1", Text: "ok"}}}
	firstTurn := with(base, says("a", DeliveryQueue, "one"), start("t1", "a"), call("t1", "i1", "c1"), result("t1", "i2", "c1"), end("t1", StopCompleted, ""))
	parked := with(asking, end("t1", StopAwaitingInput, ""))
	callMsg := Message{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, CallID: "c1", Name: "bash", Arguments: []byte(`{"cmd":"ls"}`)}}}
	resultOf := func(text string, isErr bool) Message {
		return Message{Role: RoleTool, Parts: []Part{{Type: PartToolResult, CallID: "c1", Name: "bash", Text: text, IsError: isErr}}}
	}
	choice := RequestResolved{RequestID: "r1", Resolution: ResolutionAnswered, Answer: []byte(`{"Which database?":"SQLite","Which cache?":"none"}`)}
	for _, tc := range []struct {
		name   string
		events []Event
		want   []Message
	}{
		{"an answer is the result of the call", with(parked, choice),
			[]Message{userText("hi"), callMsg, resultOf(`User has answered your questions: "Which cache?"="none", "Which database?"="SQLite". You can now continue with the user's answers in mind.`, false)}},
		{"an answer that is not a map of choices is its JSON", with(parked, resolve("r1")),
			[]Message{userText("hi"), callMsg, resultOf(`User answered: "yes"`, false)}},
		{"a dismissal is an error result", with(parked, dismiss("r1")),
			[]Message{userText("hi"), callMsg, resultOf("The user dismissed this question without answering.", true)}},
		{"a queued input is not history until a turn takes it", with(base, says("a", DeliveryQueue, "one")), nil},
		{"the inputs of a turn lead its items", firstTurn, []Message{userText("one"), calling, answered}},
		{"a promoted steer input joins at its place as an operator message", with(base, says("a", DeliveryQueue, "one"), start("t1", "a"), says("s", DeliverySteer, "two"), call("t1", "i1", "c1"), promote("s", "t1")),
			[]Message{userText("one"), calling, userText(operator + "1. two\n")}},
		{"steer inputs promoted together share one operator message", with(base, says("a", DeliveryQueue, "one"), start("t1", "a"), says("s", DeliverySteer, "two"), says("u", DeliverySteer, "three"), call("t1", "i1", "c1"), promote("s", "t1"), promote("u", "t1")),
			[]Message{userText("one"), calling, userText(operator + "1. two\n2. three\n")}},
		{"steer inputs promoted at two boundaries keep two operator messages", with(base, says("a", DeliveryQueue, "one"), start("t1", "a"), says("s", DeliverySteer, "two"), says("u", DeliverySteer, "three"), call("t1", "i1", "c1"), promote("s", "t1"), result("t1", "i2", "c1"), promote("u", "t1")),
			[]Message{userText("one"), calling, userText(operator + "1. two\n"), answered, userText(operator + "1. three\n")}},
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

func TestFold(t *testing.T) {
	turn := func(n, in, text string) []Event {
		return []Event{says(in, DeliveryQueue, text), start(n, in), end(n, StopCompleted, "")}
	}
	three := with(with(with(base, turn("t1", "a", "one")...), turn("t2", "b", "two")...), turn("t3", "c", "three")...)
	summarized := with(with(with(base, turn("t1", "a", "one")...), CompactionApplied{FromSeq: 1, ToSeq: 4, Summary: "sum"}), turn("t2", "b", "two")...)
	for _, tc := range []struct {
		name   string
		events []Event
		keep   int
		want   []Message
		to     uint64
	}{
		{"no more turns than keep fold nothing", with(with(base, turn("t1", "a", "one")...), turn("t2", "b", "two")...), 2, nil, 0},
		{"the turns before the newest keep fold up to the first kept turn", three, 2, []Message{userText("one")}, 5},
		{"keep one folds every turn but the newest", three, 1, []Message{userText("one"), userText("two")}, 8},
		{"a summary alone folds nothing", with(summarized, turn("t3", "c", "three")...), 2, nil, 0},
		{"a summary folds with the turns after it", with(summarized, turn("t3", "c", "three")...), 1,
			[]Message{userText("sum"), userText("two")}, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, to, ok := replay(t, tc.events).Fold(tc.keep)
			if !reflect.DeepEqual(got, tc.want) || to != tc.to || ok != (tc.to != 0) {
				t.Fatalf("Fold(%d) = %+v, %d, %v\nwant %+v, %d", tc.keep, got, to, ok, tc.want, tc.to)
			}
		})
	}
}

func TestResolutionOutlivesTheRequest(t *testing.T) {
	s := replay(t, with(asking, end("t1", StopAwaitingInput, ""), dismiss("r1")))
	if got, ok := s.Resolution("r1"); !ok || got.Resolution != ResolutionDismissed {
		t.Errorf("Resolution(r1) = %+v, %v, want the dismissal", got, ok)
	}
	if _, ok := s.Resolution("r2"); ok {
		t.Error("Resolution(r2) found a request that never opened")
	}
}

func TestHistoryKeepsTheSubagentParent(t *testing.T) {
	sub := ItemCompleted{ItemID: "i1", TurnID: "t1", Message: Message{Role: RoleAssistant, ParentCallID: "toolu_p", Parts: []Part{{Type: PartText, Text: "inside"}}}}
	h := replay(t, with(base, says("a", DeliveryQueue, "one"), start("t1", "a"), sub)).History()
	if len(h) != 2 || h[1].ParentCallID != "toolu_p" {
		t.Errorf("History = %+v, want the subagent message with its parent", h)
	}
}
