package harness_test

import (
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

func TestUsageSurvivesAHandoffAndACrash(t *testing.T) {
	before, after := eventlog.Usage{InputTokens: 100, OutputTokens: 10}, eventlog.Usage{InputTokens: 50, OutputTokens: 5}
	for _, tc := range []struct {
		name    string
		handoff bool
	}{{"a handoff", true}, {"a crash", false}} {
		t.Run(tc.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, openStore func() harness.Store) {
				f1, f2 := newFake(), newFake()
				k := killable{make(chan struct{})}
				r1, err := harness.NewWithBackend(harness.Options{Store: openStore(), Owner: k}, f1)
				if err != nil {
					t.Fatal(err)
				}
				submit(t, create(t, r1), text("a", "hi"))
				run := <-f1.runs
				run.measure(before)
				if tc.handoff {
					closeRuntime(t, r1)
				} else {
					close(k.lost)
					synctest.Wait()
				}
				r2 := runtime(t, openStore(), f2)
				s := open(t, r2)
				if got := s.View().Usage; got != (protocol.Usage{InputTokens: 100, OutputTokens: 10}) {
					t.Fatalf("usage after %s = %+v, want the usage of the call before it", tc.name, got)
				}
				if tc.handoff {
					next := <-f2.runs
					next.measure(after)
					next.end()
					if got := s.View().Usage; got != (protocol.Usage{InputTokens: 150, OutputTokens: 15}) {
						t.Fatalf("usage after the resumed turn = %+v, want the sum of both owners", got)
					}
				}
				closeRuntime(t, r1)
				closeRuntime(t, r2)
			})
		})
	}
}

func TestViewsShareNoSubscriptionUsageOrLastTurnWithTheirCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, f := harness.NewMemStore(), newFake()
		r := runtime(t, st, f)
		s := create(t, r)
		submit(t, s, text("a", "hi"))
		run := <-f.runs
		sub := &eventlog.SubscriptionUsage{Provider: "claude", CapturedAt: 7,
			Windows: []eventlog.SubscriptionUsageWindow{{Key: "five_hour", Label: "5-hour", UsedPercent: 40}},
			Overage: &eventlog.SubscriptionOverage{Status: "allowed"}}
		run.tele <- turn.Telemetry{SubscriptionUsage: sub}
		synctest.Wait()
		run.end()
		v, err := harness.OpenView(bg, st, "s1")
		if err != nil {
			t.Fatal(err)
		}
		for name, read := range map[string]func() protocol.Session{"Session.View": s.View, "View.Session": v.Session} {
			got := read()
			got.SubscriptionUsage.Windows[0].Label = "x"
			got.SubscriptionUsage.Overage.Status = "x"
			got.SubscriptionUsage.Plan = "x"
			got.LastTurn.StopReason = "x"
			again := read()
			if again.SubscriptionUsage.Windows[0].Label != "5-hour" || again.SubscriptionUsage.Overage.Status != "allowed" ||
				again.SubscriptionUsage.Plan != "" || again.LastTurn.StopReason != "completed" {
				t.Errorf("%s shares memory with its caller: %+v %+v", name, again.SubscriptionUsage, again.LastTurn)
			}
		}
		closeRuntime(t, r)
	})
}
