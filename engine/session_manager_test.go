package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// blockingProvider streams one event then blocks until release is closed,
// simulating a long-running turn a test wants to cancel or race against.
// Its only mutable-looking field, release, is a channel: safe for
// unsynchronized concurrent use by construction, so sharing one instance
// across several children spawned from the same test is race-free.
type blockingProvider struct {
	name    string
	release chan struct{}
}

func (p *blockingProvider) Name() string { return p.name }

func (p *blockingProvider) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	return &blockingStream{ctx: ctx, release: p.release}, nil
}

type blockingStream struct {
	ctx     context.Context
	release chan struct{}
	done    bool
}

func (s *blockingStream) Next() (provider.Event, error) {
	if s.done {
		return provider.Event{}, errUnreachableStreamEnd
	}
	select {
	case <-s.release:
		s.done = true
		msg := &message.Message{ID: "msg_a", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "ok"}}}
		return provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}, nil
	case <-s.ctx.Done():
		return provider.Event{}, s.ctx.Err()
	}
}

func (s *blockingStream) Close() error { return nil }

var errUnreachableStreamEnd = errors.New("blockingStream: Next called after completion")

// modelFor is a small helper: a ModelRef naming provider p under model "m1".
func modelFor(p string) message.ModelRef {
	return message.ModelRef{Provider: p, Model: "m1"}
}

// scriptedTurns builds a Provider (registered under name) that streams
// turns in order, one per call.
func scriptedTurns(name string, turns [][]provider.Event) provider.Provider {
	return &scriptedProvider{name: name, turns: turns}
}

// doneTurn is a one-shot scripted turn ending with text.
func doneTurn(text string) [][]provider.Event {
	return [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: text})}
}

// managedConfig builds a Config whose Providers map holds every entry in
// providers, keyed by each Provider's own Name(). Every provider a test
// needs — for the root and for every child it plans to spawn with a Model
// override — must be registered UP FRONT, here, before any Spawn call:
// Config.Providers is a plain map inherited by reference into every child's
// Config (see Spawn), so mutating it after a spawned child's goroutine has
// already started reading it races (caught live by this package's own
// -race suite while these tests were written). Routing each child to its
// own scripted behavior via a distinct Model.Provider name, decided before
// Spawn, avoids that entirely.
func managedConfig(model string, providers ...provider.Provider) Config {
	reg := make(provider.Registry, len(providers))
	for _, p := range providers {
		reg[p.Name()] = p
	}
	return Config{
		Providers: reg,
		Model:     modelFor(model),
		System:    []string{"base system"},
	}
}

// signaledBlockingProvider is blockingProvider plus a started signal,
// closed the instant Next is first entered — needed when a test must
// deterministically wait for a turn to actually be mid-flight (not just
// "the goroutine driving it has been scheduled") before racing something
// else against it. Unlike blockingProvider, one instance is single-use
// (started closes via sync.Once), matching this file's established
// per-turn-instance convention elsewhere.
type signaledBlockingProvider struct {
	name    string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *signaledBlockingProvider) Name() string { return p.name }

func (p *signaledBlockingProvider) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	return &signaledBlockingStream{ctx: ctx, p: p}, nil
}

type signaledBlockingStream struct {
	ctx  context.Context
	p    *signaledBlockingProvider
	done bool
}

func (s *signaledBlockingStream) Next() (provider.Event, error) {
	if s.done {
		return provider.Event{}, errUnreachableStreamEnd
	}
	s.p.once.Do(func() { close(s.p.started) })
	select {
	case <-s.p.release:
		s.done = true
		msg := &message.Message{ID: "msg_root", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "ok"}}}
		return provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}, nil
	case <-s.ctx.Done():
		return provider.Event{}, s.ctx.Err()
	}
}

func (s *signaledBlockingStream) Close() error { return nil }

// TestBareSessionManagerRootNeverRacesConcurrentResumeDuringOwnTurn is
// the engine-level regression test for a live-review BLOCKER: a
// SessionManager with no ExternalRunner installed — `harness run`'s bare
// CLI mode (see cmd/harness/main.go's runCmd/runGoal, which now bracket
// their own turn-driving calls with ReportTurnStart/ReportTurnEnd for
// exactly this reason) — must never let a task-spawned child's fast
// completion trigger a SECOND, CONCURRENT call to Session.Prompt on the
// same root while the root's OWN turn (the one that spawned the child)
// is still in flight. Session.Prompt is documented as never safe to call
// concurrently with itself. An earlier revision of runCmd drove its turn
// with a bare, unbracketed s.Prompt call, leaving SessionManager's view
// of the root at StatusIdle for the WHOLE turn — exactly the state
// triggerResumeLocked's no-ExternalRunner fallback (a direct s.Prompt
// call, fired automatically the instant the spawning goroutine's own
// finalizeTurn call returns a non-nil resume) needs to fire. Run under
// -race: a real concurrent Prompt call trips the race detector on
// Session.history/Session.turn even if this test's own assertions
// somehow didn't.
func TestBareSessionManagerRootNeverRacesConcurrentResumeDuringOwnTurn(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0) // no ExternalRunner: bare CLI mode
	rootBlocker := &signaledBlockingProvider{name: "root", started: make(chan struct{}), release: make(chan struct{})}
	root := mgr.NewRoot(managedConfig("root", rootBlocker, scriptedTurns("child", doneTurn("child done"))))

	mgr.ReportTurnStart(root)
	turnDone := make(chan struct{})
	var promptErr error
	go func() {
		defer close(turnDone)
		_, promptErr = root.Prompt(context.Background(), "spawn a child")
	}()
	<-rootBlocker.started

	// Mirrors the model calling `task` mid-turn: a child whose own turn
	// finishes fast, well before root's own turn is released below.
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	// If the bracket is doing its job, root is StatusRunning right now
	// (ReportTurnStart set it, and nothing has cleared it — root's own
	// turn is still blocked above), so finalizeTurn(child) only enqueued
	// a notification — it must NOT have fired triggerResumeLocked, which
	// would call root.Prompt AGAIN, concurrently with the still-in-flight
	// call started above.
	if !root.hasPendingTaskNotifications() {
		t.Error("child's notification was not enqueued on root — finalizeTurn's delivery path is broken independent of the race this test targets")
	}

	close(rootBlocker.release)
	select {
	case <-turnDone:
	case <-time.After(time.Second):
		t.Fatal("root's own turn never completed")
	}
	if promptErr != nil {
		t.Fatalf("root Prompt: %v", promptErr)
	}

	resume := mgr.ReportTurnEnd(root.ID, nil, nil)
	if resume == nil {
		t.Fatal("ReportTurnEnd returned no resume despite a pending notification — the child's result would be stranded")
	}
	resume()
}

