package eventlog

import (
	"reflect"
	"strconv"
	"testing"
)

func TestTranscriptKeepsFoldedMessagesAfterCompaction(t *testing.T) {
	var events []Event
	events = append(events, base...)
	ids := []string{"a", "b", "c", "d", "e"}
	for i, in := range ids {
		tid := "t" + strconv.Itoa(i+1)
		events = append(events, says(in, DeliveryQueue, "turn "+in), start(tid, in), end(tid, StopCompleted, ""))
	}
	pre := replay(t, events)
	before := pre.Transcript()
	var wantIDs []string
	for _, m := range before {
		wantIDs = append(wantIDs, m.ID)
	}
	cut := pre.Head()
	keptFrom := uint64(0)
	for _, e := range pre.history {
		if e.id == "msg_d" {
			keptFrom = e.seq
		}
	}
	events = append(events, CompactionApplied{FromSeq: 1, ToSeq: keptFrom - 1, Summary: "sum"})
	s := replay(t, events)
	sumID := summaryID(cut + 1)

	var got []string
	for _, m := range s.Transcript() {
		got = append(got, m.ID)
	}
	want := []string{"msg_a", "msg_b", "msg_c", sumID, "msg_d", "msg_e"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Transcript ids = %v, want %v (before compaction %v)", got, want, wantIDs)
	}

	page := s.MessagePage(0, 100)
	if page.Total != 6 || page.FirstSeq != 1 || page.LastSeq != 6 || page.HasMore {
		t.Fatalf("newest page = total %d seq %d..%d more %v", page.Total, page.FirstSeq, page.LastSeq, page.HasMore)
	}

	var paged []string
	for before := uint64(0); ; {
		p := s.MessagePage(before, 2)
		var ids []string
		for _, m := range p.Messages {
			ids = append(ids, m.ID)
		}
		paged = append(ids, paged...)
		if !p.HasMore {
			break
		}
		before = p.FirstSeq
	}
	if !reflect.DeepEqual(paged, want) {
		t.Fatalf("paging by before = %v, want each message once: %v", paged, want)
	}

	h := s.ModelHistory()
	if len(h) != 3 || h[0].Parts[0].Text != "sum" || h[1].Parts[0].Text != "turn d" {
		t.Fatalf("ModelHistory = %+v, want summary then kept messages only", h)
	}
}

func TestTranscriptKeepsEarlierSummariesAsDividers(t *testing.T) {
	events := with(base, says("a", DeliveryQueue, "one"), start("t1", "a"), end("t1", StopCompleted, ""))
	events = with(events, CompactionApplied{FromSeq: 1, ToSeq: 4, Summary: "first"},
		says("b", DeliveryQueue, "two"), start("t2", "b"), end("t2", StopCompleted, ""))
	s := replay(t, events)
	to := s.Head() - 1
	s = replay(t, with(events, CompactionApplied{FromSeq: 1, ToSeq: to, Summary: "second"}))
	var got []string
	for _, m := range s.Transcript() {
		got = append(got, m.ID)
	}
	want := []string{"msg_a", summaryID(5), "msg_b", summaryID(s.Head())}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Transcript ids = %v, want %v", got, want)
	}
}
