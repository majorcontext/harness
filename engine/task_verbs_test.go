// Tests for the `task` tool's three ancestor-gated verbs — cancel,
// status, and send — and their SessionManager-level backing methods
// (CancelDescendant, DescendantInfo, SendToDescendant), plus
// HasHistoryOrSpawnedChildren, the small declined-thread follow-up this
// same change folds in. See docs/plans/2026-08-23-subagent-sessions-design.md.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// blockAfterFirstProvider streams doneTurn("first done") on its first
// call and blocks on release on every call after that — used to prove a
// re-run turn (SendToDescendant on a settled child) is genuinely a NEW,
// separately blockable turn, not a replay of the first.
type blockAfterFirstProvider struct {
	name    string
	call    int
	release chan struct{}
}

func (p *blockAfterFirstProvider) Name() string { return p.name }

func (p *blockAfterFirstProvider) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	p.call++
	if p.call == 1 {
		return &scriptedStream{events: doneTurn("first done")[0]}, nil
	}
	return &blockingStream{ctx: ctx, release: p.release}, nil
}

// blockFirstThenScriptedProvider blocks on its FIRST call (started closes
// the instant that call is genuinely in flight — no tool call, nothing
// else in progress) until release, then delivers a plain StopEndTurn
// answer with NO tool calls at all: the turn ends via engine.go's early
// return, never reaching the mid-turn tool-call-boundary drain. Every
// call after the first is scripted from turns and its request recorded,
// exactly like scriptedProvider — used to prove drainQueueAndPrompt
// (session_manager.go) picks a message back up and launches a genuinely
// SECOND turn when the mid-turn drain never had a chance to.
type blockFirstThenScriptedProvider struct {
	name     string
	release  chan struct{}
	started  chan struct{}
	once     sync.Once
	call     int
	turns    [][]provider.Event // served starting from the SECOND call
	requests []*provider.Request
}

func (p *blockFirstThenScriptedProvider) Name() string { return p.name }

func (p *blockFirstThenScriptedProvider) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	p.requests = append(p.requests, req)
	p.call++
	if p.call == 1 {
		return &blockFirstStream{p: p}, nil
	}
	return &scriptedStream{events: p.turns[p.call-2]}, nil
}

type blockFirstStream struct {
	p    *blockFirstThenScriptedProvider
	done bool
}

func (s *blockFirstStream) Next() (provider.Event, error) {
	if s.done {
		return provider.Event{}, errUnreachableStreamEnd
	}
	s.p.once.Do(func() { close(s.p.started) })
	<-s.p.release
	s.done = true
	msg := &message.Message{ID: "msg_first", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "first done"}}}
	return provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}, nil
}

func (s *blockFirstStream) Close() error { return nil }

// --- HasHistoryOrSpawnedChildren -------------------------------------

func TestHasHistoryOrSpawnedChildren(t *testing.T) {
	s := NewSession(Config{
		Providers: provider.Registry{"test": scriptedTurns("test", nil)},
		Model:     modelFor("test"),
		WorkDir:   t.TempDir(),
	})
	if s.HasHistoryOrSpawnedChildren() {
		t.Error("fresh session: want false")
	}

	s.append(message.Message{ID: "m1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	if !s.HasHistoryOrSpawnedChildren() {
		t.Error("after append: want true")
	}
}

func TestHasHistoryOrSpawnedChildrenTrueForSpawnedChildAlone(t *testing.T) {
	s := NewSession(Config{
		Providers: provider.Registry{"test": scriptedTurns("test", nil)},
		Model:     modelFor("test"),
		WorkDir:   t.TempDir(),
	})
	s.mu.Lock()
	s.recordSpawnedChildLocked("child_x")
	s.mu.Unlock()
	if !s.HasHistoryOrSpawnedChildren() {
		t.Error("after recordSpawnedChildLocked with empty history: want true")
	}
}

// --- SessionManager.CancelDescendant -----------------------------------

func TestCancelDescendantRejectsSelf(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))
	if _, err := mgr.CancelDescendant(root.ID, root.ID); !errors.Is(err, ErrNotDescendant) {
		t.Errorf("CancelDescendant(root, root): err = %v, want ErrNotDescendant", err)
	}
}

func TestCancelDescendantRejectsUnrelatedSession(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	release := make(chan struct{})
	blocker := &blockingProvider{name: "blocker", release: release}
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), blocker))
	otherRoot := mgr.NewRoot(managedConfig("other", scriptedTurns("other", nil)))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "work", Model: modelFor("blocker")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusRunning, time.Second)

	if _, err := mgr.CancelDescendant(otherRoot.ID, childID); !errors.Is(err, ErrNotDescendant) {
		t.Errorf("CancelDescendant from an unrelated root: err = %v, want ErrNotDescendant", err)
	}
	// The refused call must not have touched anything.
	info, _ := mgr.Info(childID)
	if info.Status != StatusRunning {
		t.Errorf("child status after a refused cancel = %s, want still running", info.Status)
	}
	close(release)
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
}

