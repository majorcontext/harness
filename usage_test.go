package harness_test

import (
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

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
