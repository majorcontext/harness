package session

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/protocol"

	"github.com/majorcontext/harness/internal/turn"
)

// inputBackend reports the first part of the input of each turn, then answers.
type inputBackend struct{ runs chan protocol.LogPart }

func (*inputBackend) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (b *inputBackend) Run(_ context.Context, req turn.Request, out turn.Sink) error {
	b.runs <- req.Input[0].Parts[0]
	return out.Item(protocol.LogMessage{Role: protocol.RoleAssistant, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: "ok"}}})
}

func text(s string) []protocol.LogPart {
	return []protocol.LogPart{{Type: protocol.LogPartText, Text: s}}
}

// limitedLog is a log whose first turn a usage limit ended, resetting in an hour, with one input held.
func limitedLog(t *testing.T) *memLog {
	t.Helper()
	return limitedLogResetting(t, time.Hour)
}

func limitedLogResetting(t *testing.T, in time.Duration) *memLog {
	t.Helper()
	return encode(t, protocol.SessionCreated{Model: "m/m"}, protocol.OwnerAcquired{Epoch: 1}, *firstInput(),
		protocol.TurnStarted{TurnID: "turn_1", InputIDs: []string{"in1"}},
		protocol.InputAdmitted{InputID: "in2", Delivery: protocol.DeliveryQueue, Source: "user", Parts: text("held")},
		protocol.TurnEnded{TurnID: "turn_1", StopReason: protocol.StopFailed, Cause: protocol.CauseProviderExhausted, ResetsAt: time.Now().Add(in).Unix()})
}

func openLimited(t *testing.T, serve bool) (*Actor, *inputBackend) {
	t.Helper()
	b := &inputBackend{runs: make(chan protocol.LogPart)}
	cfg := actorConfig(t, limitedLog(t), owned{}, b)
	cfg.ContinueOnReset = serve
	a, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Run()
	return a, b
}

// quiet reports whether no turn starts within d of the bubble clock.
func quiet(b *inputBackend, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-b.runs:
		return false
	case <-timer.C:
		return true
	}
}

func TestAPendingContinueRunsAtTheResetTimeBeforeTheHeldInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		a, b := openLimited(t, true)
		if at := a.View().Session.ContinueAt; at != start.Add(time.Hour).Unix() {
			t.Fatalf("continue_at = %d, want the reset time", at)
		}
		first := <-b.runs
		if time.Since(start) < time.Hour || first.Type != protocol.LogPartEngineContext || first.Text != usageResetText {
			t.Fatalf("first turn after %s took %+v, want the usage reset text at the reset time", time.Since(start), first)
		}
		if held := <-b.runs; held.Text != "held" {
			t.Fatalf("second turn took %+v, want the held input", held)
		}
		synctest.Wait()
		if at := a.View().Session.ContinueAt; at != 0 {
			t.Fatalf("continue_at after the continue = %d, want none", at)
		}
	})
}

func TestARuntimeThatDoesNotServeTheSessionFiresNoContinue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := openLimited(t, false)
		if !quiet(b, 48*time.Hour) || a.View().Session.ContinueAt != 0 {
			t.Fatal("a runtime with ContinueOnReset false fired a continue or showed one")
		}
	})
}

func TestAnInputCancelsThePendingContinue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := openLimited(t, true)
		in := protocol.InputAdmitted{InputID: "in3", Delivery: protocol.DeliveryQueue, Source: "user", Parts: text("now")}
		if _, _, err := a.Submit(context.Background(), in, ""); err != nil {
			t.Fatal(err)
		}
		if got := (<-b.runs).Text; got != "held" {
			t.Fatalf("first turn took %q, want the held input", got)
		}
		if got := (<-b.runs).Text; got != "now" {
			t.Fatalf("second turn took %q, want the new input", got)
		}
		if !quiet(b, 48*time.Hour) {
			t.Fatal("a continue ran after an input cancelled it")
		}
	})
}

func TestAnInterruptCancelsThePendingContinue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := openLimited(t, true)
		if err := a.Interrupt(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		if !quiet(b, 48*time.Hour) || a.View().Session.ContinueAt != 0 {
			t.Fatal("a continue ran or showed after an interrupt cancelled it")
		}
	})
}

func TestAUsageResetInputIsNotFromAPerson(t *testing.T) {
	if fromPerson(protocol.InputAdmitted{Source: protocol.SourceUsageReset}) {
		t.Fatal("a usage_reset input counts as a person, so it would dismiss an open request")
	}
}

func TestOpenContinuesAtOnceWhenTheResetTimeHasPassed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		b := &inputBackend{runs: make(chan protocol.LogPart)}
		cfg := actorConfig(t, limitedLogResetting(t, -time.Minute), owned{}, b)
		cfg.ContinueOnReset = true
		a, err := Open(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		a.Run()
		if first := <-b.runs; first.Text != usageResetText || time.Since(start) != 0 {
			t.Fatalf("first turn took %+v after %s, want the usage reset text at once", first, time.Since(start))
		}
	})
}
