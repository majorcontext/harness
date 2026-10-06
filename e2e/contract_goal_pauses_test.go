package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractGoalPauses(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
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
