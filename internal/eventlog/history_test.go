package eventlog

import (
	"reflect"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

func says(id string, d protocol.Delivery, text string) protocol.Payload {
	return protocol.InputAdmitted{InputID: id, Delivery: d, Source: "user", Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: text}}}
}

func userText(text string) protocol.LogMessage {
	return protocol.LogMessage{Role: protocol.RoleUser, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: text}}}
}

const operator = "OPERATOR MESSAGES (address these, then continue the task):\n"

func TestHistory(t *testing.T) {
	calling := protocol.LogMessage{Role: protocol.RoleAssistant, Parts: []protocol.LogPart{{Type: protocol.LogPartToolCall, CallID: "c1", Name: "bash", Arguments: []byte(`{"cmd":"ls"}`)}}}
	answered := protocol.LogMessage{Role: protocol.RoleTool, Parts: []protocol.LogPart{{Type: protocol.LogPartToolResult, CallID: "c1", Text: "ok"}}}
	firstTurn := with(base, says("a", protocol.DeliveryQueue, "one"), start("t1", "a"), call("t1", "i1", "c1"), result("t1", "i2", "c1"), end("t1", protocol.StopCompleted, ""))
	parked := with(asking, end("t1", protocol.StopAwaitingInput, ""))
	callMsg := protocol.LogMessage{Role: protocol.RoleAssistant, Parts: []protocol.LogPart{{Type: protocol.LogPartToolCall, CallID: "c1", Name: "bash", Arguments: []byte(`{"cmd":"ls"}`)}}}
	resultOf := func(text string, isErr bool) protocol.LogMessage {
		return protocol.LogMessage{Role: protocol.RoleTool, Parts: []protocol.LogPart{{Type: protocol.LogPartToolResult, CallID: "c1", Name: "bash", Text: text, IsError: isErr}}}
	}
	choice := protocol.RequestResolved{RequestID: "c1", Resolution: protocol.ResolutionAnswered, Answer: []byte(`{"Which database?":"SQLite","Which cache?":"none"}`)}
	for _, tc := range []struct {
		name   string
		events []protocol.Payload
		want   []protocol.LogMessage
	}{
		{"an answer leaves its call open for the turn that it starts", with(parked, choice), []protocol.LogMessage{userText("hi"), callMsg}},
		{"the turn that an answer starts records the result of the call", with(parked, choice, start("t2"), result("t2", "i2", "c1")),
			[]protocol.LogMessage{userText("hi"), callMsg, {Role: protocol.RoleTool, Parts: []protocol.LogPart{{Type: protocol.LogPartToolResult, CallID: "c1", Text: "ok"}}}}},
		{"a dismissal is an error result", with(parked, dismiss("c1")),
			[]protocol.LogMessage{userText("hi"), callMsg, resultOf("The user dismissed this question without answering.", true)}},
		{"a queued input is not history until a turn takes it", with(base, says("a", protocol.DeliveryQueue, "one")), nil},
		{"the inputs of a turn lead its items", firstTurn, []protocol.LogMessage{userText("one"), calling, answered}},
		{"a promoted steer input joins at its place as an operator message", with(base, says("a", protocol.DeliveryQueue, "one"), start("t1", "a"), says("s", protocol.DeliverySteer, "two"), call("t1", "i1", "c1"), promote("s", "t1")),
			[]protocol.LogMessage{userText("one"), calling, userText(operator + "1. two\n")}},
		{"steer inputs promoted together share one operator message", with(base, says("a", protocol.DeliveryQueue, "one"), start("t1", "a"), says("s", protocol.DeliverySteer, "two"), says("u", protocol.DeliverySteer, "three"), call("t1", "i1", "c1"), promote("s", "t1"), promote("u", "t1")),
			[]protocol.LogMessage{userText("one"), calling, userText(operator + "1. two\n2. three\n")}},
		{"steer inputs promoted at two boundaries keep two operator messages", with(base, says("a", protocol.DeliveryQueue, "one"), start("t1", "a"), says("s", protocol.DeliverySteer, "two"), says("u", protocol.DeliverySteer, "three"), call("t1", "i1", "c1"), promote("s", "t1"), result("t1", "i2", "c1"), promote("u", "t1")),
			[]protocol.LogMessage{userText("one"), calling, userText(operator + "1. two\n"), answered, userText(operator + "1. three\n")}},
		{"a withdrawn input never joins", with(base, says("a", protocol.DeliveryQueue, "one"), says("b", protocol.DeliveryQueue, "two"), withdraw("b"), start("t1", "a")), []protocol.LogMessage{userText("one")}},
		{"a compaction summary leads the items after it", with(firstTurn, says("b", protocol.DeliveryQueue, "two"), start("t2", "b"), protocol.CompactionApplied{FromSeq: 1, ToSeq: 7, Summary: "sum"}),
			[]protocol.LogMessage{userText(SummaryBanner + "sum"), userText("two")}},
		{"the summary message of the model ends with the index of the retained results", with(firstTurn, says("b", protocol.DeliveryQueue, "two"), start("t2", "b"), protocol.CompactionApplied{FromSeq: 1, ToSeq: 7, Summary: "sum", Index: "idx"}),
			[]protocol.LogMessage{userText(SummaryBanner + "sum\n\nidx"), userText("two")}},
		{"the newest compaction wins", with(firstTurn, protocol.CompactionApplied{FromSeq: 1, ToSeq: 4, Summary: "old"}, says("b", protocol.DeliveryQueue, "two"), start("t2", "b"), protocol.CompactionApplied{FromSeq: 1, ToSeq: 6, Summary: "new"}),
			[]protocol.LogMessage{userText(SummaryBanner + "new"), userText("two")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replay(t, tc.events).History(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("History = %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestFold(t *testing.T) {
	turn := func(n, in, text string) []protocol.Payload {
		return []protocol.Payload{says(in, protocol.DeliveryQueue, text), start(n, in), end(n, protocol.StopCompleted, "")}
	}
	three := with(with(with(base, turn("t1", "a", "one")...), turn("t2", "b", "two")...), turn("t3", "c", "three")...)
	summarized := with(with(with(base, turn("t1", "a", "one")...), protocol.CompactionApplied{FromSeq: 1, ToSeq: 4, Summary: "sum"}), turn("t2", "b", "two")...)
	for _, tc := range []struct {
		name   string
		events []protocol.Payload
		keep   int
		want   []protocol.LogMessage
		to     uint64
	}{
		{"no more turns than keep fold nothing", with(with(base, turn("t1", "a", "one")...), turn("t2", "b", "two")...), 2, nil, 0},
		{"the turns before the newest keep fold up to the first kept turn", three, 2, []protocol.LogMessage{userText("one")}, 5},
		{"keep one folds every turn but the newest", three, 1, []protocol.LogMessage{userText("one"), userText("two")}, 8},
		{"a summary alone folds nothing", with(summarized, turn("t3", "c", "three")...), 2, nil, 0},
		{"a summary folds with the turns after it", with(summarized, turn("t3", "c", "three")...), 1,
			[]protocol.LogMessage{userText(SummaryBanner + "sum"), userText("two")}, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, to, ok := replay(t, tc.events).Fold(tc.keep)
			if !reflect.DeepEqual(got, tc.want) || to != tc.to || ok != (tc.to != 0) {
				t.Fatalf("Fold(%d) = %+v, %d, %v\nwant %+v, %d", tc.keep, got, to, ok, tc.want, tc.to)
			}
		})
	}
}

func TestFoldRunningTurn(t *testing.T) {
	one := with(base, says("a", protocol.DeliveryQueue, "one"), start("t1", "a"))
	trips := with(one, call("t1", "i1", "c1"), result("t1", "i2", "c1"), call("t1", "i3", "c2"), result("t1", "i4", "c2"), call("t1", "i5", "c3"), result("t1", "i6", "c3"))
	parallel := with(one, call("t1", "i1", "c1", "c2"), result("t1", "i2", "c1"), result("t1", "i3", "c2"), call("t1", "i4", "c3"), result("t1", "i5", "c3"))
	for _, tc := range []struct {
		name   string
		events []protocol.Payload
		n      int
		to     uint64
	}{
		{"a turn with one round trip folds nothing", with(one, call("t1", "i1", "c1"), result("t1", "i2", "c1")), 0, 0},
		{"a turn with no round trip folds nothing", one, 0, 0},
		{"a turn that ended folds nothing", with(trips, end("t1", protocol.StopCompleted, "")), 0, 0},
		{"the running turn folds up to its newest round trip", trips, 5, 7},
		{"a round trip of parallel calls folds whole", parallel, 4, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, to, ok := replay(t, tc.events).Fold(1)
			if len(got) != tc.n || to != tc.to || ok != (tc.n > 0) {
				t.Fatalf("Fold(1) = %d messages, %d, %v\nwant %d messages, %d", len(got), to, ok, tc.n, tc.to)
			}
		})
	}
}

func TestCompactionInsideATurnKeepsTheWordsOfThePerson(t *testing.T) {
	steered := with(base, says("a", protocol.DeliveryQueue, "one"), start("t1", "a"), call("t1", "i1", "c1"), says("s", protocol.DeliverySteer, "two"), promote("s", "t1"),
		result("t1", "i2", "c1"), call("t1", "i3", "c2"), result("t1", "i4", "c2"))
	first := with(steered, protocol.CompactionApplied{FromSeq: 1, ToSeq: 7, Summary: "sum"})
	second := with(first, call("t1", "i5", "c3"), result("t1", "i6", "c3"), protocol.CompactionApplied{FromSeq: 8, ToSeq: 10, Summary: "again"})
	callOf := func(id string) protocol.LogMessage { return replay(t, with(running, call("t1", "i", id))).History()[1] }
	resultOf := func(id string) protocol.LogMessage {
		return replay(t, with(running, call("t1", "i", id), result("t1", "j", id))).History()[2]
	}
	summary := func(text string) protocol.LogMessage { return userText(SummaryBanner + text) }
	for _, tc := range []struct {
		name   string
		events []protocol.Payload
		want   []protocol.LogMessage
	}{
		{"the messages of the person follow the summary, then the kept round trip", first,
			[]protocol.LogMessage{summary("sum"), userText("one"), userText(operator + "1. two\n"), callOf("c2"), resultOf("c2")}},
		{"a later compaction folds them as other history and keeps them again", second,
			[]protocol.LogMessage{summary("again"), userText("one"), userText(operator + "1. two\n"), callOf("c3"), resultOf("c3")}},
		{"a compaction after the turn ended keeps no message of the person", with(steered, end("t1", protocol.StopCompleted, ""), protocol.CompactionApplied{FromSeq: 1, ToSeq: 7, Summary: "sum"}),
			[]protocol.LogMessage{summary("sum"), callOf("c2"), resultOf("c2")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := replay(t, tc.events)
			if got := s.History(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("History = %+v\nwant %+v", got, tc.want)
			}
			if got := s.ModelHistory(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ModelHistory = %+v\nwant %+v", got, tc.want)
			}
		})
	}
	t.Run("readers see each message once", func(t *testing.T) {
		var got []string
		for _, m := range replay(t, second).Transcript() {
			got = append(got, m.ID)
		}
		want := []string{"msg_a", "msg_i1", "msg_s", "msg_i2", summaryID(10), "msg_i3", "msg_i4", summaryID(13), "msg_i5", "msg_i6"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Transcript IDs = %v\nwant %v", got, want)
		}
	})
}

func TestResolutionOutlivesTheRequest(t *testing.T) {
	s := replay(t, with(asking, end("t1", protocol.StopAwaitingInput, ""), dismiss("c1")))
	if got, ok := s.Resolution("c1"); !ok || got.Resolution != protocol.ResolutionDismissed {
		t.Errorf("Resolution(c1) = %+v, %v, want the dismissal", got, ok)
	}
	if _, ok := s.Resolution("r2"); ok {
		t.Error("Resolution(r2) found a request that never opened")
	}
}

func TestSteerMessageKeepsTheBlobPartsOfEachInput(t *testing.T) {
	blob := protocol.LogPart{Type: protocol.LogPartBlob, MediaType: "image/png", BlobKey: "k", Bytes: 3}
	m := SteerMessage([][]protocol.LogPart{{{Type: protocol.LogPartText, Text: "look"}, blob}, {blob}})
	if !reflect.DeepEqual(m.Parts[1:], []protocol.LogPart{blob, blob}) || m.Parts[0].Type != protocol.LogPartText {
		t.Errorf("SteerMessage parts = %+v, want the text, then each blob in order", m.Parts)
	}
}

func TestBannerAt(t *testing.T) {
	firstTurn := with(base, says("a", protocol.DeliveryQueue, "one"), start("t1", "a"), call("t1", "i1", "c1"), result("t1", "i2", "c1"), end("t1", protocol.StopCompleted, ""))
	compacted := with(firstTurn, says("b", protocol.DeliveryQueue, "two"), start("t2", "b"), protocol.CompactionApplied{FromSeq: 1, ToSeq: 7, Summary: "sum"})
	for _, tc := range []struct {
		name   string
		events []protocol.Payload
		want   int
	}{
		{"the banner follows the first message", firstTurn, 1},
		{"the banner follows the whole history that a compaction left", compacted, 2},
		{"a later message leaves the place of the banner", with(compacted, call("t2", "i3", "c2"), result("t2", "i4", "c2")), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replay(t, tc.events).BannerAt(); got != tc.want {
				t.Fatalf("BannerAt = %d, want %d", got, tc.want)
			}
		})
	}
}
