package e2e

import (
	"slices"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

func seedInput(id, source, text string) protocol.InputAdmitted {
	return protocol.InputAdmitted{InputID: id, Delivery: protocol.DeliveryQueue, Source: source,
		Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: text}}}
}

func finishedTurn(end protocol.TurnEnded) []protocol.Payload {
	return []protocol.Payload{seedInput("seed_in1", "parent", "brief"), protocol.TurnStarted{TurnID: "turn1", InputIDs: []string{"seed_in1"}},
		protocol.ItemCompleted{ItemID: "m1", TurnID: "turn1", Message: protocol.LogMessage{Role: protocol.RoleAssistant, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: "done"}}}},
		end}
}

func goalModel() []harnesstest.Step {
	return []harnesstest.Step{evaluatorStep("judge", "MET: ok", true), agentStep("work", "done", true)}
}

func TestContractGoalBlocked(t *testing.T) {
	skipShort(t)
	completed := protocol.TurnEnded{TurnID: "turn1", StopReason: protocol.StopCompleted}
	stopped := protocol.TurnEnded{TurnID: "turn1", StopReason: protocol.StopInterrupted, Cause: protocol.CauseStopped}
	exhausted := protocol.TurnEnded{TurnID: "turn1", StopReason: protocol.StopFailed, Cause: protocol.CauseProviderExhausted, Error: "limit"}
	set := protocol.GoalSet{Condition: "finish the brief"}
	join := func(parts ...[]protocol.Payload) []protocol.Payload { return slices.Concat(parts...) }
	onHosts(t, func(t *testing.T, h host) {
		for _, row := range []struct {
			name string
			log  []protocol.Payload
		}{
			{"a_goal_set_after_a_completed_turn_with_no_driver_blocks_at_open", join(finishedTurn(completed),
				[]protocol.Payload{set})},
			{"a_goal_set_after_a_stopped_turn_with_no_driver_blocks_at_open", join(finishedTurn(stopped),
				[]protocol.Payload{set})},
			{"a_goal_activated_with_a_held_goal_input_blocks_at_open", join(finishedTurn(exhausted),
				[]protocol.Payload{set, protocol.GoalChanged{State: protocol.GoalPaused, Reason: "provider_exhausted", RetryAt: time.Now().Add(time.Hour)},
					protocol.GoalChanged{State: protocol.GoalActive}, seedInput("seed_goal1", "goal", "Continue working toward the goal.\n\nGOAL: finish the brief")})},
			{"a_goal_set_after_settled_children_with_no_driver_blocks_at_open", join(finishedTurn(completed),
				[]protocol.Payload{protocol.ChildSpawned{ChildID: "ses_c"}, protocol.ChildSettled{ChildID: "ses_c", Outcome: protocol.OutcomeDone}, set})},
		} {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				d, fake := startOn(t, h, runtimeWorkdir(t, nil), nil, goalModel()...)
				id := d.Create(t)
				d.halt(t)
				d.seed(t, id, row.log...)
				d.start(t)
				g := d.view(t, id).Goal
				if g == nil || g.State != "blocked" || g.Reason != "no_driver" || !g.RetryAt.IsZero() {
					t.Fatalf("goal after open = %+v, want blocked with reason no_driver and no retry_at", g)
				}
				if d.resumable(t, id) {
					t.Error("a blocked goal makes the session resumable")
				}
				changes := d.count(t, id, protocol.KindGoalChanged)
				d.Restart(t, false)
				if n := d.count(t, id, protocol.KindGoalChanged); n != changes {
					t.Errorf("goal.changed records after a second open = %d, want %d", n, changes)
				}
				if n := len(fake.Requests()); n != 0 {
					t.Fatalf("model requests before any input = %d, want 0", n)
				}
				d.Submit(t, id, "go on")
				d.WaitIdle(t, id)
				if g := d.view(t, id).Goal; g == nil || g.State != "achieved" {
					t.Errorf("goal after an input = %+v, want achieved: the input resumed it and the turn was judged", g)
				}
			})
		}

		t.Run("a_goal_set_while_the_queue_is_held_blocks", func(t *testing.T) { goalSetOnHeldQueueBlocks(t, h) })
		t.Run("ending_a_parent_ends_the_goal_of_a_child_with_a_running_turn", func(t *testing.T) { endingParentEndsChildGoal(t, h) })
	})
}

func goalSetOnHeldQueueBlocks(t *testing.T, h host) {
	t.Parallel()
	d, fake := startOn(t, h, runtimeWorkdir(t, nil), nil, goalModel()...)
	id := d.Create(t)
	d.halt(t)
	d.seed(t, id, seedInput("seed_in1", "user", "hello"), protocol.TurnStarted{TurnID: "turn1", InputIDs: []string{"seed_in1"}},
		protocol.TurnEnded{TurnID: "turn1", StopReason: protocol.StopFailed, Cause: protocol.CauseProviderExhausted, Error: "limit"},
		seedInput("seed_in2", "user", "held"))
	d.start(t)
	d.SetGoal(t, id, "finish the brief", 0, false)
	if g := d.view(t, id).Goal; g == nil || g.State != "blocked" || g.Reason != "no_driver" {
		t.Fatalf("goal after SetGoal on a held queue = %+v, want blocked with reason no_driver", g)
	}
	if d.resumable(t, id) {
		t.Error("a blocked goal on a held queue makes the session resumable")
	}
	if n := len(fake.Requests()); n != 0 {
		t.Errorf("model requests = %d, want 0", n)
	}
}

func endingParentEndsChildGoal(t *testing.T, h host) {
	t.Parallel()
	call := harnesstest.ToolCall{ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"}}
	d, fake := startOn(t, h, runtimeWorkdir(t, nil), nil,
		evaluatorStep("judge", "NOT MET: more", true),
		harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{call}}},
		harnesstest.Step{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "partial", Block: true}},
		harnesstest.Step{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
		agentStep("more", "more", true),
	)
	parent := d.Create(t)
	d.Submit(t, parent, "delegate")
	if !fake.AwaitRequests(3, waitBound) {
		t.Fatalf("waited %s for the parent to wait on the child; saw %d requests", waitBound, len(fake.Requests()))
	}
	d.WaitIdle(t, parent)
	var kid string
	for _, id := range d.sessionIDs(t) {
		if id != parent {
			kid = id
		}
	}
	d.SetGoal(t, kid, "finish the child work", 0, false)
	d.EndSession(t, parent)
	if err := d.AwaitTurnEnd(kid); err != nil {
		t.Fatalf("child turn: %v", err)
	}
	v := d.view(t, kid)
	if v.Goal == nil || v.Goal.State != "cleared" || v.Goal.Reason != "ended" {
		t.Errorf("goal of the ended child = %+v, want cleared with reason ended", v.Goal)
	}
	for _, r := range fake.Requests() {
		if isEvaluator(r) {
			t.Error("the evaluator judged the turn of an ended child")
		}
	}
	if v.Status != protocol.StatusIdle {
		t.Errorf("status of the ended child = %s, want idle", v.Status)
	}
}