func TestSessionManagerNewRootStartsIdle(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))

	info, ok := mgr.Info(root.ID)
	if !ok {
		t.Fatalf("Info(%s) not found", root.ID)
	}
	if info.Status != StatusIdle {
		t.Errorf("status = %s, want idle", info.Status)
	}
	if info.ParentID != "" || info.Depth != 0 {
		t.Errorf("root parent/depth = %q/%d, want empty/0", info.ParentID, info.Depth)
	}
}

func TestSessionManagerSpawnLifecycleDone(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("child result")),
	))

	childID, err := mgr.Spawn(SpawnOptions{
		ParentID: root.ID,
		Prompt:   "explore the repo",
		Model:    modelFor("child"),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	info, _ := mgr.Info(childID)
	if info.ParentID != root.ID {
		t.Errorf("parent = %q, want %q", info.ParentID, root.ID)
	}
	if info.Depth != 1 {
		t.Errorf("depth = %d, want 1", info.Depth)
	}
	if info.Result != "child result" {
		t.Errorf("result = %q, want %q", info.Result, "child result")
	}
	if info.FailReason != "" {
		t.Errorf("failReason = %q, want empty", info.FailReason)
	}

	rootInfo, _ := mgr.Info(root.ID)
	if len(rootInfo.Children) != 1 || rootInfo.Children[0] != childID {
		t.Errorf("root children = %v, want [%s]", rootInfo.Children, childID)
	}
}

func TestSessionManagerSpawnLifecycleFailed(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	// No turns scripted at all: the FIRST Stream call returns
	// io.ErrUnexpectedEOF (see scriptedProvider.Stream), a failure the
	// child's Prompt call surfaces as an error.
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "do work"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	waitForStatus(t, mgr, childID, StatusFailed, 2*time.Second)

	info, _ := mgr.Info(childID)
	if info.FailReason == "" {
		t.Errorf("failReason empty, want a classified reason")
	}
}

func TestSessionManagerDepthLimit(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 1, 0) // depth limit 1
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("done")),
	))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn depth 1: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	// The child is at depth 1, the manager's limit — a further Spawn from
	// it must be rejected with ErrDepthLimit, mirroring the `task` tool
	// being withheld at the limit (Stage 3) but proving the race case still
	// fails cleanly rather than crashing.
	if _, err := mgr.Spawn(SpawnOptions{ParentID: childID, Prompt: "go deeper"}); !errors.Is(err, ErrDepthLimit) {
		t.Errorf("Spawn at depth limit: err = %v, want ErrDepthLimit", err)
	}
}

func TestSessionManagerConcurrencyLimit(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 2) // concurrency limit 2
	release := make(chan struct{})
	blocker := &blockingProvider{name: "blocker", release: release}
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), blocker))

	// Spawn two children that block mid-turn, filling the concurrency
	// budget, then confirm a third is rejected while they're still running.
	var ids []string
	for i := 0; i < 2; i++ {
		id, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "work", Model: modelFor("blocker")})
		if err != nil {
			t.Fatalf("Spawn %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	waitForStatus(t, mgr, ids[0], StatusRunning, time.Second)
	waitForStatus(t, mgr, ids[1], StatusRunning, time.Second)

	if _, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "one too many", Model: modelFor("blocker")}); !errors.Is(err, ErrConcurrencyLimit) {
		t.Errorf("Spawn over limit: err = %v, want ErrConcurrencyLimit", err)
	}

	close(release)
	waitForStatus(t, mgr, ids[0], StatusDone, time.Second)
	waitForStatus(t, mgr, ids[1], StatusDone, time.Second)

	// The budget is freed once both children finish: a further Spawn now
	// succeeds.
	if _, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "now it fits", Model: modelFor("blocker")}); err != nil {
		t.Errorf("Spawn after release: %v", err)
	}
}

