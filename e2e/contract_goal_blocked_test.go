package e2e

import (
	"slices"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

// halt stops the host and leaves its store as it is.
func (d *runtimeDriver) halt(t *testing.T) {
	t.Helper()
	if d.serve {
		d.proc.terminate(t)
		return
	}
	d.stop(t, t.Context())
}

// seed appends events to the log of session id in the store of the halted host.
func (d *runtimeDriver) seed(t *testing.T, id string, events ...protocol.Payload) {
	t.Helper()
	st := store.NewDiskStore(d.store)
	head, err := st.Head(t.Context(), id)
	if err != nil {
		t.Fatalf("head of %s: %v", id, err)
	}
	for _, ev := range events {
		head++
		data, err := eventlog.Envelope{Seq: head, Time: time.Now(), Event: ev}.Encode()
		if err != nil {
			t.Fatalf("encode %T: %v", ev, err)
		}
		if err := st.Append(t.Context(), id, head-1, data); err != nil {
			t.Fatalf("append %T: %v", ev, err)
		}
	}
}

// resumable reads the log of session id with no host, as a box does before it wakes the session.
func (d *runtimeDriver) resumable(t *testing.T, id string) bool {
	t.Helper()
	v, err := harness.OpenView(t.Context(), store.NewDiskStore(d.store), id)
	if err != nil {
		t.Fatalf("view %s: %v", id, err)
	}
	return v.Resumable()
}

func (d *runtimeDriver) count(t *testing.T, id string, kind string) int {
	t.Helper()
	n := 0
	for _, e := range d.events(t, id) {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func seedInput(id, source, text string) protocol.InputAdmitted {
	return protocol.InputAdmitted{InputID: id, Delivery: protocol.DeliveryQueue, Source: source,
		Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: text}}}
}

func seedOwners(n int) []protocol.Payload {
	var out []protocol.Payload
	for i := range n {
		out = append(out, protocol.OwnerAcquired{Epoch: uint64(i + 2), Owner: "earlier"})
	}
	return out
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
	set := protocol.GoalSet{Condition: "finish the brief"}
	join := func(parts ...[]protocol.Payload) []protocol.Payload { return slices.Concat(parts...) }
	onHosts(t, func(t *testing.T, h host) {
		for _, row := range []struct {
			name string
			log  []protocol.Payload
		}{
			{"a_goal_set_after_a_completed_turn_with_no_driver_blocks_at_open", join(finishedTurn(completed),
				[]protocol.Payload{protocol.ContextMeasured{Source: "migrated"}, set}, seedOwners(3))},
			{"a_goal_set_after_a_stopped_turn_with_no_driver_blocks_at_open", join(finishedTurn(stopped),
				[]protocol.Payload{protocol.ContextMeasured{Source: "migrated"}, set}, seedOwners(3))},
			{"a_goal_set_after_settled_children_with_no_driver_blocks_at_open", join(finishedTurn(completed),
				[]protocol.Payload{protocol.ChildSpawned{ChildID: "ses_c"}, protocol.ChildSettled{ChildID: "ses_c", Outcome: protocol.OutcomeDone}, set}, seedOwners(4))},
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
				d.Restart(t, false)
				if n := d.count(t, id, protocol.KindGoalChanged); n != 1 {
					t.Errorf("goal.changed records after a second open = %d, want 1", n)
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