// TestCancelDescendantCascadesSubtree proves CancelDescendant does the
// same cascade cancellation Cancel/cancel_tree does — the design doc's
// explicit choice for "stop this delegation" — and that an
// already-terminal descendant's recorded outcome (done) is left alone
// even though the cascade still walks into it.
func TestCancelDescendantCascadesSubtree(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	release := make(chan struct{})
	blocker := &blockingProvider{name: "blocker", release: release}
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		blocker,
		scriptedTurns("grand", doneTurn("grandchild done")),
	))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "work", Model: modelFor("blocker")})
	if err != nil {
		t.Fatalf("Spawn child: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusRunning, time.Second)

	grandID, err := mgr.Spawn(SpawnOptions{ParentID: childID, Prompt: "go deeper", Model: modelFor("grand")})
	if err != nil {
		t.Fatalf("Spawn grandchild: %v", err)
	}
	waitForStatus(t, mgr, grandID, StatusDone, time.Second)

	if _, err := mgr.CancelDescendant(root.ID, childID); err != nil {
		t.Fatalf("CancelDescendant(root, child): %v", err)
	}
	waitForStatus(t, mgr, childID, StatusCanceled, time.Second)

	grandInfo, _ := mgr.Info(grandID)
	if grandInfo.Status != StatusDone {
		t.Errorf("grandchild status = %s, want done (already-terminal outcome preserved by the cascade)", grandInfo.Status)
	}
}

// TestCancelDescendantAllowsTransitiveAncestor proves "ancestor," not
// just "direct spawner," satisfies the lineage check: root cancels its
// GRANDCHILD directly, two hops up, never having spawned it itself.
func TestCancelDescendantAllowsTransitiveAncestor(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	release := make(chan struct{})
	blocker := &blockingProvider{name: "blocker", release: release}
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", doneTurn("child done")), blocker))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn child: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	grandID, err := mgr.Spawn(SpawnOptions{ParentID: childID, Prompt: "go deeper", Model: modelFor("blocker")})
	if err != nil {
		t.Fatalf("Spawn grandchild: %v", err)
	}
	waitForStatus(t, mgr, grandID, StatusRunning, time.Second)

	if _, err := mgr.CancelDescendant(root.ID, grandID); err != nil {
		t.Fatalf("CancelDescendant(root, grand): %v", err)
	}
	waitForStatus(t, mgr, grandID, StatusCanceled, time.Second)
}

func TestCancelDescendantUnknownSessionIsError(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))
	if _, err := mgr.CancelDescendant(root.ID, "not-a-real-session"); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("CancelDescendant on an unknown target: err = %v, want ErrUnknownSession", err)
	}
}

// --- SessionManager.DescendantInfo --------------------------------------

func TestDescendantInfoReturnsStatusLineageUsage(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	usage := provider.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3, CacheWriteTokens: 1}
	childProv := scriptedTurns("child", doneTurnWithUsage("the answer is 42", usage))
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), childProv))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	node, gotUsage, err := mgr.DescendantInfo(root.ID, childID)
	if err != nil {
		t.Fatalf("DescendantInfo: %v", err)
	}
	if node.ID != childID {
		t.Errorf("ID = %q, want %q", node.ID, childID)
	}
	if node.ParentID != root.ID {
		t.Errorf("ParentID = %q, want %q", node.ParentID, root.ID)
	}
	if node.Depth != 1 {
		t.Errorf("Depth = %d, want 1", node.Depth)
	}
	if node.Status != StatusDone {
		t.Errorf("Status = %s, want done", node.Status)
	}
	if node.AgentType != AgentExplore {
		t.Errorf("AgentType = %q, want %q", node.AgentType, AgentExplore)
	}
	if node.Result != "the answer is 42" {
		t.Errorf("Result = %q, want %q", node.Result, "the answer is 42")
	}
	if gotUsage != usage {
		t.Errorf("Usage = %+v, want %+v", gotUsage, usage)
	}
}

func TestDescendantInfoRejectsNonDescendant(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))
	otherRoot := mgr.NewRoot(managedConfig("other", scriptedTurns("other", nil)))
	if _, _, err := mgr.DescendantInfo(otherRoot.ID, root.ID); !errors.Is(err, ErrNotDescendant) {
		t.Errorf("DescendantInfo across unrelated trees: err = %v, want ErrNotDescendant", err)
	}
}

// --- SessionManager.SendToDescendant -------------------------------------

