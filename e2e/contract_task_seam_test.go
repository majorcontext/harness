package e2e

import (
	"sync"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	treeseam "github.com/majorcontext/harness/internal/tree"
)

// childGate stops one step of the report and send of one child. Settling holds
// the report after the turn ended; Decided holds the send after it decided.
type childGate struct {
	entered, waits, decided chan struct{}
	release, proceed        chan struct{}
	once                    [5]sync.Once
}

func newChildGate() *childGate {
	return &childGate{entered: make(chan struct{}), waits: make(chan struct{}), decided: make(chan struct{}),
		release: make(chan struct{}), proceed: make(chan struct{})}
}

func (g *childGate) openReport() { g.once[3].Do(func() { close(g.release) }) }
func (g *childGate) openSend()   { g.once[4].Do(func() { close(g.proceed) }) }

// childGates maps the ID of a child to its gate. One seam serves every row of
// the process, because the seam is global and the rows run in parallel.
var (
	childGates   sync.Map
	installSeam  sync.Once
	gateOfChild  = func(child string) *childGate { g, _ := childGates.Load(child); gate, _ := g.(*childGate); return gate }
	installGates = func() {
		installSeam.Do(func() {
			treeseam.SetSeam(&treeseam.Seam{
				Settling: func(_, child string) {
					if g := gateOfChild(child); g != nil {
						g.once[0].Do(func() { close(g.entered) })
						<-g.release
					}
				},
				SendWaits: func(child string) {
					if g := gateOfChild(child); g != nil {
						g.once[1].Do(func() { close(g.waits) })
					}
				},
				Decided: func(child string) {
					if g := gateOfChild(child); g != nil {
						g.once[2].Do(func() { close(g.decided) })
						<-g.proceed
					}
				},
			})
		})
	}
)

// armGate puts the gate g on the child as. holdReport holds its report after
// the turn ended; holdSend holds its first send after the send decided.
type armGate struct {
	g                    *childGate
	as                   string
	holdReport, holdSend bool
}

func (a armGate) run(t *testing.T, r *run) {
	installGates()
	id := r.id(t, a.as)
	if !a.holdReport {
		a.g.openReport()
	}
	if !a.holdSend {
		a.g.openSend()
	}
	childGates.Store(id, a.g)
	t.Cleanup(func() {
		a.g.openReport()
		a.g.openSend()
		childGates.Delete(id)
	})
}

// awaitGate waits until the step of the gate is reached.
type awaitGate struct {
	g    *childGate
	step string
}

func (a awaitGate) run(t *testing.T, _ *run) {
	ch := map[string]chan struct{}{"report": a.g.entered, "decided": a.g.decided}[a.step]
	select {
	case <-ch:
	case <-t.Context().Done():
		t.Fatalf("the gate never reached %s", a.step)
	}
}

// releaseAfterSend opens the report once the send has decided: either the
// child got its nth request, or the send waits for the held report. The wait
// for the request ends with the model server.
type releaseAfterSend struct {
	g *childGate
	n int
}

func (a releaseAfterSend) run(t *testing.T, r *run) {
	got := make(chan bool, 1)
	go func() { got <- r.fake.AwaitRequests(a.n, waitBound) }()
	select {
	case <-got:
	case <-a.g.waits:
	}
	a.g.openReport()
}

// openSend lets the held send go on.
type openSend struct{ g *childGate }

func (a openSend) run(*testing.T, *run) { a.g.openSend() }

// heldReportRows are the rows of TestContractTaskSend that hold a step of the
// report or of the send of a child with a seam, so they run on the runtime host only.
func heldReportRows() []scenario {
	inFlight, mid := newChildGate(), newChildGate()
	sendSteps := func() []harnesstest.Step {
		return []harnesstest.Step{
			taskStep("delegate", userStarts("delegate"), fixed(spawn("general-purpose", "child work"))),
			{Name: "child", Match: userStarts("child work"), Reply: harnesstest.Reply{Text: "first child text", Block: true}},
			{Name: "ack", Match: matchAll(rootStarts("delegate"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Block: true,
				Calls: taskCalls(onKid(func(kid string) []map[string]any {
					return []map[string]any{onSession("send", kid, "prompt", "second work")}
				}))}},
			{Name: "child again", Match: userStarts("second work"), Reply: harnesstest.Reply{Text: "second child text", Block: true}},
			{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
		}
	}
	return []scenario{{
		name:       "send_between_the_end_of_a_child_turn_and_its_report_reports_the_second_turn",
		concurrent: true,
		model:      sendSteps(),
		actions: []action{
			create{as: "a"},
			submit{as: "a", text: "delegate"},
			awaitRequests{n: 3},
			bindChild{as: "kid", parent: "a", record: true},
			armGate{g: inFlight, as: "kid", holdReport: true},
			release{step: "child"},
			awaitGate{g: inFlight, step: "report"},
			release{step: "ack"},
			releaseAfterSend{g: inFlight, n: 4},
			release{step: "child again"},
			awaitSettled{as: "kid", parent: "a"},
			waitIdle{as: "a"},
		},
	}, {
		name:       "send_to_a_running_child_whose_turn_ends_before_the_text_lands_reports_the_second_turn",
		concurrent: true,
		model:      sendSteps(),
		actions: []action{
			create{as: "a"},
			submit{as: "a", text: "delegate"},
			awaitRequests{n: 3},
			bindChild{as: "kid", parent: "a", record: true},
			armGate{g: mid, as: "kid", holdSend: true},
			release{step: "ack"},
			awaitGate{g: mid, step: "decided"},
			release{step: "child"},
			awaitSettled{as: "kid", parent: "a"},
			openSend{g: mid},
			release{step: "child again"},
			awaitSettled{as: "kid", parent: "a"},
			waitIdle{as: "a"},
		},
	}}
}