// TestSessionManagerConcurrencyLimitRace hammers Spawn from many goroutines
// at once and asserts the manager never admits more than maxConcurrent
// running children — the "a race is still answered with an error, not a
// crash" requirement, exercised under -race.
func TestSessionManagerConcurrencyLimitRace(t *testing.T) {
	const limit = 3
	mgr := NewSessionManager(context.Background(), 0, limit)
	release := make(chan struct{})
	blocker := &blockingProvider{name: "blocker", release: release}
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), blocker))

	const attempts = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	var spawned []string
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "work", Model: modelFor("blocker")})
			if err != nil {
				return
			}
			mu.Lock()
			spawned = append(spawned, id)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(spawned) != limit {
		t.Fatalf("admitted %d spawns, want exactly %d", len(spawned), limit)
	}
	close(release)
	for _, id := range spawned {
		waitForStatus(t, mgr, id, StatusDone, 2*time.Second)
	}
}

func TestSessionManagerCancelCascadesSubtree(t *testing.T) {
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

	// The grandchild is spawned from the child while the child's own turn
	// is still mid-flight (blocked on release) — Spawn only inspects parent
	// bookkeeping (depth/concurrency), not whether the parent's own
	// goroutine happens to be blocked, so this is legal.
	grandID, err := mgr.Spawn(SpawnOptions{ParentID: childID, Prompt: "go deeper", Model: modelFor("grand")})
	if err != nil {
		t.Fatalf("Spawn grandchild: %v", err)
	}
	waitForStatus(t, mgr, grandID, StatusDone, time.Second)

	if err := mgr.Cancel(root.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	rootInfo, _ := mgr.Info(root.ID)
	if rootInfo.Status != StatusCanceled {
		t.Errorf("root status = %s, want canceled", rootInfo.Status)
	}
	waitForStatus(t, mgr, childID, StatusCanceled, time.Second)

	// The grandchild had already reached a terminal state (done) before the
	// cancel — Cancel must leave that recorded outcome alone even though it
	// still walks into it.
	grandInfo, _ := mgr.Info(grandID)
	if grandInfo.Status != StatusDone {
		t.Errorf("grandchild status = %s, want done (cancel must not overwrite a terminal outcome)", grandInfo.Status)
	}

	close(release) // let the child's blocked goroutine observe ctx.Done and exit
}

func TestSessionManagerUnknownParent(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	if _, err := mgr.Spawn(SpawnOptions{ParentID: "ses_doesnotexist", Prompt: "go"}); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("err = %v, want ErrUnknownSession", err)
	}
}

func TestSessionManagerCancelUnknown(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	if err := mgr.Cancel("ses_doesnotexist"); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("err = %v, want ErrUnknownSession", err)
	}
}

func TestSessionManagerSpawnAfterCancelRejected(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))
	if err := mgr.Cancel(root.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go"}); !errors.Is(err, ErrSessionCanceled) {
		t.Errorf("Spawn after cancel: err = %v, want ErrSessionCanceled", err)
	}
}

func TestSessionManagerRestrictToolNames(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("ok")),
	))

	childID, err := mgr.Spawn(SpawnOptions{
		ParentID:  root.ID,
		Prompt:    "look around",
		Model:     modelFor("child"),
		ToolNames: []string{"read_file"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	child, _ := mgr.Session(childID)
	if len(child.tools) != 1 {
		t.Fatalf("child tools = %v, want exactly [read_file]", toolNames(child))
	}
	if _, ok := child.tools["read_file"]; !ok {
		t.Errorf("read_file missing from restricted tool set: %v", toolNames(child))
	}
}

func TestSessionManagerRestrictToolNamesUnknownIsError(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))
	if _, err := mgr.Spawn(SpawnOptions{
		ParentID:  root.ID,
		Prompt:    "go",
		ToolNames: []string{"not_a_real_tool"},
	}); err == nil {
		t.Errorf("Spawn with unknown tool name: want error, got nil")
	}
}

// TestSessionManagerChildLogRecordsParent proves child session logs persist
// under the existing session-log layout with the parent id recorded — the
// design doc's requirement, satisfied by reusing Config.ParentSession
// (already durable via the session header record, see store.go).
func TestSessionManagerChildLogRecordsParent(t *testing.T) {
	dir := t.TempDir()
	mgr := NewSessionManager(context.Background(), 0, 0)
	rootCfg := managedConfig("root", scriptedTurns("root", nil), scriptedTurns("child", doneTurn("ok")))
	rootCfg.SessionDir = dir
	root := mgr.NewRoot(rootCfg)

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	logPath := filepath.Join(dir, childID+".jsonl")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("child session log missing at %s: %v", logPath, err)
	}

	loaded, err := LoadSession(rootCfg, childID)
	if err != nil {
		t.Fatalf("LoadSession(%s): %v", childID, err)
	}
	if loaded.ParentSession() != root.ID {
		t.Errorf("loaded ParentSession = %q, want %q", loaded.ParentSession(), root.ID)
	}
}

func toolNames(s *Session) []string {
	names := make([]string, 0, len(s.tools))
	for n := range s.tools {
		names = append(names, n)
	}
	return names
}