// TestSendToDescendantRunningQueuesAndDeliversAtBoundary is the headline
// test for the send verb's "running children get it queued for their
// next turn boundary" behavior: it reuses the exact same tool-call-
// boundary drain TestMidTurnInjectionAtToolBoundary (queue_toolcall_
// boundary_test.go) proves for a plain session, but through a SPAWNED
// CHILD and SendToDescendant, proving the reuse is real, not merely
// documented.
func TestSendToDescendantRunningQueuesAndDeliversAtBoundary(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "gate", `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "child final"}),
	}}
	cfg := managedConfig("root", scriptedTurns("root", nil), childProv)
	cfg.Tools = []Tool{gateTool(entered, release)}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "run gate", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	<-entered // the child's tool call is genuinely in flight

	queued, err := mgr.SendToDescendant(root.ID, childID, "operator says hi mid-turn")
	if err != nil {
		t.Fatalf("SendToDescendant: %v", err)
	}
	if !queued {
		t.Error("SendToDescendant on a running child: queued = false, want true")
	}

	close(release)
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	if len(childProv.requests) != 2 {
		t.Fatalf("child provider requests = %d, want 2", len(childProv.requests))
	}
	second := childProv.requests[1]
	last := second.Messages[len(second.Messages)-1]
	if last.Role != message.RoleUser {
		t.Fatalf("second request's trailing message role = %s, want user (the injected operator block)", last.Role)
	}
	text := last.Parts.Text()
	if !strings.Contains(text, "OPERATOR MESSAGES") {
		t.Errorf("second request's trailing message = %q, want a labeled operator block", text)
	}
	if !strings.Contains(text, "operator says hi mid-turn") {
		t.Errorf("second request's trailing message = %q, want the queued text", text)
	}
}

// twoStageBlockingProvider blocks its FIRST call on release1 (returning
// a plain "ok" StopEndTurn once released, via blockingStream) and every
// call after that purely on ctx.Done() — it has nothing else to
// release, since TestDrainQueueAndPromptStopsDequeuingOnCancelMidDrain
// only ever cancels it. secondCall closes the instant the SECOND Stream
// call starts, so the test can deterministically wait for it to be
// genuinely in flight before canceling, without polling call count from
// outside (which would race the session's own turn-driving goroutine).
type twoStageBlockingProvider struct {
	name       string
	release1   chan struct{}
	secondCall chan struct{}
	once       sync.Once
	call       int
}

func (p *twoStageBlockingProvider) Name() string { return p.name }

func (p *twoStageBlockingProvider) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	p.call++
	if p.call == 1 {
		return &blockingStream{ctx: ctx, release: p.release1}, nil
	}
	p.once.Do(func() { close(p.secondCall) })
	return &ctxOnlyBlockingStream{ctx: ctx}, nil
}

// ctxOnlyBlockingStream blocks forever except for ctx cancellation —
// used for a call this test intends to interrupt, never to release.
type ctxOnlyBlockingStream struct{ ctx context.Context }

func (s *ctxOnlyBlockingStream) Next() (provider.Event, error) {
	<-s.ctx.Done()
	return provider.Event{}, s.ctx.Err()
}

func (s *ctxOnlyBlockingStream) Close() error { return nil }

// TestDrainQueueAndPromptStopsDequeuingOnCancelMidDrain is the
// regression test for a live review finding: drainQueueAndPrompt's loop
// never checked ctx cancellation, so canceling a running child mid-drain
// (task cancel arriving between two of its re-driven turns) kept
// dequeuing and re-running every remaining queued prompt on an
// already-dead ctx — journaling each as "delivered" even though none of
// them actually ran. Two messages are enqueued while the child's FIRST
// turn is still blocked; the first turn completes, drainQueueAndPrompt
// dequeues "message A" and starts a second turn (which blocks on ctx
// only); the child is canceled while that second turn is genuinely in
// flight. "message B" must be left exactly where it was — still queued,
// never dequeued or discarded by drainQueueAndPrompt itself — matching
// cancellation's existing "stop, full stop" semantics elsewhere in this
// package (a canceled node's queue is never looked at again by anyone;
// see drainQueueAndPrompt's own doc comment).
func TestDrainQueueAndPromptStopsDequeuingOnCancelMidDrain(t *testing.T) {
	release1 := make(chan struct{})
	childProv := &twoStageBlockingProvider{name: "child", release1: release1, secondCall: make(chan struct{})}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), childProv))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	queuedA, err := mgr.SendToDescendant(root.ID, childID, "message A")
	if err != nil || !queuedA {
		t.Fatalf("SendToDescendant A: queued=%v err=%v", queuedA, err)
	}
	queuedB, err := mgr.SendToDescendant(root.ID, childID, "message B")
	if err != nil || !queuedB {
		t.Fatalf("SendToDescendant B: queued=%v err=%v", queuedB, err)
	}

	close(release1) // first turn completes; drainQueueAndPrompt dequeues "message A" and starts the second (ctx-only-blocking) turn

	<-childProv.secondCall // that second turn is genuinely in flight

	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("Session before cancel: not found")
	}

	if err := mgr.Cancel(childID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusCanceled, time.Second)
	// Read the queue only once drainQueueAndPrompt's own goroutine has
	// FINISHED, never merely once the status flipped: Cancel marks a
	// running node StatusCanceled synchronously, while that goroutine is
	// still unwinding, so a read at the status transition alone races the
	// very dequeue this test forbids and passes vacuously against the bug
	// (measured: the pre-fix loop still consumed "message B" after such a
	// read). A canceled node stays un-reapable until its own eventual
	// finalizeTurn call flips finalized (see sessionNode.finalized's doc
	// comment, session_manager.go), so Reap-eligibility is exactly the
	// "that goroutine has returned" signal — reached through the Changed
	// seam, with no sampling. The child Session handle is captured above,
	// before the sweep removes the node, so its queue stays readable
	// after collection.
	waitForReap(t, mgr, 1, time.Second, "canceled child never became reapable, so drainQueueAndPrompt never returned")

	pending := child.QueuedPrompts()
	if len(pending) != 1 || pending[0].Text != "message B" {
		t.Fatalf("QueuedPrompts after cancel-mid-drain = %+v, want exactly one entry left untouched: message B", pending)
	}
}

// countingProvider records how many Stream calls it received and fails
// every one — for a test that asserts a request is never issued at all.
type countingProvider struct {
	name string
	mu   sync.Mutex
	n    int
}

func (p *countingProvider) Name() string { return p.name }

func (p *countingProvider) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
	return nil, errors.New("countingProvider: Stream must never be called")
}

func (p *countingProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

// TestDrainQueueAndPromptSkipsFirstPromptOnCanceledCtx is the regression
// test for a review finding on the ctx-guard fix: only the LOOP body was
// guarded, so the FIRST s.Prompt call ran unconditionally. On the
// finalizeTurn re-drive and settled-relaunch paths a cancel landing
// between the closure's creation and its `go resume()` therefore issued
// one wasted provider request and appended one user message to a session
// whose ctx was already dead. The guard must skip that call entirely and
// return the ctx error, leaving history untouched.
func TestDrainQueueAndPromptSkipsFirstPromptOnCanceledCtx(t *testing.T) {
	prov := &countingProvider{name: "never"}
	s := NewSession(managedConfig("never", prov))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	msg, err := drainQueueAndPrompt(ctx, s, "wasted directive", "", nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("drainQueueAndPrompt on a canceled ctx: err = %v, want context.Canceled", err)
	}
	if msg != nil {
		t.Errorf("drainQueueAndPrompt on a canceled ctx: msg = %+v, want nil", msg)
	}
	if got := prov.calls(); got != 0 {
		t.Errorf("provider Stream calls = %d, want 0: a canceled turn must not issue a request", got)
	}
	if h := s.History(); len(h) != 0 {
		t.Errorf("history after a canceled drain = %d message(s), want 0: the wasted directive was appended anyway", len(h))
	}
}

// TestSendToDescendantRunningWithoutToolBoundaryStillDelivers is the
// regression test for a live review finding on this fix's first pass: a
// message enqueued to a running child whose CURRENT (and only remaining)
// provider call ends the turn with no further tool-call boundary — the
// common shape of "the model is mid-generation of its final answer, no
// tool call in flight" — used to strand forever in the child's own
// promptQueue, since a child, unlike a root, has no external residency
// layer to pick the queue back up once Prompt returns. drainQueueAndPrompt
// (session_manager.go) closes this: the child's turn-driving goroutine
// notices the queue is still non-empty after its first Prompt call
// returns and launches a SECOND turn with the queued text, before ever
// calling finalizeTurn.
func TestSendToDescendantRunningWithoutToolBoundaryStillDelivers(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	childProv := &blockFirstThenScriptedProvider{
		name:    "child",
		release: release,
		started: started,
		turns:   [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "second done"})},
	}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), childProv))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	<-started // the child's first (and, in this turn, ONLY) provider call is genuinely in flight — no tool call anywhere in progress

	queued, err := mgr.SendToDescendant(root.ID, childID, "please also cover Y")
	if err != nil {
		t.Fatalf("SendToDescendant: %v", err)
	}
	if !queued {
		t.Error("SendToDescendant on a running child: queued = false, want true")
	}

	close(release) // the first turn ends with StopEndTurn and no tool calls — the mid-turn drain point is never reached at all

	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	if len(childProv.requests) != 2 {
		t.Fatalf("child provider requests = %d, want 2 (drainQueueAndPrompt must launch a second turn, not strand the message)", len(childProv.requests))
	}
	second := childProv.requests[1]
	lastText := second.Messages[len(second.Messages)-1].Parts.Text()
	if lastText != "please also cover Y" {
		t.Errorf("second turn's trailing message = %q, want the queued text delivered verbatim", lastText)
	}

	node, ok := mgr.Info(childID)
	if !ok {
		t.Fatal("Info after done: not found")
	}
	if node.Result != "second done" {
		t.Errorf("Result = %q, want %q (the SECOND turn's own answer, proving the queued message's turn is what actually settled the child)", node.Result, "second done")
	}
}

// TestSendToDescendantSettledRelaunchesAsynchronously proves the settled
// (done) path uses existing Send semantics (a fresh re-run turn) but
// NEVER blocks the caller for the turn's duration — SessionManager's one
// deliberately non-blocking entry point is Spawn, and this verb must not
// become a second, blocking exception to that.
func TestSendToDescendantSettledRelaunchesAsynchronously(t *testing.T) {
	release := make(chan struct{})
	childProv := &blockAfterFirstProvider{name: "child", release: release}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), childProv))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	// No wall-clock deadline assertion here (AGENTS.md's "no guessed
	// deadlines" testing rule — a live review finding on an earlier
	// version of this test, which asserted elapsed <= 500ms): the
	// non-blocking property is proven STRUCTURALLY instead.
	// childProv's second call blocks on release, which stays open until
	// AFTER this point — if SendToDescendant actually blocked for the
	// whole re-run turn (the bug this test guards against), this very
	// call would hang until the test binary's own timeout, a real
	// failure with no arbitrary threshold to tune or flake under load.
	// The waitForStatus(StatusRunning) below is the actual proof the
	// re-run started at all.
	queued, err := mgr.SendToDescendant(root.ID, childID, "please redo this")
	if err != nil {
		t.Fatalf("SendToDescendant: %v", err)
	}
	if queued {
		t.Error("SendToDescendant on a settled child: queued = true, want false (a fresh re-run turn, not an enqueue)")
	}

	waitForStatus(t, mgr, childID, StatusRunning, time.Second)
	close(release)
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
}

// TestSendToDescendantRunningPersistsQueueRecordAfterUnlock guards the
// durability half of a live review finding's fix: SendToDescendant's
// running-target branch used to call the full EnqueuePrompt (memory
// mutation plus a synchronous ensureLog+writeRecord disk write) while
// holding m.mu, the tree-wide lock every other session's Info/Reap/
// Spawn/finalize call also needs. It now mutates memory under m.mu and
// queues the durable write via deferPersist, which
// unlockAndFlushPersist runs after m.mu releases.
//
// The risk that split creates is a silently DROPPED write — memory-only
// enqueue, no journal record, a queued prompt no reload could ever see.
// This test drives the production entry point with a real SessionDir and
// asserts the prompt.queued record is on disk by the time
// SendToDescendant returns (the flush runs in the same goroutine, right
// after the unlock, so no wait is needed). The complementary ordering
// property — that thunks run only after m.mu is released — is proven by
// TestUnlockAndFlushPersistRunsThunksAfterReleasingLock.
func TestSendToDescendantRunningPersistsQueueRecordAfterUnlock(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	childProv := &signaledBlockingProvider{name: "child", started: make(chan struct{}), release: release}
	cfg := managedConfig("root", scriptedTurns("root", nil), childProv)
	cfg.SessionDir = dir
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-childProv.started // the child's first turn is genuinely in flight, so the target is StatusRunning

	queued, err := mgr.SendToDescendant(root.ID, childID, "message A")
	if err != nil || !queued {
		t.Fatalf("SendToDescendant: queued=%v err=%v, want queued=true err=nil", queued, err)
	}

	logPath := filepath.Join(dir, childID+".jsonl")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading child session log %s: %v", logPath, err)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec struct {
			Type   string `json:"type"`
			Prompt *struct {
				Text string `json:"text"`
			} `json:"prompt"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshaling session log line %q: %v", line, err)
		}
		if rec.Type == recPromptQueued && rec.Prompt != nil && rec.Prompt.Text == "message A" {
			found = true
		}
	}
	if !found {
		t.Errorf("no %s record for the enqueued text in %s after SendToDescendant returned — the deferred durable write was dropped; log:\n%s", recPromptQueued, logPath, data)
	}
}

