package session

import (
	"context"
	"errors"
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

func TestStepEndAfterCancellation(t *testing.T) {
	ctx := context.Background()
	a := newActor(Config{ID: "s1", Store: &memLog{}, Ownership: owned{}, Backend: newHeldBackend(false), Base: ctx}, &eventlog.State{})
	if err := a.append(protocol.SessionCreated{Model: "test/model"}, protocol.InputAdmitted{InputID: "i", Delivery: protocol.DeliveryQueue, Source: "user", Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: "hi"}}}, protocol.TurnStarted{TurnID: "t", InputIDs: []string{"i"}}, protocol.StepStarted{TurnID: "t", Index: 0}); err != nil {
		t.Fatal(err)
	}
	r := a.newRun(kindTurn, "t")
	a.run = r
	r.cancel(errStopTurn)
	go func() { (<-a.mail)() }()
	if err := (&turnRun{a: a, r: r}).StepEnded(0, "end_turn"); !errors.Is(err, errStopTurn) {
		t.Fatalf("step end error = %v, want cancellation", err)
	}
}