// TestReapRemovesTerminalLeavesAndUpdatesParent proves Reap frees a
// terminal, childless node's *Session and cleans its id out of the
// parent's Children list, and that a parent left childless by reaping
// becomes reapable itself on a later call — a live review flagged
// m.nodes growing unbounded on a long-lived process fanning out many
// `task` children.
// TestReapCancelsNodeContextBeforeRemoving is the regression test for a
// review finding: a naturally completed child (finalizeTurn sets
// Done/Failed — the only path a Reap-eligible leaf reaches without going
// through Cancel/cancelSubtreeLocked, which is the ONLY place that calls
// n.cancel()) never has its own context.CancelFunc invoked. Since every
// child ctx is context.WithCancel(parent.ctx), it registers itself in
// the parent cancelCtx's internal children map; Reap deleting the node
// without calling n.cancel() first drops the last Go-level reference to
// that CancelFunc without ever invoking it, leaking the registration for
// the rest of the root's lifetime — one leaked cancelCtx per completed
// task on a long-lived server, silently defeating part of the
// reclamation Reap exists to provide.
func TestReapCancelsNodeContextBeforeRemoving(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("done")),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	mgr.mu.Lock()
	childCtx := mgr.nodes[childID].ctx
	mgr.mu.Unlock()
	select {
	case <-childCtx.Done():
		t.Fatal("child context already done before Reap — test setup invalid")
	default:
	}

	if n := mgr.Reap(); n != 1 {
		t.Fatalf("Reap() = %d, want 1", n)
	}

	select {
	case <-childCtx.Done():
	default:
		t.Error("child context not canceled by Reap — its CancelFunc registration leaked in the parent's cancelCtx tree")
	}
}

func TestReapRemovesTerminalLeavesAndUpdatesParent(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("done")),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	if n := mgr.Reap(); n != 1 {
		t.Fatalf("Reap() = %d, want 1", n)
	}
	if _, ok := mgr.Info(childID); ok {
		t.Error("child still tracked after Reap")
	}
	rootInfo, ok := mgr.Info(root.ID)
	if !ok {
		t.Fatal("root no longer tracked (roots must never be reaped)")
	}
	if len(rootInfo.Children) != 0 {
		t.Errorf("root Children = %v, want empty after reaping its only child", rootInfo.Children)
	}

	// A second Reap with nothing new terminal is a no-op.
	if n := mgr.Reap(); n != 0 {
		t.Errorf("second Reap() = %d, want 0", n)
	}
}

// TestReapThenReloadRestoresToolRestriction is the regression test for an
// architecture-review BLOCKER: Spawn only ever narrowed the child's
// IN-MEMORY tools map via restrictTools — nothing persisted the agent
// name or resolved tool list, so a reload (Reap, or a process restart)
// followed by a legitimate session.send follow-up reconstructed the
// child via LoadSession's own unconditional FULL default registry, with
// no memory of the restriction at all: an explore child regained
// bash/write_file under its own read-only identity. Proves a reaped
// explore child's read-only set survives a reload exactly.
func TestReapThenReloadRestoresToolRestriction(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("done")),
	))
	childID, err := mgr.Spawn(SpawnOptions{
		ParentID: root.ID, Prompt: "go", Model: modelFor("child"),
		AgentType: AgentExplore, ToolNames: readOnlyTools,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("child not found before reap")
	}
	if _, ok := child.tools["bash"]; ok {
		t.Fatalf("test setup: explore child has bash before reap: %v", toolNames(child))
	}

	if n := mgr.Reap(); n != 1 {
		t.Fatalf("Reap() = %d, want 1", n)
	}
	if _, ok := mgr.Session(childID); ok {
		t.Fatal("child still tracked after Reap — test setup invalid")
	}

	// A REAL reload — a fresh *Session built from the SAME persisted
	// cfg (TaskParentID/TaskAgentType/TaskToolNames included), exactly
	// as LoadSession would reconstruct it — not the same in-memory
	// object, which would trivially keep its already-narrowed tools map
	// regardless of whether the restore logic under test does anything
	// at all. NewSession always installs the FULL default registry
	// unconditionally; only adoptReloadedLocked's own restore logic can
	// narrow it back down from here.
	reloaded := NewSession(child.cfg)
	reloaded.ID = child.ID
	if _, ok := reloaded.tools["bash"]; !ok {
		t.Fatal("test setup: fresh reload object unexpectedly missing bash — NewSession's own full-default-registry behavior changed")
	}

	// Legitimate follow-up on the reaped, done child (session.send's own
	// contract) — mirrors what runPrompt/handleSessionSend's cold-load
	// path does.
	mgr.ReportTurnStart(reloaded)

	if _, ok := reloaded.tools["bash"]; ok {
		t.Errorf("reloaded explore child regained bash: %v — the tool restriction did not survive the reload", toolNames(reloaded))
	}
	if _, ok := reloaded.tools["write_file"]; ok {
		t.Errorf("reloaded explore child regained write_file: %v", toolNames(reloaded))
	}
	for _, want := range readOnlyTools {
		if _, ok := reloaded.tools[want]; !ok {
			t.Errorf("reloaded explore child missing %q, want it retained (readOnlyTools): %v", want, toolNames(reloaded))
		}
	}

	info, ok := mgr.Info(childID)
	if !ok {
		t.Fatal("child not re-adopted")
	}
	if info.AgentType != AgentExplore {
		t.Errorf("lineage.agent_type = %q after reload, want %q — a live review flagged this going blank after a reap", info.AgentType, AgentExplore)
	}
}

