package eventlog

import (
	"testing"
	"time"

	"github.com/majorcontext/harness/protocol"
)

func exhausted(turn string, resetsAt time.Time) protocol.Payload {
	e := protocol.TurnEnded{TurnID: turn, StopReason: protocol.StopFailed, Cause: protocol.CauseProviderExhausted}
	if !resetsAt.IsZero() {
		e.ResetsAt = resetsAt.Unix()
	}
	return e
}

func usageReset(id string) protocol.Payload {
	return protocol.InputAdmitted{InputID: id, Delivery: protocol.DeliveryQueue, Source: protocol.SourceUsageReset,
		Parts: []protocol.LogPart{{Type: protocol.LogPartEngineContext, Text: "reset"}}}
}

func limited(resetsAt time.Time) []protocol.Payload {
	return with(running, exhausted("t1", resetsAt))
}

func TestContinueAtFollowsAUsageLimitWithAResetTime(t *testing.T) {
	reset := t0.Add(time.Hour)
	for _, tc := range []struct {
		name   string
		events []protocol.Payload
		want   time.Time
	}{
		{"a reset time within a day arms the continue", limited(reset), reset},
		{"a reset time exactly a day away arms the continue", limited(t0.Add(ContinueWindow)), t0.Add(ContinueWindow)},
		{"a reset time in the past arms the continue", limited(t0.Add(-time.Hour)), t0.Add(-time.Hour)},
		{"a reset time past a day arms nothing", limited(t0.Add(ContinueWindow + time.Second)), time.Time{}},
		{"a usage limit with no reset time arms nothing", limited(time.Time{}), time.Time{}},
		{"a turn that completes arms nothing", with(running, end("t1", protocol.StopCompleted, "")), time.Time{}},
		{"a live goal arms nothing", with(base, setGoal, admit("a"), start("t1", "a"), exhausted("t1", reset)), time.Time{}},
		{"an input cancels the continue", with(limited(reset), admit("b")), time.Time{}},
		{"a goal set after the limit takes the continue away for good", with(limited(reset), setGoal, goal(protocol.GoalCleared)), time.Time{}},
		{"a withdrawn input cancels the continue", with(running, admit("w"), exhausted("t1", reset), withdraw("w")), time.Time{}},
		{"a cancel record clears the continue", with(limited(reset), protocol.ContinueCancelled{ContinueAt: reset.Unix()}), time.Time{}},
		{"a limit after a cancel arms again", with(limited(reset), protocol.ContinueCancelled{ContinueAt: reset.Unix()}, admit("b"), start("t2", "b"), exhausted("t2", reset)), reset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := replay(t, tc.events).ContinueAt()
			if ok != !tc.want.IsZero() || !got.Equal(tc.want) {
				t.Fatalf("ContinueAt = %v, %v; want %v", got, ok, tc.want)
			}
		})
	}
}

func TestContinueAtIsNotArmedForAChildSession(t *testing.T) {
	events := []protocol.Payload{protocol.SessionCreated{Model: "openai/gpt-5", Origin: "task", ParentID: "ses_parent"}, admit("a"), start("t1", "a"), exhausted("t1", t0.Add(time.Hour))}
	if at, ok := replay(t, events).ContinueAt(); ok {
		t.Fatalf("ContinueAt = %v, want none for a child", at)
	}
}

func TestTwoUsageResetInputsInARowArmNoThirdContinue(t *testing.T) {
	reset := t0.Add(time.Hour)
	events := with(limited(reset), usageReset("c1"), start("t2", "c1"), exhausted("t2", reset))
	if _, ok := replay(t, events).ContinueAt(); !ok {
		t.Fatal("no continue after the first usage_reset input, want one")
	}
	events = with(events, usageReset("c2"), start("t3", "c2"), exhausted("t3", reset))
	if _, ok := replay(t, events).ContinueAt(); ok {
		t.Fatal("a continue after two usage_reset inputs in a row, want none")
	}
}

func TestAnotherInputOrAnotherEndResetsTheContinueCount(t *testing.T) {
	reset := t0.Add(time.Hour)
	twice := with(limited(reset), usageReset("c1"), start("t2", "c1"), exhausted("t2", reset), usageReset("c2"), start("t3", "c2"))
	after := with(twice, end("t3", protocol.StopCompleted, ""), admit("b"), start("t4", "b"), exhausted("t4", reset))
	if _, ok := replay(t, after).ContinueAt(); !ok {
		t.Fatal("no continue after a completed turn and a new limit, want one")
	}
	steered := with(twice, steer("b"), promote("b", "t3"), exhausted("t3", reset))
	if _, ok := replay(t, steered).ContinueAt(); !ok {
		t.Fatal("no continue after an input of a person reset the count, want one")
	}
}

func TestUsageResetInputRunsFirstAndLiftsTheHold(t *testing.T) {
	s := replay(t, with(limited(t0.Add(time.Hour)), admit("held")))
	if !s.Held() {
		t.Fatal("queue is not held after a usage limit")
	}
	s = replay(t, with(limited(t0.Add(time.Hour)), admit("held"), usageReset("c1")))
	if s.Held() {
		t.Fatal("queue is held with the usage_reset input first")
	}
	if got := s.Progress().Queued; len(got) != 2 || got[0] != "c1" || got[1] != "held" {
		t.Fatalf("Progress.Queued = %v, want [c1 held]", got)
	}
	if q := s.Queue(); len(q) != 2 || q[0].InputID != "c1" {
		t.Fatalf("Queue = %v, want c1 first", q)
	}
}

func TestAPromptThatNamesTheSourceUsageResetIsNotTheRuntimeInput(t *testing.T) {
	in := protocol.InputAdmitted{InputID: "x", Delivery: protocol.DeliveryQueue, Source: protocol.SourceUsageReset, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: "hi"}}}
	if IsUsageReset(in) {
		t.Fatal("a text prompt with source usage_reset is the runtime input")
	}
}

func TestUsageLimitPauseDoesNotCountInPauses(t *testing.T) {
	paused := func(limit bool) protocol.Payload {
		return protocol.GoalChanged{State: protocol.GoalPaused, RetryAt: t0.Add(time.Hour), UsageLimit: limit}
	}
	counted := replay(t, with(base, setGoal, paused(false))).Progress().Goal.Pauses
	free := replay(t, with(base, setGoal, paused(true))).Progress().Goal.Pauses
	if counted != 1 || free != 0 {
		t.Fatalf("pauses = %d counted, %d for a usage limit; want 1 and 0", counted, free)
	}
}

func TestLastTurnCarriesTheResetTime(t *testing.T) {
	reset := t0.Add(time.Hour)
	if got := replay(t, limited(reset)).Progress().LastTurn.ResetsAt; got != reset.Unix() {
		t.Fatalf("LastTurn.ResetsAt = %d, want %d", got, reset.Unix())
	}
}

func TestACancelNamesThePendingContinue(t *testing.T) {
	reset := t0.Add(time.Hour)
	for name, events := range map[string][]protocol.Payload{
		"no continue is pending":      with(running, protocol.ContinueCancelled{ContinueAt: reset.Unix()}),
		"another continue is pending": with(limited(reset), protocol.ContinueCancelled{ContinueAt: reset.Unix() + 1}),
	} {
		var s State
		var err error
		for _, e := range events {
			if err = s.Apply(record(t, s.Head()+1, e)); err != nil {
				break
			}
		}
		if err == nil {
			t.Errorf("%s: continue.cancelled applied, want an illegal transition", name)
		}
	}
}