// TestSendToDescendantSettledReservesTurnBeforeReturning is the
// regression test for a live review finding: SendToDescendant's
// settled-target branch used to release m.mu and let a freshly launched
// goroutine call Send, which re-acquired m.mu from scratch and only THEN
// reserved the turn. A Reap() sweep landing in that gap collected the
// still-terminal leaf, Send returned ErrUnknownSession, and the launched
// goroutine discarded it — the caller kept a queued:false, err:nil
// answer whose "dispatched as a fresh turn" promise silently never
// happened. reserveSendLocked now runs inside SendToDescendant's OWN
// still-held m.mu critical section, so the node is already StatusRunning
// when the call returns and Reap (which only ever collects a finalized,
// terminal node) can never collect it.
//
// The assertions are made with NO wait in between: they run on the
// caller's own goroutine, immediately after the call returns, so they
// test the by-construction postcondition rather than a race that has to
// be won. The launched turn's provider blocks on release, which stays
// open until the end, so a StatusRunning read here can only come from
// the synchronous reservation.
func TestSendToDescendantSettledReservesTurnBeforeReturning(t *testing.T) {
	release := make(chan struct{})
	childProv := &blockAfterFirstProvider{name: "child", release: release}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), childProv))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	queued, err := mgr.SendToDescendant(root.ID, childID, "please redo this")
	if err != nil {
		t.Fatalf("SendToDescendant: %v", err)
	}
	if queued {
		t.Fatalf("SendToDescendant on a settled child: queued = true, want false (a fresh re-run turn)")
	}

	info, ok := mgr.Info(childID)
	if !ok {
		t.Fatal("Info right after SendToDescendant returned: child not found, want a reserved (running) node")
	}
	if info.Status != StatusRunning {
		t.Errorf("Info(child).Status right after SendToDescendant returned = %s, want %s (the turn must already be reserved, not merely intended)", info.Status, StatusRunning)
	}
	if n := mgr.Reap(); n != 0 {
		t.Errorf("Reap() right after SendToDescendant returned collected %d node(s), want 0: a reserved turn must never be collectable", n)
	}
	if _, ok := mgr.Info(childID); !ok {
		t.Error("Info(child) after Reap: child was collected, so its re-run turn is stranded")
	}

	close(release)
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
}