// TestReloadedChildWithUnresolvableAgentDefFailsClosed is the regression
// test for the SAME architecture-review blocker's "fail closed" clause:
// a legacy or otherwise-incomplete record (TaskAgentType recorded, but
// TaskToolNames missing — a log written between the two fields'
// rollout, since Spawn now always sets both together going forward) for
// an agent name that no longer resolves to any current definition (the
// custom .agents/*.md file was deleted, or renamed) must not fall back
// to the FULL unrestricted registry — the exact escalation this whole
// fix exists to close. It must restrict to nothing at all instead.
func TestReloadedChildWithUnresolvableAgentDefFailsClosed(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("done")),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("child not found")
	}
	if len(child.tools) == 0 {
		t.Fatal("test setup: child has no tools at all before simulating the incomplete record")
	}

	// Simulate the incomplete-record shape directly: an agent name WAS
	// recorded, but (unlike anything Spawn produces today) no resolved
	// tool list was — the only way TaskToolNames legitimately ends up
	// nil while TaskAgentType is set.
	child.cfg.TaskAgentType = "explore-deleted-by-the-time-this-reloads"
	child.cfg.TaskToolNames = nil

	if n := mgr.Reap(); n != 1 {
		t.Fatalf("Reap() = %d, want 1", n)
	}
	mgr.ReportTurnStart(child)

	if len(child.tools) != 0 {
		t.Errorf("reloaded child with an unresolvable agent def has tools = %v, want none (fail closed)", toolNames(child))
	}
}

// TestReapThenReloadRestoresTrueDepthNotAFreshRoot is the regression test
// for a live-reproduced depth-limit bypass: a child reaped as a terminal,
// childless leaf (the shape a child hits the instant its first turn
// finishes — session.send's own doc comment explicitly permits a
// legitimate later follow-up to a done/failed child) must NOT come back
// as an unrestricted depth-0 root the next time an external scheduler
// reports a turn starting on it. It must be re-attached at its TRUE
// depth, with the depth-based `task` tool restriction that implies
// intact. Depth limit 1 here (mirrors TestTaskToolWithheldAtDepthLimit)
// makes the bug directly observable via tool presence: the child is
// spawned at depth 1, correctly WITHOUT `task`; an earlier revision of
// ReportTurnStart's adopt-on-first-sight always re-adopted an untracked
// id as a depth-0 root, which — at depth 0 — DOES get `task`, letting a
// reaped child regain the ability to spawn further children with no
// depth ceiling at all.
func TestReapThenReloadRestoresTrueDepthNotAFreshRoot(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 1, 0) // depth limit 1
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("done")),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("child not found before reap")
	}
	if _, hasTask := child.tools[taskToolName]; hasTask {
		t.Fatalf("task tool present on depth-1 child before reap (test setup invalid): %v", toolNames(child))
	}

	if n := mgr.Reap(); n != 1 {
		t.Fatalf("Reap() = %d, want 1", n)
	}
	if _, ok := mgr.Session(childID); ok {
		t.Fatal("child still tracked after Reap — test setup invalid")
	}

	// Simulate the legitimate follow-up: an external scheduler (the
	// server's runPrompt, via claimForPrompt's cold-load path, or a
	// session.send hitting handleSessionSend's own cold-load-and-adopt
	// fallback) reports a turn starting on this session, exactly as it
	// would for any session whose SessionManager node Reap already
	// removed.
	mgr.ReportTurnStart(child)

	info, ok := mgr.Info(childID)
	if !ok {
		t.Fatal("child not re-adopted by ReportTurnStart")
	}
	if info.Depth != 1 {
		t.Errorf("re-adopted depth = %d, want 1 (true lineage restored via TaskParentID, not reset to a fresh root)", info.Depth)
	}
	if info.ParentID != root.ID {
		t.Errorf("re-adopted parent = %q, want %q", info.ParentID, root.ID)
	}
	if _, hasTask := child.tools[taskToolName]; hasTask {
		t.Errorf("task tool present on reaped-then-reloaded depth-1 child: %v — the live-reproduced depth-limit bypass", toolNames(child))
	}
}

// TestReloadedChildWithUnknownParentGetsConservativeDepth proves the
// OTHER branch of adoptReloadedLocked: when a child's recorded true
// parent is not tracked by THIS SessionManager either (a fresh process —
// e.g. `harness run -r <id>` naming a former task-tool child from a
// previous process, whose tree lived and died with that process), the
// true depth is unrecoverable, and the safe default is the MOST
// restrictive one (m.maxDepth, refusing `task` outright) rather than the
// MOST permissive one (depth 0, unrestricted) an earlier revision used.
func TestReloadedChildWithUnknownParentGetsConservativeDepth(t *testing.T) {
	mgr1 := NewSessionManager(context.Background(), 3, 0)
	root := mgr1.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("done")),
	))
	childID, err := mgr1.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr1, childID, StatusDone, time.Second)
	child, ok := mgr1.Session(childID)
	if !ok {
		t.Fatal("child not found")
	}

	// A brand new SessionManager (a different process entirely) has never
	// heard of child's true parent: its TaskParentID names an id this
	// manager's tree has no record of at all.
	mgr2 := NewSessionManager(context.Background(), 3, 0)
	mgr2.ReportTurnStart(child)

	info, ok := mgr2.Info(childID)
	if !ok {
		t.Fatal("child not adopted by the second manager")
	}
	if info.Depth != 3 {
		t.Errorf("depth = %d, want 3 (the configured max — refused rather than guessed permissively)", info.Depth)
	}
	if info.ParentID != "" {
		t.Errorf("parent = %q, want empty (true parent unrecoverable in this manager)", info.ParentID)
	}
	if _, hasTask := child.tools[taskToolName]; hasTask {
		t.Errorf("task tool present despite unrecoverable depth: %v", toolNames(child))
	}
}

