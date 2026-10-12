package e2e

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

func TestContractRuntimeTurn(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_TURN_KEY", "k")
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"wait_returns_the_stop_reason_and_the_usage_of_the_turn", rowWaitReturnsTheStopReasonAndTheUsageOfTheTurn},
		{"events_end_with_the_turn_that_took_the_input", rowEventsEndWithTheTurnThatTookTheInput},
		{"a_repeated_input_id_returns_the_turn_of_the_first_receipt", rowARepeatedInputIDReturnsTheTurnOfTheFirstReceipt},
		{"turn_of_an_earlier_receipt_waits_on_a_reopened_session", rowTurnOfAnEarlierReceiptWaitsOnAReopenedSession},
		{"cancel_withdraws_an_input_that_waits_for_the_next_turn", rowCancelWithdrawsAnInputThatWaitsForTheNextTurn},
		{"cancel_interrupts_the_turn_that_took_the_input", rowCancelInterruptsTheTurnThatTookTheInput},
		{"steer_joins_the_running_turn", rowSteerJoinsTheRunningTurn},
		{"steer_of_an_input_that_waits_fails_with_turn_mismatch", rowSteerOfAnInputThatWaitsFailsWithTurnMismatch},
		{"a_typed_command_waits_for_its_final_status", rowATypedCommandWaitsForItsFinalStatus},
		{"wait_ends_with_the_stop_cause_when_the_session_stops_first", rowWaitEndsWithTheStopCauseWhenTheSessionStopsFirst},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			row.run(t)
		})
	}
}

func turnRuntime(t *testing.T, steps ...harnesstest.Step) (*harness.Runtime, *harness.Session, *harnesstest.Server) {
	t.Helper()
	fake := harnesstest.NewChat(t, steps...)
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Getenv: os.Getenv, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_TURN_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	return r, s, fake
}

func textInput(id, text string) protocol.Input {
	return protocol.Input{ID: id, Parts: []protocol.Part{{Type: protocol.PartText, Text: text}}}
}

func promptTurn(t *testing.T, s *harness.Session, in protocol.Input) *harness.Turn {
	t.Helper()
	turn, err := s.Prompt(t.Context(), in)
	if err != nil {
		t.Fatalf("Prompt(%s): %v", in.ID, err)
	}
	return turn
}

func waitTurn(t *testing.T, turn *harness.Turn) harness.TurnResult {
	t.Helper()
	res, err := turn.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait(%s): %v", turn.Admitted().InputID, err)
	}
	return res
}

func awaitRequest(t *testing.T, fake *harnesstest.Server) {
	t.Helper()
	if !fake.AwaitRequests(1, 10*time.Second) {
		t.Fatal("the model server got no request")
	}
}

func rowWaitReturnsTheStopReasonAndTheUsageOfTheTurn(t *testing.T) {
	_, s, _ := turnRuntime(t, harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: "done", Usage: harnesstest.Usage{Input: 7, Output: 4}}})
	res := waitTurn(t, promptTurn(t, s, textInput("a", "go")))
	if res.InputID != "a" || res.TurnID == "" || res.StopReason != protocol.StopCompleted || res.Cause != "" || res.Withdrawn {
		t.Errorf("result = %+v, want input a completed by a turn", res)
	}
	if res.Usage.OutputTokens != 4 || res.Usage.InputTokens != 7 {
		t.Errorf("usage = %+v, want 7 in and 4 out", res.Usage)
	}
}

func rowEventsEndWithTheTurnThatTookTheInput(t *testing.T) {
	_, s, _ := turnRuntime(t, harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: "done"}})
	turn := promptTurn(t, s, textInput("a", "go"))
	var kinds []string
	for e, err := range turn.Events(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		if !e.Ephemeral {
			kinds = append(kinds, e.Kind)
		}
	}
	if len(kinds) < 3 || kinds[0] != protocol.KindInputAdmitted || kinds[len(kinds)-1] != protocol.KindTurnEnded {
		t.Errorf("events = %v, want input.admitted first and turn.ended last", kinds)
	}
}

func rowARepeatedInputIDReturnsTheTurnOfTheFirstReceipt(t *testing.T) {
	_, s, _ := turnRuntime(t, harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: "done"}})
	first := promptTurn(t, s, textInput("a", "go"))
	want := waitTurn(t, first)
	again := promptTurn(t, s, textInput("a", "go"))
	if a := again.Admitted(); !a.Repeat || a.Seq != first.Admitted().Seq {
		t.Errorf("receipt = %+v, want the first seq with Repeat", a)
	}
	if got := waitTurn(t, again); got != want {
		t.Errorf("result = %+v, want %+v", got, want)
	}
}

