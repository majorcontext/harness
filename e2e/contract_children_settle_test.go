package e2e

import (
	"sync"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	treeseam "github.com/majorcontext/harness/internal/tree"
)

// settleGate stops the report of one child after its turn ended and before its
// parent records it, and tells the row when a send waits for that report.
type settleGate struct {
	child               string
	entered, waits      chan struct{}
	release             chan struct{}
	enterOnce, waitOnce sync.Once
	releaseOnceFn       sync.Once
}

func newSettleGate() *settleGate {
	return &settleGate{entered: make(chan struct{}), waits: make(chan struct{}), release: make(chan struct{})}
}

func (g *settleGate) open() { g.releaseOnceFn.Do(func() { close(g.release) }) }

// armSettleGate holds the report of the child as until the gate opens.
type armSettleGate struct {
	g  *settleGate
	as string
}

func (a armSettleGate) run(t *testing.T, r *run) {
	a.g.child = r.id(t, a.as)
	restore := treeseam.SetSeam(&treeseam.Seam{
		Settling: func(_, child string) {
			if child == a.g.child {
				a.g.enterOnce.Do(func() { close(a.g.entered) })
				<-a.g.release
			}
		},
		SendWaits: func(child string) {
			if child == a.g.child {
				a.g.waitOnce.Do(func() { close(a.g.waits) })
			}
		},
	})
	t.Cleanup(func() {
		a.g.open()
		restore()
	})
}

// awaitHeldReport waits until the report of the child is in flight.
type awaitHeldReport struct{ g *settleGate }

func (a awaitHeldReport) run(t *testing.T, _ *run) {
	select {
	case <-a.g.entered:
	case <-t.Context().Done():
		t.Fatal("the report of the child never reached its parent")
	}
}

// releaseAfterSend opens the gate once the send has decided: either the child
// got its nth request, or the send waits for the held report.
type releaseAfterSend struct {
	g *settleGate
	n int
}

func (a releaseAfterSend) run(t *testing.T, r *run) {
	got := make(chan bool, 1)
	go func() { got <- r.fake.AwaitRequests(a.n, waitBound) }()
	select {
	case <-got:
	case <-a.g.waits:
	}
	a.g.open()
}

func TestContractChildSettleSend(t *testing.T) {
	g := newSettleGate()
	runScenarios(t, []scenario{{
		name:       "send_between_the_end_of_a_child_turn_and_its_report_reports_the_second_turn",
		concurrent: true,
		model: []harnesstest.Step{
			taskStep("delegate", userStarts("delegate"), fixed(spawn("general-purpose", "child work"))),
			{Name: "child", Match: userStarts("child work"), Reply: harnesstest.Reply{Text: "first child text", Block: true}},
			{Name: "ack", Match: matchAll(rootStarts("delegate"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Block: true,
				Calls: taskCalls(onKid(func(kid string) []map[string]any {
					return []map[string]any{onSession("send", kid, "prompt", "second work")}
				}))}},
			{Name: "child again", Match: userStarts("second work"), Reply: harnesstest.Reply{Text: "second child text", Block: true}},
			{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
		},
		actions: []action{
			create{as: "a"},
			submit{as: "a", text: "delegate"},
			awaitRequests{n: 3},
			bindChild{as: "kid", parent: "a", record: true},
			armSettleGate{g: g, as: "kid"},
			release{step: "child"},
			awaitHeldReport{g: g},
			release{step: "ack"},
			releaseAfterSend{g: g, n: 4},
			release{step: "child again"},
			awaitSettled{as: "kid", parent: "a"},
			waitIdle{as: "a"},
		},
	}})
}