// TestReportTurnEndNilMsgOnReloadedChildDoesNotPanic is the regression
// test for a review finding: finalizeTurn's default (done) branch
// unconditionally dereferenced msg (n.result = msg.Parts.Text()).
// server's runGoal and cmd/harness's own runGoal both call
// ReportTurnEnd(id, nil, err) unconditionally, documented as safe only
// because "a child is never resident, so runGoal never runs for one" —
// an invariant TestReapThenReloadRestoresTrueDepthNotAFreshRoot's own
// adoptReloadedLocked broke: a reloaded former child whose true parent
// is STILL tracked is re-attached as a genuine depth>0 node (not a
// root), so POST /session/{reapedChildID}/goal cold-loading it and
// calling ReportTurnEnd(id, nil, nil) reaches this exact branch with
// msg == nil, reproduced here directly.
func TestReportTurnEndNilMsgOnReloadedChildDoesNotPanic(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("done")),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("child not found before reap")
	}

	if n := mgr.Reap(); n != 1 {
		t.Fatalf("Reap() = %d, want 1", n)
	}

	// Re-adopted as a genuine depth>0 child, root still tracked (see
	// TestReapThenReloadRestoresTrueDepthNotAFreshRoot).
	mgr.ReportTurnStart(child)
	info, ok := mgr.Info(childID)
	if !ok || info.Depth == 0 {
		t.Fatalf("test setup: child not re-adopted as depth>0, info=%+v ok=%v", info, ok)
	}

	// Mirrors runGoal's own call shape exactly (server/handlers.go,
	// cmd/harness/main.go): msg is always nil.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ReportTurnEnd(id, nil, nil) panicked: %v", r)
			}
		}()
		mgr.ReportTurnEnd(childID, nil, nil)
	}()

	info, ok = mgr.Info(childID)
	if !ok {
		t.Fatal("child no longer tracked after ReportTurnEnd")
	}
	if info.Status != StatusDone {
		t.Errorf("status = %v, want StatusDone", info.Status)
	}
	if info.Result != "" {
		t.Errorf("result = %q, want empty (msg was nil)", info.Result)
	}
}

// TestReportTurnStartBalancesRunningByRootForReloadedChild is the
// regression test for a review finding: ReportTurnStart marks a
// depth>0 node StatusRunning without incrementing runningByRoot, but
// finalizeTurn's decrementRunningLocked decrements it unconditionally
// for any depth>0 node on completion — an unbalanced decrement that
// corrupts the tree-wide concurrency count below the true in-flight
// total, eventually letting Spawn/Send overrun maxConcurrent. Proven via
// the concurrency cap itself: with maxConcurrent 1, a reloaded child
// running via ReportTurnStart must occupy the ONE slot exactly like a
// Spawn-launched child would — a second Spawn attempt while it is
// "running" must be refused, and allowed again once ReportTurnEnd
// finalizes it.
func TestReportTurnStartBalancesRunningByRootForReloadedChild(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 1) // concurrency cap 1
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("done")),
		scriptedTurns("other", doneTurn("other done")),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn child: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("child not found before reap")
	}
	if n := mgr.Reap(); n != 1 {
		t.Fatalf("Reap() = %d, want 1", n)
	}

	mgr.ReportTurnStart(child) // re-adopted as depth>0, running

	if _, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("other")}); !errors.Is(err, ErrConcurrencyLimit) {
		t.Errorf("Spawn while reloaded child occupies the slot: err = %v, want ErrConcurrencyLimit (runningByRoot was never incremented by ReportTurnStart)", err)
	}

	// A second ReportTurnStart on the SAME already-running node (mirrors
	// this method's own documented idempotent no-op case) must not
	// double-increment — the slot stays occupied by exactly one turn,
	// not two.
	mgr.ReportTurnStart(child)
	if _, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("other")}); !errors.Is(err, ErrConcurrencyLimit) {
		t.Errorf("Spawn after a second ReportTurnStart: err = %v, want ErrConcurrencyLimit (still just the one slot)", err)
	}

	resume := mgr.ReportTurnEnd(childID, &message.Message{Parts: message.Parts{&message.Text{Text: "reloaded done"}}}, nil)
	if resume != nil {
		resume()
	}

	otherID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("other")})
	if err != nil {
		t.Fatalf("Spawn after ReportTurnEnd released the slot: %v", err)
	}
	waitForStatus(t, mgr, otherID, StatusDone, time.Second)
}

// slowCancelProvider blocks in Next until its context is canceled, THEN
// keeps blocking a while longer before actually returning — simulating a
// provider call that is "still unwinding" after cancellation rather than
// returning instantly, the narrow-but-real window
// TestReapNeverRemovesACanceledNodeStillUnwinding needs to hold open
// deterministically. ctxDoneSeen closes the instant Next observes
// ctx.Done(), letting a test wait for exactly that moment before making
// any assertion about what Reap does while the goroutine is still
// "in flight."
type slowCancelProvider struct {
	name        string
	ctxDoneSeen chan struct{}
	release     chan struct{}
	once        sync.Once
}