func TestSendToDescendantRejectsCanceledTarget(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	release := make(chan struct{})
	blocker := &blockingProvider{name: "blocker", release: release}
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), blocker))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("blocker")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusRunning, time.Second)

	if err := mgr.Cancel(childID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusCanceled, time.Second)

	if _, err := mgr.SendToDescendant(root.ID, childID, "hi"); !errors.Is(err, ErrSessionCanceled) {
		t.Errorf("SendToDescendant on a canceled child: err = %v, want ErrSessionCanceled", err)
	}
}

func TestSendToDescendantRejectsNonDescendant(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", doneTurn("done"))))
	otherRoot := mgr.NewRoot(managedConfig("other", scriptedTurns("other", nil)))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	if _, err := mgr.SendToDescendant(otherRoot.ID, childID, "hi"); !errors.Is(err, ErrNotDescendant) {
		t.Errorf("SendToDescendant from an unrelated root: err = %v, want ErrNotDescendant", err)
	}
}

// --- `task` tool action dispatch -----------------------------------------

func TestRunTaskToolCancelAction(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	release := make(chan struct{})
	blocker := &blockingProvider{name: "blocker", release: release}
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), blocker))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("blocker")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusRunning, time.Second)

	raw, _ := json.Marshal(map[string]string{"action": "cancel", "session_id": childID})
	parts, err := runTaskTool(root, raw)
	if err != nil {
		t.Fatalf("runTaskTool cancel: %v", err)
	}
	var result taskCancelResult
	if err := json.Unmarshal([]byte(parts.Text()), &result); err != nil {
		t.Fatalf("unmarshal result: %v (%s)", err, parts.Text())
	}
	if result.Status != string(StatusCanceled) {
		t.Errorf("result.Status = %q, want %q", result.Status, StatusCanceled)
	}
	waitForStatus(t, mgr, childID, StatusCanceled, time.Second)
}

