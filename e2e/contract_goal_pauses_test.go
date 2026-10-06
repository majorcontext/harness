package e2e

import (
	"fmt"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractGoalPauses(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("a_goal_that_keeps_hitting_a_usage_limit_fails_after_six_pauses", func(t *testing.T) {
			t.Parallel()
			const budget = 6
			const before = 2
			wall := func(name string) harnesstest.Step {
				return harnesstest.Step{Name: name, Match: notEvaluator, Reply: harnesstest.Reply{HTTPStatus: 429, ErrorMessage: usageLimitMessage}}
			}
			var steps []harnesstest.Step
			for i := range before {
				steps = append(steps, wall(fmt.Sprintf("early%d", i+1)))
			}
			steps = append(steps, agentStep("work", "partial", false), evaluatorStep("judge", "NOT MET: keep going", false))
			for i := range budget + 1 {
				steps = append(steps, wall(fmt.Sprintf("wall%d", i+1)))
			}
			steps = append(steps, agentStep("after", "done", false))
			d, _ := startOn(t, h, runtimeWorkdir(t, nil), nil, steps...)
			id := d.Create(t)
			d.SetGoal(t, id, "say done", 0, false)
			d.WaitIdle(t, id)
			expectPaused := func(want int) {
				t.Helper()
				if g := d.view(t, id).Goal; g == nil || g.State != "paused" || g.Pauses != want {
					t.Fatalf("goal = %+v, want paused with %d pauses", g, want)
				}
			}
			for want := 1; want <= before; want++ {
				expectPaused(want)
				d.Submit(t, id, fmt.Sprintf("early %d", want))
				d.WaitIdle(t, id)
			}
			for want := 1; want <= budget; want++ {
				expectPaused(want)
				d.Submit(t, id, fmt.Sprintf("again %d", want))
				d.WaitIdle(t, id)
			}
			g := d.view(t, id).Goal
			if g == nil || g.State != "failed" || g.Reason != "retries_exhausted" {
				t.Fatalf("goal after the seventh wall = %+v, want failed with reason retries_exhausted", g)
			}
			d.Submit(t, id, "continue")
			d.WaitIdle(t, id)
		})

		t.Run("a_paused_goal_reports_its_pause_count_and_a_verdict_resets_it", func(t *testing.T) {
			t.Parallel()
			wall := func(name string) harnesstest.Step {
				return harnesstest.Step{Name: name, Match: notEvaluator, Reply: harnesstest.Reply{HTTPStatus: 429, ErrorMessage: usageLimitMessage}}
			}
			d, fake := startOn(t, h, runtimeWorkdir(t, nil), nil,
				wall("wall1"), wall("wall2"),
				agentStep("work", "partial", false),
				evaluatorStep("judge", "NOT MET: keep going", false),
				harnesstest.Step{Name: "blocked", Match: notEvaluator, Reply: harnesstest.Reply{Text: "done", Block: true}},
				evaluatorStep("judge2", "MET: ok", false),
			)
			id := d.Create(t)
			d.SetGoal(t, id, "say done", 0, false)
			d.WaitIdle(t, id)
			if g := d.view(t, id).Goal; g == nil || g.State != "paused" || g.Pauses != 1 {
				t.Fatalf("goal after the first pause = %+v, want paused with 1 pause", g)
			}
			d.Submit(t, id, "again")
			d.WaitIdle(t, id)
			if g := d.view(t, id).Goal; g == nil || g.State != "paused" || g.Pauses != 2 {
				t.Fatalf("goal after the second pause = %+v, want paused with 2 pauses", g)
			}
			d.Submit(t, id, "third")
			if !fake.AwaitRequests(5, waitBound) {
				t.Fatalf("waited %s for the guidance turn; saw %d requests", waitBound, len(fake.Requests()))
			}
			if g := d.view(t, id).Goal; g == nil || g.State != "active" || g.Pauses != 0 {
				t.Errorf("goal after a verdict = %+v, want active with no pauses", g)
			}
			fake.Release("blocked")
			d.WaitIdle(t, id)
		})
	})
}