func (p *slowCancelProvider) Name() string { return p.name }

func (p *slowCancelProvider) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	return &slowCancelStream{ctx: ctx, p: p}, nil
}

type slowCancelStream struct {
	ctx context.Context
	p   *slowCancelProvider
}

func (s *slowCancelStream) Next() (provider.Event, error) {
	<-s.ctx.Done()
	s.p.once.Do(func() { close(s.p.ctxDoneSeen) })
	<-s.p.release
	return provider.Event{}, s.ctx.Err()
}

func (s *slowCancelStream) Close() error { return nil }

// twoTurnSlowCancelProvider answers its FIRST Stream call with an
// ordinary, immediate done event, then behaves exactly like
// slowCancelProvider (blocks until ctx.Done(), then keeps blocking on
// release before actually returning) for every call after that —
// letting a test put a child through a real first turn to completion,
// then restart it (Send) for a second turn it can hold open
// deterministically.
type twoTurnSlowCancelProvider struct {
	name      string
	firstDone string
	// slow is a single, persistent *slowCancelProvider reused (by
	// pointer, never copied) for every call after the first — a
	// sync.Once must never be copied by value (go vet's copylocks:
	// sync.Once embeds sync.noCopy), so this is constructed once, here,
	// rather than freshly per call.
	slow *slowCancelProvider
	call int
	mu   sync.Mutex
}

func newTwoTurnSlowCancelProvider(name, firstDone string) *twoTurnSlowCancelProvider {
	return &twoTurnSlowCancelProvider{
		name:      name,
		firstDone: firstDone,
		slow:      &slowCancelProvider{name: name, ctxDoneSeen: make(chan struct{}), release: make(chan struct{})},
	}
}

func (p *twoTurnSlowCancelProvider) Name() string { return p.name }

func (p *twoTurnSlowCancelProvider) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	p.mu.Lock()
	n := p.call
	p.call++
	p.mu.Unlock()
	if n == 0 {
		msg := &message.Message{ID: "msg_first", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: p.firstDone}}}
		// scriptedStream is engine_test.go's own (same package) — reused
		// directly for this first-call reply rather than duplicated.
		return &scriptedStream{events: []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn}}}, nil
	}
	return p.slow.Stream(ctx, req)
}

// TestReapDoesNotLeakConcurrencySlotAcrossSendThenAbort is the
// regression test for a review finding: sessionNode.finalized is set
// true once a child's first turn completes, but nothing ever clears it
// back to false when that SAME child is legitimately restarted for a
// SECOND turn (Send — a done/failed child is eligible for a follow-up
// message, per session.send's own contract). cancelOneNodeLocked only
// sets finalized `if !wasRunning`, so aborting that second turn WHILE it
// is running leaves the stale true from the FIRST turn's completion in
// place. Reap's `!n.finalized` guard then wrongly treats the SECOND
// turn's still-unsettled concurrency reservation as already safe to
// remove: it deletes the node mid-unwind, and the eventual finalizeTurn
// call for that second turn finds the node gone and never decrements —
// runningByRoot stuck inflated forever, wire-reachable via
// session.send -> abort -> the periodic reap ticker.
//
// Proven end-to-end with maxConcurrent 1: after the leak, a fresh Spawn
// under the same root must fail with ErrConcurrencyLimit despite
// NOTHING actually running — the smoking gun. This test asserts the
// opposite: the slot is correctly freed and the fresh Spawn succeeds.
func TestReapDoesNotLeakConcurrencySlotAcrossSendThenAbort(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 1) // concurrency cap 1
	prov := newTwoTurnSlowCancelProvider("child", "first turn done")
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		prov,
		scriptedTurns("other", doneTurn("other done")),
	))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	// Restart the same, now-done child for a legitimate follow-up turn —
	// exactly what session.send permits.
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		mgr.Send(context.Background(), childID, "follow-up") //nolint:errcheck // expected to return a context-canceled error once aborted below
	}()
	select {
	case <-prov.slow.ctxDoneSeen:
		t.Fatal("second turn reported ctx.Done() before it was ever started — test setup invalid")
	case <-time.After(50 * time.Millisecond):
	}
	waitForStatus(t, mgr, childID, StatusRunning, time.Second)

	if err := mgr.AbortTurn(childID); err != nil {
		t.Fatalf("AbortTurn: %v", err)
	}
	select {
	case <-prov.slow.ctxDoneSeen:
	case <-time.After(time.Second):
		t.Fatal("second turn's provider never observed the abort")
	}

	// Still unwinding (blocked on prov.slow.release) — must not be reapable yet.
	if n := mgr.Reap(); n != 0 {
		t.Fatalf("Reap() = %d while the second turn is still unwinding, want 0", n)
	}

	close(prov.slow.release)
	<-sendDone

	deadline := time.Now().Add(time.Second)
	for {
		if n := mgr.Reap(); n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never became reapable after its second turn finished")
		}
		time.Sleep(2 * time.Millisecond)
	}

	otherID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("other")})
	if err != nil {
		t.Fatalf("Spawn after the slot should have been freed: %v — the reservation leaked across the Send-then-abort cycle", err)
	}
	waitForStatus(t, mgr, otherID, StatusDone, time.Second)
}

