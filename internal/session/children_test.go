package session

import (
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
)

func TestSettlement(t *testing.T) {
	said := eventlog.ItemCompleted{ItemID: "i1", TurnID: "t1", Message: eventlog.Message{Role: eventlog.RoleAssistant,
		Parts: []eventlog.Part{{Type: eventlog.PartText, Text: "found it"}}}}
	ended := func(r eventlog.StopReason, cause eventlog.Cause, msg string) eventlog.Event {
		return eventlog.TurnEnded{TurnID: "t1", StopReason: r, Cause: cause, Error: msg}
	}
	const head = "A background task you started has finished.\n\ntask: kid (agent explore)\noutcome: "
	for _, tc := range []struct {
		name    string
		events  []eventlog.Event
		outcome eventlog.Outcome
		text    string
	}{
		{"a completed turn is done with its last text", []eventlog.Event{said, ended(eventlog.StopCompleted, "", "")},
			eventlog.OutcomeDone, head + "done\n\nfound it"},
		{"a failed turn names its error", []eventlog.Event{ended(eventlog.StopFailed, "", "boom")}, eventlog.OutcomeFailed, head + "failed: boom"},
		{"a crashed turn failed", []eventlog.Event{said, ended(eventlog.StopInterrupted, eventlog.CauseCrashed, "")},
			eventlog.OutcomeFailed, head + "failed: crashed\n\nfound it"},
		{"a usage limit failed the turn with the provider message", []eventlog.Event{ended(eventlog.StopFailed, eventlog.CauseProviderExhausted, "limit reached")},
			eventlog.OutcomeFailed, head + "failed: limit reached"},
		{"a stopped turn is canceled", []eventlog.Event{ended(eventlog.StopInterrupted, eventlog.CauseStopped, "")}, eventlog.OutcomeCanceled, head + "canceled: stopped"},
		{"a running turn has not settled", []eventlog.Event{said}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &eventlog.State{}
			events := append([]eventlog.Event{eventlog.SessionCreated{ParentID: "p", Agent: "explore", Model: "m/m"},
				eventlog.InputAdmitted{InputID: "a", Delivery: eventlog.DeliveryQueue, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: "go"}}},
				eventlog.TurnStarted{TurnID: "t1", InputIDs: []string{"a"}}}, tc.events...)
			for i, e := range events {
				data, err := eventlog.Envelope{Seq: uint64(i) + 1, Event: e}.Encode()
				if err == nil {
					err = s.Apply(eventlog.Record{Seq: uint64(i) + 1, Data: data})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			got, text, ok := Settlement("kid", s)
			want := eventlog.ChildSettled{ChildID: "kid", Outcome: tc.outcome, ResultRef: "t1"}
			if ok != (tc.outcome != "") || ok && (got != want || text != tc.text) {
				t.Errorf("Settlement = %+v, %q, %v; want %+v, %q", got, text, ok, want, tc.text)
			}
		})
	}
}