func rowTurnOfAnEarlierReceiptWaitsOnAReopenedSession(t *testing.T) {
	r, s, _ := turnRuntime(t, harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: "done"}})
	turn := promptTurn(t, s, textInput("a", "go"))
	want := waitTurn(t, turn)
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := r.Open(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitTurn(t, reopened.Turn(turn.Admitted())); got != want {
		t.Errorf("result = %+v, want %+v", got, want)
	}
}

func rowCancelWithdrawsAnInputThatWaitsForTheNextTurn(t *testing.T) {
	_, s, fake := turnRuntime(t, harnesstest.Step{Name: "first", Reply: harnesstest.Reply{Text: "working", Block: true}})
	running := promptTurn(t, s, textInput("a", "go"))
	awaitRequest(t, fake)
	queued := textInput("b", "later")
	queued.Delivery = protocol.DeliveryQueue
	waiting := promptTurn(t, s, queued)
	if err := waiting.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	if res := waitTurn(t, waiting); !res.Withdrawn || res.TurnID != "" {
		t.Errorf("result = %+v, want a withdrawn input", res)
	}
	fake.Release("first")
	if res := waitTurn(t, running); res.StopReason != protocol.StopCompleted {
		t.Errorf("running turn = %+v, want completed", res)
	}
}

func rowCancelInterruptsTheTurnThatTookTheInput(t *testing.T) {
	_, s, fake := turnRuntime(t, harnesstest.Step{Name: "first", Reply: harnesstest.Reply{Text: "working", Block: true}})
	turn := promptTurn(t, s, textInput("a", "go"))
	awaitRequest(t, fake)
	if err := turn.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	res := waitTurn(t, turn)
	if res.StopReason != protocol.StopInterrupted || res.Cause != protocol.CauseStopped {
		t.Errorf("result = %+v, want interrupted with cause stopped", res)
	}
	if err := turn.Cancel(t.Context()); err != nil {
		t.Errorf("Cancel of an ended turn = %v, want nil", err)
	}
}

func rowSteerJoinsTheRunningTurn(t *testing.T) {
	_, s, fake := turnRuntime(t,
		harnesstest.Step{Name: "first", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "missing"}}, Block: true}},
		harnesstest.Step{Name: "second", Reply: harnesstest.Reply{Text: "done"}})
	turn := promptTurn(t, s, textInput("a", "go"))
	awaitRequest(t, fake)
	steer, err := turn.Steer(t.Context(), textInput("b", "also this"))
	if err != nil {
		t.Fatal(err)
	}
	fake.Release("first")
	got, want := waitTurn(t, steer), waitTurn(t, turn)
	if got.TurnID != want.TurnID || got.StopReason != protocol.StopCompleted {
		t.Errorf("steer result = %+v, want the turn %s completed", got, want.TurnID)
	}
}

func rowSteerOfAnInputThatWaitsFailsWithTurnMismatch(t *testing.T) {
	_, s, fake := turnRuntime(t,
		harnesstest.Step{Name: "first", Reply: harnesstest.Reply{Text: "working", Block: true}},
		harnesstest.Step{Name: "later", Reply: harnesstest.Reply{Text: "done"}})
	running := promptTurn(t, s, textInput("a", "go"))
	awaitRequest(t, fake)
	queued := textInput("b", "later")
	queued.Delivery = protocol.DeliveryQueue
	waiting := promptTurn(t, s, queued)
	if _, err := waiting.Steer(t.Context(), textInput("c", "now")); !errors.Is(err, harness.ErrTurnMismatch) {
		t.Errorf("Steer = %v, want ErrTurnMismatch", err)
	}
	fake.Release("first")
	waitTurn(t, running)
	waitTurn(t, waiting)
}

func rowATypedCommandWaitsForItsFinalStatus(t *testing.T) {
	_, s, _ := turnRuntime(t)
	in := textInput("a", "/thinking high")
	in.Source = protocol.SourceTyped
	res := waitTurn(t, promptTurn(t, s, in))
	if res.Command != protocol.CommandSucceeded || res.TurnID != "" {
		t.Errorf("result = %+v, want a command that succeeded", res)
	}
}

func rowWaitEndsWithTheStopCauseWhenTheSessionStopsFirst(t *testing.T) {
	_, s, fake := turnRuntime(t, harnesstest.Step{Name: "first", Reply: harnesstest.Reply{Text: "working", Block: true}})
	turn := promptTurn(t, s, textInput("a", "go"))
	awaitRequest(t, fake)
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(t.Context()); !errors.Is(err, harness.ErrSessionReleased) || !errors.Is(err, harness.ErrSessionNotOwned) {
		t.Errorf("Wait = %v, want ErrSessionReleased and ErrSessionNotOwned", err)
	}
	for _, err := range turn.Events(t.Context()) {
		if err != nil {
			if !errors.Is(err, harness.ErrSessionReleased) {
				t.Errorf("Events ended with %v, want ErrSessionReleased", err)
			}
			return
		}
	}
	t.Error("Events ended with no error, want ErrSessionReleased")
}