// TestReapNeverRemovesACanceledNodeStillUnwinding is the regression test
// for a review finding: cancelSubtreeLocked sets a RUNNING child
// StatusCanceled directly but deliberately does NOT decrement
// runningByRoot — finalizeTurn is the sole decrementer, and that
// child's Prompt goroutine is still unwinding its now-canceled provider
// call and will call finalizeTurn itself once that call actually
// returns. If Reap treats this StatusCanceled leaf as eligible before
// that happens, it deletes the node out from under the still-running
// goroutine; when finalizeTurn finally runs, m.nodes[id] is gone,
// finalizeTurn no-ops (see its own "no-op for an id m does not track"
// contract), and decrementRunningLocked never runs — permanently
// leaking that child's runningByRoot reservation. With maxConcurrent 1,
// a leaked reservation manifests directly: nothing can ever Spawn under
// this root again.
func TestReapNeverRemovesACanceledNodeStillUnwinding(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 1) // concurrency cap 1
	prov := &slowCancelProvider{name: "slow", ctxDoneSeen: make(chan struct{}), release: make(chan struct{})}
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), prov, scriptedTurns("fast", doneTurn("fast done"))))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("slow")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusRunning, time.Second)

	if err := mgr.Cancel(childID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-prov.ctxDoneSeen:
	case <-time.After(time.Second):
		t.Fatal("provider never observed cancellation")
	}
	// The goroutine has seen cancellation but is deliberately still
	// blocked in Next (on prov.release) — exactly the "still unwinding"
	// window. The node's own status is already canceled (set
	// synchronously by Cancel), but it must not be reapable yet.
	if info, ok := mgr.Info(childID); !ok || info.Status != StatusCanceled {
		t.Fatalf("test setup: child info = %+v ok=%v, want tracked and StatusCanceled", info, ok)
	}
	if n := mgr.Reap(); n != 0 {
		t.Fatalf("Reap() = %d while the canceled node's turn is still unwinding, want 0 (it would leak the runningByRoot slot)", n)
	}
	if _, ok := mgr.Info(childID); !ok {
		t.Fatal("Reap removed a not-yet-finalized canceled node")
	}

	close(prov.release) // let the goroutine's Prompt call actually return, triggering finalizeTurn

	deadline := time.Now().Add(time.Second)
	for {
		if n := mgr.Reap(); n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never became reapable after its turn finished — finalizeTurn never ran, or never marked it finalized")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The slot is free again: a fresh Spawn under the same root, at the
	// same concurrency cap, must now succeed.
	otherID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("fast")})
	if err != nil {
		t.Fatalf("Spawn after the slot was freed: %v — the reservation leaked", err)
	}
	waitForStatus(t, mgr, otherID, StatusDone, time.Second)
}

// TestReapNeverRemovesRootOrNodeWithChildren proves the two things Reap
// must never do: remove a root (the tree's own address), or remove a
// terminal node that still has a live or terminal-but-not-yet-reaped
// child.
func TestReapNeverRemovesRootOrNodeWithChildren(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("mid", doneTurn("mid done")),
		scriptedTurns("grand", doneTurn("grand done")),
	))

	midID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("mid")})
	if err != nil {
		t.Fatalf("Spawn mid: %v", err)
	}
	waitForStatus(t, mgr, midID, StatusDone, time.Second)
	grandID, err := mgr.Spawn(SpawnOptions{ParentID: midID, Prompt: "go", Model: modelFor("grand")})
	if err != nil {
		t.Fatalf("Spawn grand: %v", err)
	}
	waitForStatus(t, mgr, grandID, StatusDone, time.Second)

	// mid is terminal (done) but has a child (grand) — must survive a
	// Reap that only removes grand (the actual leaf).
	if n := mgr.Reap(); n != 1 {
		t.Fatalf("Reap() = %d, want 1 (only the grandchild leaf)", n)
	}
	if _, ok := mgr.Info(midID); !ok {
		t.Error("mid removed while it still had a child — must survive until childless")
	}
	if _, ok := mgr.Info(grandID); ok {
		t.Error("grand still tracked after Reap")
	}

	// mid is now childless AND terminal — reapable on the next call.
	if n := mgr.Reap(); n != 1 {
		t.Fatalf("second Reap() = %d, want 1 (mid, now a childless leaf)", n)
	}
	if _, ok := mgr.Info(midID); ok {
		t.Error("mid still tracked after becoming a childless leaf")
	}

	// The root itself is now a childless leaf too (its only child, mid,
	// was just reaped) — but a root is never reaped regardless, since it
	// is the tree's own address.
	if n := mgr.Reap(); n != 0 {
		t.Errorf("Reap() removed the root: %d nodes removed", n)
	}
	if _, ok := mgr.Info(root.ID); !ok {
		t.Fatal("root removed by Reap — must never happen")
	}
}

// waitForStatus polls until id reaches want or the timeout elapses.
func waitForStatus(t *testing.T, mgr *SessionManager, id string, want SessionStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		info, ok := mgr.Info(id)
		if !ok {
			t.Fatalf("Info(%s): not found", id)
		}
		if info.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Info(%s).Status = %s after %s, want %s", id, info.Status, timeout, want)
		}
		time.Sleep(time.Millisecond)
	}
}