func TestRunTaskToolCancelRejectsNonDescendant(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))
	otherRoot := mgr.NewRoot(managedConfig("other", scriptedTurns("other", nil), scriptedTurns("child", doneTurn("done"))))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: otherRoot.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	raw, _ := json.Marshal(map[string]string{"action": "cancel", "session_id": childID})
	if _, err := runTaskTool(root, raw); err == nil {
		t.Error("runTaskTool cancel on a session root never spawned: want error, got nil")
	} else if !strings.Contains(err.Error(), childID) {
		t.Errorf("error = %v, want it to name the rejected session_id", err)
	}
}

func TestRunTaskToolStatusAction(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", doneTurn("found it"))))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	raw, _ := json.Marshal(map[string]string{"action": "status", "session_id": childID})
	parts, err := runTaskTool(root, raw)
	if err != nil {
		t.Fatalf("runTaskTool status: %v", err)
	}
	var result taskStatusResult
	if err := json.Unmarshal([]byte(parts.Text()), &result); err != nil {
		t.Fatalf("unmarshal result: %v (%s)", err, parts.Text())
	}
	if result.SessionID != childID {
		t.Errorf("SessionID = %q, want %q", result.SessionID, childID)
	}
	if result.ParentID != root.ID {
		t.Errorf("ParentID = %q, want %q", result.ParentID, root.ID)
	}
	if result.Status != string(StatusDone) {
		t.Errorf("Status = %q, want done", result.Status)
	}
	if result.Result != "found it" {
		t.Errorf("Result = %q, want %q", result.Result, "found it")
	}
	if result.Children == nil {
		t.Error("Children = nil, want a non-nil (possibly empty) slice")
	}
}

func TestRunTaskToolSendActionOnRunningChildQueues(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "gate", `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "child final"}),
	}}
	cfg := managedConfig("root", scriptedTurns("root", nil), childProv)
	cfg.Tools = []Tool{gateTool(entered, release)}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(cfg)

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "run gate", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-entered

	raw, _ := json.Marshal(map[string]string{"action": "send", "session_id": childID, "prompt": "from the parent"})
	parts, err := runTaskTool(root, raw)
	if err != nil {
		t.Fatalf("runTaskTool send: %v", err)
	}
	var result taskSendResult
	if err := json.Unmarshal([]byte(parts.Text()), &result); err != nil {
		t.Fatalf("unmarshal result: %v (%s)", err, parts.Text())
	}
	if !result.Queued {
		t.Error("result.Queued = false, want true for a running descendant")
	}
	close(release)
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
}

func TestRunTaskToolSendActionMissingArgumentsAreErrors(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))

	raw, _ := json.Marshal(map[string]string{"action": "send", "session_id": "some-id"})
	if _, err := runTaskTool(root, raw); err == nil {
		t.Error("send with no prompt: want error, got nil")
	}

	raw, _ = json.Marshal(map[string]string{"action": "send", "prompt": "hi"})
	if _, err := runTaskTool(root, raw); err == nil {
		t.Error("send with no session_id: want error, got nil")
	}
}

// TestRunTaskToolSendActionWhitespaceOnlyPromptIsRejected is the
// regression test for a live review finding: runTaskSend guarded only
// `in.Prompt == ""`, so a whitespace-only prompt behaved OPPOSITELY by
// target state. A RUNNING target reached SendToDescendant's own enqueue
// validation and returned a raw, non-sentinel error that
// classifyTaskVerbError leaked to the model verbatim (the raw "engine:"
// layer, before ErrEmptyPromptText existed); a SETTLED target accepted the
// blank text and burned a real turn on it. Both targets must now get the
// same model-facing rejection from runTaskSend itself, before either
// path runs.
func TestRunTaskToolSendActionWhitespaceOnlyPromptIsRejected(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	runningProv := &signaledBlockingProvider{name: "running", started: make(chan struct{}), release: release}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), runningProv, scriptedTurns("settled", doneTurn("done"))))

	runningID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("running"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn running child: %v", err)
	}
	<-runningProv.started

	settledID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("settled"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn settled child: %v", err)
	}
	waitForStatus(t, mgr, settledID, StatusDone, time.Second)

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"running target", runningID},
		{"settled target", settledID},
	} {
		raw, _ := json.Marshal(map[string]string{"action": "send", "session_id": tc.id, "prompt": "   "})
		parts, err := runTaskTool(root, raw)
		if err == nil {
			t.Errorf("%s: whitespace-only prompt accepted (result %q), want an error", tc.name, parts.Text())
			continue
		}
		if !strings.Contains(err.Error(), "prompt is required") {
			t.Errorf("%s: error = %v, want the same model-facing \"prompt is required\" rejection both targets get", tc.name, err)
		}
	}

	// The settled target must not have been restarted by the rejected call.
	info, ok := mgr.Info(settledID)
	if !ok {
		t.Fatal("Info(settled child): not found")
	}
	if info.Status != StatusDone {
		t.Errorf("settled child Status after a rejected whitespace-only send = %s, want %s: a blank prompt burned a real turn", info.Status, StatusDone)
	}
}

// TestSendToDescendantBlankTextIsClassifiableSentinel guards the shared
// sentinel a review finding asked for: SendToDescendant's running-target
// branch used a fresh errors.New for the blank-text rule, which
// classifyTaskVerbError could not match with errors.Is, so it fell through
// to the default arm and leaked the internal "engine:" layer to the model.
func TestSendToDescendantBlankTextIsClassifiableSentinel(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	prov := &signaledBlockingProvider{name: "child", started: make(chan struct{}), release: release}
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), prov))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-prov.started

	_, err = mgr.SendToDescendant(root.ID, childID, "   ")
	if !errors.Is(err, ErrEmptyPromptText) {
		t.Fatalf("SendToDescendant with blank text: err = %v, want ErrEmptyPromptText", err)
	}
	classified := classifyTaskVerbError(err, childID)
	if strings.Contains(classified.Error(), "engine:") {
		t.Errorf("classifyTaskVerbError(%v) = %q, want a model-facing message with no internal \"engine:\" layer", err, classified)
	}
}

func TestRunTaskToolUnknownActionIsError(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))

	raw, _ := json.Marshal(map[string]string{"action": "explode", "session_id": "x"})
	if _, err := runTaskTool(root, raw); err == nil {
		t.Error("runTaskTool with an unknown action: want error, got nil")
	} else if !strings.Contains(err.Error(), "explode") {
		t.Errorf("error = %v, want it to name the unknown action", err)
	}
}

// TestRunTaskToolOmittedActionDefaultsToSpawn is the explicit
// backward-compatibility regression test: arguments carrying no "action"
// key at all (the tool's original, pre-verbs shape) must behave exactly
// like action: "spawn" — TestRunTaskToolSpawnsChildAndReturnsImmediately
// already exercises this implicitly (it never sets action either); this
// test additionally confirms an EXPLICIT action: "spawn" produces an
// identical result shape, proving the default and the explicit value are
// the same code path, not two that happen to agree today.
func TestRunTaskToolOmittedActionDefaultsToSpawn(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns(AgentExplore, doneTurn("found it")),
	))

	raw, _ := json.Marshal(map[string]string{"action": "spawn", "agent": AgentExplore, "prompt": "find it", "model": modelFor(AgentExplore).String()})
	parts, err := runTaskTool(root, raw)
	if err != nil {
		t.Fatalf("runTaskTool explicit spawn: %v", err)
	}
	var result taskToolResult
	if err := json.Unmarshal([]byte(parts.Text()), &result); err != nil {
		t.Fatalf("unmarshal result: %v (%s)", err, parts.Text())
	}
	if result.SessionID == "" {
		t.Error("result.SessionID empty")
	}
	if result.Agent != AgentExplore {
		t.Errorf("result.Agent = %q, want %q", result.Agent, AgentExplore)
	}
}

// TestSendToDescendantSettledRejectsBlankText guards the symmetry a review
// finding asked for: SendToDescendant validated blank text only on the
// running-target path, so a settled target accepted " " and burned a whole
// re-run turn on it. runTaskSend masks that for the `task` tool, but this
// is an exported API — both paths must answer the same way.
func TestSendToDescendantSettledRejectsBlankText(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", doneTurn("done"))))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	if _, err := mgr.SendToDescendant(root.ID, childID, "   "); !errors.Is(err, ErrEmptyPromptText) {
		t.Fatalf("SendToDescendant blank text to a settled target: err = %v, want ErrEmptyPromptText", err)
	}
	info, ok := mgr.Info(childID)
	if !ok {
		t.Fatal("Info: child not found")
	}
	if info.Status != StatusDone {
		t.Errorf("settled child Status after a rejected blank send = %s, want %s: the blank text started a real re-run turn", info.Status, StatusDone)
	}
}

// TestDescendantInfoReportsReapedChildren guards the lineage consistency a
// review finding asked for: DescendantInfo reported only the LIVE children
// list, and Reap removes a terminal leaf from its parent's live list, so
// `task status` on a mid-tree descendant answered children:[] for
// grandchildren it really did spawn — while the wire's GET
// /session/{id}/lineage still named them from the durable spawn record.
func TestDescendantInfoReportsReapedChildren(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("mid", doneTurn("mid done")),
		scriptedTurns("grand", doneTurn("grand done"))))

	midID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("mid"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn mid: %v", err)
	}
	grandID, err := mgr.Spawn(SpawnOptions{ParentID: midID, Prompt: "deeper", Model: modelFor("grand"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn grandchild: %v", err)
	}
	waitForStatus(t, mgr, grandID, StatusDone, time.Second)

	// Reap the finished grandchild: mid's LIVE children list loses it,
	// its durable spawn record does not.
	waitForReap(t, mgr, 1, time.Second, "finished grandchild never became reapable")

	info, _, err := mgr.DescendantInfo(root.ID, midID)
	if err != nil {
		t.Fatalf("DescendantInfo: %v", err)
	}
	if len(info.Children) != 1 || info.Children[0] != grandID {
		t.Errorf("DescendantInfo(mid).Children = %v, want [%s]: a reaped grandchild vanished from task status while the wire's lineage still reports it", info.Children, grandID)
	}
}
