package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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

// streamSignalProvider behaves exactly like scriptedTurns(name, nil) — a
// provider with no scripted turn, whose every Stream call fails at once —
// except that it also reports each call on streamed. It is the
// synchronization seam for "this session's turn has really started": a
// turn reaches Stream strictly after SessionManager.ReportTurnStart has
// marked the node StatusRunning, so a receive from streamed proves the
// node is past that transition without sampling its status.
type streamSignalProvider struct {
	name     string
	streamed chan struct{}
}

func (p *streamSignalProvider) Name() string { return p.name }

func (p *streamSignalProvider) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	select {
	case p.streamed <- struct{}{}:
	default:
	}
	return nil, io.ErrUnexpectedEOF
}

// doneTurn is a one-shot scripted turn ending with text.
func doneTurn(text string) [][]provider.Event {
	return [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: text})}
}

// doneTurnWithUsage is doneTurn plus an explicit Usage on the terminal
// event — for tests exercising per-tree token budget accounting
// (SessionManager.SetMaxTreeTokens), where asstTurn's own zero-Usage
// default is not useful.
func doneTurnWithUsage(text string, usage provider.Usage) [][]provider.Event {
	turn := asstTurn(provider.StopEndTurn, &message.Text{Text: text})
	turn[0].Usage = usage
	return [][]provider.Event{turn}
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

// TestSessionAndInfoMatchesSeparateCalls proves SessionAndInfo (Server.lookup's
// one-lock replacement for a separate Session-then-Info pair) returns
// exactly what those two calls would, for both a known and an unknown id.
func TestSessionAndInfoMatchesSeparateCalls(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))

	sess, info, ok := mgr.SessionAndInfo(root.ID)
	if !ok {
		t.Fatalf("SessionAndInfo(%s) not found", root.ID)
	}
	wantSess, sessOK := mgr.Session(root.ID)
	wantInfo, infoOK := mgr.Info(root.ID)
	if !sessOK || !infoOK {
		t.Fatalf("Session/Info(%s) not found", root.ID)
	}
	if sess != wantSess {
		t.Errorf("SessionAndInfo session = %p, want %p (Session's own)", sess, wantSess)
	}
	if !reflect.DeepEqual(info, wantInfo) {
		t.Errorf("SessionAndInfo info = %+v, want %+v (Info's own)", info, wantInfo)
	}

	if _, _, ok := mgr.SessionAndInfo("ses_unknown"); ok {
		t.Error("SessionAndInfo(unknown id) ok = true, want false")
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

// TestReloadedChildWithEmptyIntersectionRestrictionStaysEmpty is the
// regression test for a live review finding on store.go's TaskToolNames
// persistence: `omitempty` on that field collapsed BOTH "no restriction
// recorded" (nil) AND a real, deliberate ZERO-tool restriction (a
// non-nil, len-0 slice — reachable via Spawn's parent-effective-set
// INTERSECTION whenever a restricted parent's tools and a child
// definition's tools are disjoint) to the identical omitted-field wire
// shape. On an ACTUAL reload through the store (not the in-memory
// NewSession(child.cfg) shortcut most of this file's other reload tests
// use — that shortcut never touches the JSON marshal/unmarshal this bug
// lives in), restoreTaskToolRestrictionLocked saw the resulting nil
// TaskToolNames and fell back to re-resolving TaskAgentType's
// definition directly — re-granting that definition's OWN tools (here,
// bash) with NO parent intersection at all: a write/exec tool escaping
// a restriction the child's restricted parent could never have granted
// in the first place.
func TestReloadedChildWithEmptyIntersectionRestrictionStaysEmpty(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A definition disjoint from the restricted parent's own tools below
	// (read_file, task) — the shape that makes Spawn's intersection
	// produce a non-nil, EMPTY slice rather than a merely-narrowed one.
	defContent := "---\n" +
		"name: bash-only\n" +
		"description: A definition whose only tool the restricted parent never had\n" +
		"tools: bash\n" +
		"---\n" +
		"A bash-only agent.\n"
	if err := os.WriteFile(filepath.Join(agentsDir, "bash-only.md"), []byte(defContent), 0o644); err != nil {
		t.Fatal(err)
	}

	rootProv := &scriptedProvider{name: "root"}
	midProv := &scriptedProvider{name: "mid", turns: doneTurn("mid done")}
	childProv := &scriptedProvider{name: "child", turns: doneTurn("child done")}
	cfg := Config{
		Providers:  provider.Registry{rootProv.Name(): rootProv, midProv.Name(): midProv, childProv.Name(): childProv},
		Model:      modelFor("root"),
		WorkDir:    dir,
		SessionDir: dir,
	}
	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(cfg)

	// A restricted, non-leaf parent: read_file and task only — the exact
	// shape the review named.
	midID, err := mgr.Spawn(SpawnOptions{
		ParentID: root.ID, Prompt: "go", Model: modelFor("mid"),
		AgentType: "custom-read-and-spawn",
		ToolNames: []string{"read_file", taskToolName},
	})
	if err != nil {
		t.Fatalf("Spawn mid: %v", err)
	}
	waitForStatus(t, mgr, midID, StatusDone, time.Second)

	// A child spawned FROM mid, naming the disjoint bash-only definition
	// — the intersection of mid's {read_file, task} with bash-only's
	// {bash} is empty, so the child ends up with ZERO tools, and
	// child.cfg.TaskToolNames is a non-nil, empty slice (session_manager.go's
	// Spawn: `narrowed := make([]string, 0, ...)`, never touched again
	// when nothing matches).
	childID, err := mgr.Spawn(SpawnOptions{
		ParentID: midID, Prompt: "go", Model: modelFor("child"),
		AgentType: "bash-only", ToolNames: []string{"bash"},
	})
	if err != nil {
		t.Fatalf("Spawn child: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
	child, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("child not found before reap")
	}
	if len(child.tools) != 0 {
		t.Fatalf("test setup: child has tools before reap, want none from the disjoint intersection: %v", toolNames(child))
	}

	if n := mgr.Reap(); n != 1 {
		t.Fatalf("Reap() = %d, want 1", n)
	}
	if _, ok := mgr.Session(childID); ok {
		t.Fatal("child still tracked after Reap — test setup invalid")
	}

	// A REAL reload through the store — LoadSession, not the in-memory
	// NewSession(child.cfg) shortcut other reload tests in this file use
	// — this is the ONLY way to exercise the JSON marshal/unmarshal round
	// trip the omitempty bug lived in.
	reloaded, err := LoadSession(cfg, childID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	mgr.ReportTurnStart(reloaded)

	if got := toolNames(reloaded); len(got) != 0 {
		t.Errorf("reloaded child has tools = %v, want none — its restricted parent never had bash to grant, and the child's own persisted restriction was zero tools", got)
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

// TestReloadedChildWithUnknownParentUsesDurableTaskDepth is the regression
// test for a live audit finding (the "singleton 3" bug): a live-reproduced
// GET /session/{id}.lineage.depth on a genuine DIRECT child (true depth 1)
// reported 3 == DefaultMaxTaskDepth, indistinguishable from a session
// genuinely refused at the depth limit — while every OTHER child (real
// grandchildren included) reported a flatly wrong 0. Root cause: THIS
// exact scenario — adoptReloadedLocked's "child's recorded true parent is
// not tracked by THIS SessionManager either" branch (a fresh process, or
// — the far more common live shape — an ancestor that Reap already
// collected while this specific child stayed live/re-touched) — used to
// substitute m.maxDepth, a deliberate REFUSAL SENTINEL for "true depth is
// unrecoverable", with no way for a caller to tell that sentinel apart
// from a session genuinely AT that depth.
//
// It no longer needs to guess: Config.TaskDepth (set by Spawn, durably
// persisted and restored by LoadSession exactly like TaskParentID/
// TaskAgentType already were — see that field's own doc comment) records
// the child's real depth at spawn time, so this branch now uses it
// whenever it is present (> 0) instead of falling back to the sentinel.
// TestReloadedChildWithUnknownParentAndNoDurableDepthStaysConservative
// covers the complementary legacy case (TaskDepth never recorded), where
// the OLD conservative sentinel behavior this test used to assert still
// applies unchanged.
func TestReloadedChildWithUnknownParentUsesDurableTaskDepth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		if d := child.TaskDepth(); d != 1 {
			t.Fatalf("child.TaskDepth() = %d, want 1 (test setup invalid)", d)
		}

		// A brand new SessionManager (a different process entirely, or this
		// SAME process after Reap collected the root while this child stayed
		// live) has never heard of child's true parent: its TaskParentID
		// names an id this manager's tree has no record of at all.
		mgr2 := NewSessionManager(context.Background(), 3, 0)
		mgr2.ReportTurnStart(child)

		info, ok := mgr2.Info(childID)
		if !ok {
			t.Fatal("child not adopted by the second manager")
		}
		if info.Depth != 1 {
			t.Errorf("depth = %d, want 1 (the child's own durable TaskDepth, not the m.maxDepth=3 refusal sentinel)", info.Depth)
		}
		if info.ParentID != "" {
			t.Errorf("parent = %q, want empty (true parent id itself is still unrecoverable in this manager — only depth is)", info.ParentID)
		}
		// A real depth-1 child, 2 below the depth-3 limit, correctly regains
		// the task tool — the OLD sentinel-based fallback wrongly withheld
		// it from every such child (true depth < limit) whose immediate
		// parent simply wasn't tracked in this particular process.
		if _, hasTask := child.tools[taskToolName]; !hasTask {
			t.Errorf("task tool withheld from a real depth-1 child (limit 3): %v", toolNames(child))
		}
	})
}

// TestReloadedChildWithUnknownParentAndNoDurableDepthStaysConservative
// covers the legacy/backward-compat half of adoptReloadedLocked's "parent
// not tracked" branch: a session predating Config.TaskDepth (or one whose
// header the field was for any other reason never recorded on) reports
// TaskDepth() == 0 — indistinguishable, by construction, from "never
// spawned a child" — so this branch must NOT trust it, and must fall back
// to the exact same m.maxDepth refusal sentinel it always used, preserving
// the original safety invariant this test used to cover before
// TestReloadedChildWithUnknownParentUsesDurableTaskDepth's fix: refusing
// an unverifiable depth is always safer than guessing permissively.
func TestReloadedChildWithUnknownParentAndNoDurableDepthStaysConservative(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		// Simulate a legacy record: TaskDepth was never persisted for this
		// session (the field predates it, or this log was written between
		// TaskParentID's own rollout and TaskDepth's — see TaskAgentType's
		// doc comment for the same already-accepted rollout gap). Poking
		// cfg directly is safe here: child has not been exposed to any
		// other goroutine since Spawn returned, and this test's own
		// mgr2.ReportTurnStart call below is the first thing to touch it.
		child.cfg.TaskDepth = 0

		mgr2 := NewSessionManager(context.Background(), 3, 0)
		mgr2.ReportTurnStart(child)

		info, ok := mgr2.Info(childID)
		if !ok {
			t.Fatal("child not adopted by the second manager")
		}
		if info.Depth != 3 {
			t.Errorf("depth = %d, want 3 (the configured max — refused rather than guessed permissively, exactly as before TaskDepth existed)", info.Depth)
		}
		if info.ParentID != "" {
			t.Errorf("parent = %q, want empty (true parent unrecoverable in this manager)", info.ParentID)
		}
		if _, hasTask := child.tools[taskToolName]; hasTask {
			t.Errorf("task tool present despite unrecoverable depth: %v", toolNames(child))
		}
	})
}

// TestReloadedChildDurableDepthWinsOverBadTrackedParentDepth is a
// review-driven addition: proves adoptReloadedLocked's depth derivation
// checks the CHILD's own durable TaskDepth first, even when its parent is
// currently tracked — not just when the parent is untracked (the case
// TestReloadedChildWithUnknownParentUsesDurableTaskDepth already covers).
//
// Reproduces a mixed legacy/non-legacy tree across a rollout: "mid" is a
// legacy node (predates Config.TaskDepth) whose OWN parent is not tracked
// in this manager, so it gets adopted at the m.maxDepth refusal sentinel
// (5 here) — a WRONG depth for mid, but the best this manager can do
// without a durable TaskDepth to fall back on. "child" is mid's own real,
// non-legacy child, spawned in an earlier, correctly-functioning process
// with its true depth (2) durably recorded. Reloading child while mid IS
// tracked used to compute depth = mid.depth+1 = 6 (the sentinel,
// propagated forward and off by one), discarding child's own known-correct
// value and silently denying it the task tool (TaskToolAllowed(6) is
// always false at maxDepth 5) even though its true depth (2) is well
// under the limit. child's own durable TaskDepth now wins regardless.
func TestReloadedChildDurableDepthWinsOverBadTrackedParentDepth(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 5, 0) // maxDepth 5

	midCfg := managedConfig("mid", scriptedTurns("mid", nil))
	midCfg.TaskParentID = "ses_0000000000000099" // untracked in this manager
	mid := NewSession(midCfg)
	if err := mgr.AdoptReloaded(mid); err != nil {
		t.Fatalf("AdoptReloaded(mid): %v", err)
	}
	midInfo, ok := mgr.Info(mid.ID)
	if !ok {
		t.Fatal("mid not adopted")
	}
	if midInfo.Depth != 5 {
		t.Fatalf("mid depth = %d, want 5 (the sentinel — test setup invalid)", midInfo.Depth)
	}

	childCfg := managedConfig("child", scriptedTurns("child", nil))
	childCfg.TaskParentID = mid.ID
	childCfg.TaskDepth = 2 // real, durably recorded depth from an earlier process
	child := NewSession(childCfg)
	if err := mgr.AdoptReloaded(child); err != nil {
		t.Fatalf("AdoptReloaded(child): %v", err)
	}

	info, ok := mgr.Info(child.ID)
	if !ok {
		t.Fatal("child not adopted")
	}
	if info.Depth != 2 {
		t.Errorf("child depth = %d, want 2 (its own durable TaskDepth, not mid.depth+1 = %d)", info.Depth, midInfo.Depth+1)
	}
	if info.ParentID != mid.ID {
		t.Errorf("child parent = %q, want %q (mid IS tracked, so the live attach still applies)", info.ParentID, mid.ID)
	}
	if _, hasTask := child.tools[taskToolName]; !hasTask {
		t.Errorf("task tool withheld from child (true depth 2, limit 5): %v", toolNames(child))
	}
}

// TestTaskDepthHeaderRoundTrip is a review-driven addition, mirroring
// TestParentSessionLegacyHeaderCompat/TestParentSessionHeaderRoundTrip
// (parent_session_test.go): proves Config.TaskDepth's own restore rule
// directly against a REAL on-disk header, not just an in-memory *Session
// object poked by a live Spawn call — the two prior tests above only
// exercise TaskDepth() on an object Spawn itself just constructed, never a
// genuinely reloaded one. A legacy header with no "task_depth" key at all
// restores TaskDepth() == 0; a header that DOES carry it restores the
// exact persisted value, regardless of what the loading Config supplies.
func TestTaskDepthHeaderRoundTrip(t *testing.T) {
	dir := t.TempDir()

	legacyID := "ses_4444444444444446"
	legacyData := `{"type":"session","id":"ses_4444444444444446","created_at":"2025-01-02T03:04:05Z","task_parent_id":"ses_0000000000000001","task_agent_type":"reviewer"}
{"type":"model","model":"test/m1"}
`
	if err := os.WriteFile(filepath.Join(dir, legacyID+".jsonl"), []byte(legacyData), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{SessionDir: dir, Model: message.ModelRef{Provider: "test", Model: "m1"}}
	legacy, err := LoadSession(cfg, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if got := legacy.TaskDepth(); got != 0 {
		t.Errorf("legacy header (no task_depth key) TaskDepth() = %d, want 0", got)
	}

	recordedID := "ses_4444444444444447"
	recordedData := `{"type":"session","id":"ses_4444444444444447","created_at":"2025-01-02T03:04:05Z","task_parent_id":"ses_0000000000000001","task_agent_type":"reviewer","task_depth":2}
{"type":"model","model":"test/m1"}
`
	if err := os.WriteFile(filepath.Join(dir, recordedID+".jsonl"), []byte(recordedData), 0o644); err != nil {
		t.Fatal(err)
	}
	recorded, err := LoadSession(cfg, recordedID)
	if err != nil {
		t.Fatal(err)
	}
	if got := recorded.TaskDepth(); got != 2 {
		t.Errorf("header with task_depth:2 restored TaskDepth() = %d, want 2", got)
	}

	// Review finding: a legacy child (no task_depth key) loaded under a
	// Config whose OWN TaskDepth is already non-zero must still restore
	// to 0, not silently inherit that value. This is exactly the shape
	// recoverCrashedChildrenLocked produces in production —
	// configSnapshot() copies Config BY VALUE from the parent node
	// currently being adopted, TaskDepth included, before calling
	// LoadSession for each of that parent's own candidate children — so
	// a genuinely legacy child would otherwise inherit its PARENT's depth
	// instead of correctly falling back to adoptReloadedLocked's own
	// m.maxDepth refusal sentinel.
	inheritedCfg := cfg
	inheritedCfg.TaskDepth = 5 // simulates a live parent's own configSnapshot
	legacyUnderInheritedCfg, err := LoadSession(inheritedCfg, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if got := legacyUnderInheritedCfg.TaskDepth(); got != 0 {
		t.Errorf("legacy child loaded under a Config with inherited TaskDepth=5 restored TaskDepth() = %d, want 0 (reset, not inherited)", got)
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
//
// started closes the instant Stream is first entered, and is optional
// (only closed when a test supplies it). A test must wait on it before
// canceling: a node reads StatusRunning from the moment Spawn reserves
// its slot, which is BEFORE the launched goroutine has called Prompt at
// all, and drainQueueAndPrompt starts no turn whose ctx is already
// canceled (see its own doc comment). Waiting on the node status alone
// therefore does not prove this provider will ever be entered.
type slowCancelProvider struct {
	name        string
	started     chan struct{}
	ctxDoneSeen chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	once        sync.Once
}

func (p *slowCancelProvider) Name() string { return p.name }

func (p *slowCancelProvider) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	if p.started != nil {
		p.startOnce.Do(func() { close(p.started) })
	}
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

	waitForReap(t, mgr, 1, time.Second, "node never became reapable after its second turn finished")

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
	prov := &slowCancelProvider{name: "slow", started: make(chan struct{}), ctxDoneSeen: make(chan struct{}), release: make(chan struct{})}
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), prov, scriptedTurns("fast", doneTurn("fast done"))))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("slow")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusRunning, time.Second)
	// Wait for the provider call itself, not just the reserved slot:
	// Spawn sets StatusRunning before its launched goroutine ever calls
	// Prompt, and drainQueueAndPrompt starts no turn on an
	// already-canceled ctx — so a Cancel racing ahead of that first call
	// would leave this provider never entered and ctxDoneSeen never
	// closed.
	<-prov.started

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

	waitForReap(t, mgr, 1, time.Second, "node never became reapable after its turn finished — finalizeTurn never ran, or never marked it finalized")

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

// TestForgetRootRemovesIdleRootWithNoChildren proves ForgetRoot's happy
// path: a childless, non-running root is removed from m.nodes — the
// follow-up fix for the leak Reap's own doc comment describes as a
// deliberate v1 scope cut (a root is never automatically reaped).
func TestForgetRootRemovesIdleRootWithNoChildren(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))

	if err := mgr.ForgetRoot(root.ID); err != nil {
		t.Fatalf("ForgetRoot: %v", err)
	}
	if _, ok := mgr.Info(root.ID); ok {
		t.Error("root still tracked after ForgetRoot")
	}
}

// TestForgetRootAlsoCleansUsageAndRunningMaps is the regression test for
// a live review finding: ForgetRoot deleted only m.nodes[id], leaving a
// stale m.usageByRoot[id]/m.runningByRoot[id] entry behind — both keyed
// by root id and written to by every turn anywhere in the tree — for
// the rest of the process's life, one pair per forgotten root on a
// long-lived server that creates and deletes many.
func TestForgetRootAlsoCleansUsageAndRunningMaps(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))
	mgr.usageByRoot[root.ID] = provider.Usage{InputTokens: 5}
	mgr.runningByRoot[root.ID] = 0

	if err := mgr.ForgetRoot(root.ID); err != nil {
		t.Fatalf("ForgetRoot: %v", err)
	}
	if _, ok := mgr.usageByRoot[root.ID]; ok {
		t.Error("usageByRoot entry still present after ForgetRoot")
	}
	if _, ok := mgr.runningByRoot[root.ID]; ok {
		t.Error("runningByRoot entry still present after ForgetRoot")
	}
}

// TestForgetRootThenChildrenReapedEventuallyCollectsRoot is the
// regression test for a live review finding: a root DELETEd while it
// still had live children (cascade-canceled by endSubagentLineage, but
// not yet removed — Reap collects them bottom-up, one generation per
// call) was refused by ForgetRoot and then NEVER revisited — Reap
// unconditionally skips every root, so the now-childless root leaked
// for the rest of the process's life. ForgetRoot's pendingForget flag
// closes this: once the child is reaped away, a LATER Reap call also
// collects the root itself.
func TestForgetRootThenChildrenReapedEventuallyCollectsRoot(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	// rootStreamed reports the exact instant root's downstream resume turn
	// reaches the provider — see the wait below for why that instant is
	// what this test needs.
	rootStreamed := make(chan struct{}, 1)
	root := mgr.NewRoot(managedConfig("root",
		&streamSignalProvider{name: "root", streamed: rootStreamed},
		scriptedTurns("child", doneTurn("done")),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	// The child's own completion also delivers a notification to root
	// (idle at that point) and fires an active resume. The wait above only
	// confirms the CHILD's status, not that root's own resume turn has
	// settled back out of StatusRunning. ForgetRoot checks StatusRunning
	// BEFORE it checks for children, so racing that transient Running
	// window makes ForgetRoot take the "busy" refusal path instead of the
	// "has children" one — and the children path is the ONE path that arms
	// pendingForget, so the second Reap below would then wrongly find
	// nothing to collect.
	//
	// Waiting for root to read StatusIdle is not enough on its own: root
	// IS idle until the resume goroutine starts, so a lone status wait
	// returns on that pre-resume idle and proves nothing. Wait for the two
	// halves in order instead. First block until root's resume turn has
	// really reached the provider, which happens strictly after
	// ReportTurnStart set root StatusRunning; the status wait that follows
	// therefore cannot observe the pre-resume idle, only the settle after
	// the turn (root's provider has no scripted turn, so it fails at once).
	<-rootStreamed
	waitForStatus(t, mgr, root.ID, StatusIdle, time.Second)

	// Refused: root still has a (terminal, but not yet reaped) child.
	if err := mgr.ForgetRoot(root.ID); err == nil {
		t.Fatal("ForgetRoot on a root with a child: want error, got nil")
	}
	if _, ok := mgr.Info(root.ID); !ok {
		t.Fatal("root removed despite still having a child")
	}

	// First Reap collects the child (one generation).
	if n := mgr.Reap(); n != 1 {
		t.Fatalf("first Reap() = %d, want 1 (the child)", n)
	}
	// Second Reap: root is now childless AND pendingForget — must be
	// collected too, the one exception to "Reap never removes a root".
	if n := mgr.Reap(); n != 1 {
		t.Fatalf("second Reap() = %d, want 1 (the now-childless, pendingForget root)", n)
	}
	if _, ok := mgr.Info(root.ID); ok {
		t.Error("root still tracked after its pendingForget'd subtree was fully reaped")
	}
}

// TestReapNeverCollectsAnOrdinaryRootEvenWhenChildless proves the
// pendingForget exception is exactly that — an exception, not a
// loosening of "Reap never removes a root" for the ordinary case: a
// ROOT no caller ever asked ForgetRoot to remove stays tracked
// indefinitely even once childless, exactly like before this fix.
func TestReapNeverCollectsAnOrdinaryRootEvenWhenChildless(t *testing.T) {
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

	if n := mgr.Reap(); n != 1 {
		t.Fatalf("Reap() = %d, want 1 (the child)", n)
	}
	// root is now childless, but ForgetRoot was never called — must
	// survive indefinitely, unlike the pendingForget case above.
	if n := mgr.Reap(); n != 0 {
		t.Errorf("Reap() removed the root without ForgetRoot ever being called: %d nodes removed", n)
	}
	if _, ok := mgr.Info(root.ID); !ok {
		t.Fatal("ordinary root removed by Reap — must never happen")
	}
}

// TestForgetRootRejectsRootWithChildren proves ForgetRoot refuses to
// orphan a live subtree — matches Cancel's own cascade philosophy: tear
// the subtree down first (via Cancel) if that's really the intent.
func TestForgetRootRejectsRootWithChildren(t *testing.T) {
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

	if err := mgr.ForgetRoot(root.ID); err == nil {
		t.Error("ForgetRoot on a root with a live (even if terminal) child: want error, got nil")
	}
	if _, ok := mgr.Info(root.ID); !ok {
		t.Error("root removed despite still having a child")
	}
}

// TestForgetRootRejectsBusyRoot proves ForgetRoot refuses a currently
// running root — an in-flight turn still has a goroutine that will
// eventually call finalizeTurn expecting to find this node.
func TestForgetRootRejectsBusyRoot(t *testing.T) {
	blocker := &blockingProvider{name: "root", release: make(chan struct{})}
	t.Cleanup(func() { close(blocker.release) })
	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(managedConfig("root", blocker))

	mgr.ReportTurnStart(root)
	go root.Prompt(context.Background(), "go") //nolint:errcheck // released via t.Cleanup
	waitForStatus(t, mgr, root.ID, StatusRunning, time.Second)

	if err := mgr.ForgetRoot(root.ID); err == nil {
		t.Error("ForgetRoot on a running root: want error, got nil")
	}
	if _, ok := mgr.Info(root.ID); !ok {
		t.Error("root removed while still running")
	}
}

// TestForgetRootRejectsNonRoot proves ForgetRoot is never a substitute
// for Reap on a child — that is Reap's own job (a terminal, childless
// leaf) or, for a non-terminal child, nobody's job at all (its own
// in-flight turn must be protected).
func TestForgetRootRejectsNonRoot(t *testing.T) {
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

	if err := mgr.ForgetRoot(childID); err == nil {
		t.Error("ForgetRoot on a non-root child: want error, got nil")
	}
	if _, ok := mgr.Info(childID); !ok {
		t.Error("child removed by ForgetRoot — not its job")
	}
}

// TestForgetRootUnknownIDIsError proves ForgetRoot on an id this
// SessionManager never tracked returns ErrUnknownSession, mirroring
// Cancel/AbortTurn's own identical contract.
func TestForgetRootUnknownIDIsError(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	if err := mgr.ForgetRoot("ses_doesnotexist00000000"); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("ForgetRoot(unknown) = %v, want ErrUnknownSession", err)
	}
}

// TestSpawnPersistsTaskSpawnedRecord is the regression test for a
// follow-up finding: "child journal records." Before this fix, a task
// spawn had no durable, structured, independently-queryable trace of
// "child X spawned by Y at T" — only the rendered "[tasks: ...]"
// conversation text once the child eventually delivered. Proves Spawn
// writes a task.spawned record on the PARENT's own log, immediately, not
// waiting for delivery.
func TestSpawnPersistsTaskSpawnedRecord(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", nil)
	childProv := scriptedTurns("child", doneTurn("done"))
	reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}
	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(Config{Providers: reg, Model: modelFor("root"), SessionDir: dir})

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	data, err := os.ReadFile(filepath.Join(dir, root.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, `"type":"task.spawned"`) {
		t.Fatalf("parent log missing task.spawned record: %s", log)
	}
	if !strings.Contains(log, `"child_id":"`+childID+`"`) {
		t.Errorf("task.spawned record missing child_id %q: %s", childID, log)
	}
	if !strings.Contains(log, `"agent":"`+AgentExplore+`"`) {
		t.Errorf("task.spawned record missing agent %q: %s", AgentExplore, log)
	}
}

// TestSpawnBudgetExceeded is the regression test for a follow-up finding
// ("per-tree budgets"): Spawn now refuses once its tree's cumulative
// child token usage reaches the configured SetMaxTreeTokens budget,
// mirroring ErrDepthLimit/ErrConcurrencyLimit's identical shape.
func TestSpawnBudgetExceeded(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	mgr.SetMaxTreeTokens(100)
	child1Prov := scriptedTurns("child1", doneTurnWithUsage("done", provider.Usage{InputTokens: 60, OutputTokens: 50}))
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		child1Prov,
		scriptedTurns("child2", nil),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child1")})
	if err != nil {
		t.Fatalf("Spawn child1: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	// child1 alone spent 110 tokens (60+50), already over the 100-token
	// budget — a second spawn from the same root must be refused.
	if _, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child2")}); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("Spawn over budget = %v, want ErrBudgetExceeded", err)
	}
}

// TestSpawnBudgetCountsCacheTokensToo is the regression test for a live
// review finding: the ErrBudgetExceeded gate used to compare only
// InputTokens+OutputTokens against SetMaxTreeTokens, while usageByRoot
// itself already accumulated all four provider.Usage fields — a
// cache-heavy child (a large prompt resent every turn, reading mostly
// from cache, the shape AGENTS.md calls out for the openaicompat/
// Fireworks and anthropic routes) could spend well past the operator's
// real intended ceiling with the gate never noticing, because cache
// read/write tokens were silently exempt from the very check meant to
// bound them. Gives a child a small input+output total (20) but a large
// cache total (200) against a 100-token budget: input+output alone
// (20) would let a second Spawn sail through; the true four-field total
// (220) must refuse it.
func TestSpawnBudgetCountsCacheTokensToo(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	mgr.SetMaxTreeTokens(100)
	child1Prov := scriptedTurns("child1", doneTurnWithUsage("done", provider.Usage{
		InputTokens: 10, OutputTokens: 10, CacheReadTokens: 150, CacheWriteTokens: 50,
	})) // input+output = 20 (under budget alone); all four fields = 220 (well over)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		child1Prov,
		scriptedTurns("child2", nil),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child1")})
	if err != nil {
		t.Fatalf("Spawn child1: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	if _, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child2")}); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("Spawn over budget (cache-heavy) = %v, want ErrBudgetExceeded — the gate must count cache read/write tokens, not just input+output", err)
	}
}

// TestSpawnBudgetUnsetByDefault proves SetMaxTreeTokens is opt-in: a
// SessionManager that never calls it enforces no budget at all,
// regardless of how much usage accumulates.
func TestSpawnBudgetUnsetByDefault(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child1", doneTurnWithUsage("done", provider.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})),
		scriptedTurns("child2", nil),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child1")})
	if err != nil {
		t.Fatalf("Spawn child1: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	if _, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child2")}); err != nil {
		t.Errorf("Spawn with no budget configured: want nil error, got %v", err)
	}
}

// TestSpawnBudgetDeltaAccountingAcrossFollowupSend is the regression test
// for a real bug caught before it shipped: n.session.Usage() is
// CUMULATIVE across all of a session's turns, but finalizeTurn can run
// MULTIPLE times for the same node (session.send restarting an
// already-done child for a legitimate follow-up turn). Adding the full
// cumulative total on every finalizeTurn call, rather than just the
// NEW turn's delta, would double-count every prior turn on each
// follow-up. Proves the budget check sees exactly the true total spent
// (two 50-token turns = 100, not 150 or 200), by spawning a child, then
// sending it one follow-up, then asserting a THIRD child is refused only
// once the TRUE cumulative total (not an inflated one) crosses budget.
func TestSpawnBudgetDeltaAccountingAcrossFollowupSend(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	mgr.SetMaxTreeTokens(120)
	childProv := &scriptedProvider{name: "child1", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "first"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "second"}),
	}}
	childProv.turns[0][0].Usage = provider.Usage{InputTokens: 30, OutputTokens: 20} // 50
	childProv.turns[1][0].Usage = provider.Usage{InputTokens: 30, OutputTokens: 20} // 50 more; cumulative 100
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		childProv,
		scriptedTurns("child2", nil),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child1")})
	if err != nil {
		t.Fatalf("Spawn child1: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	if _, err := mgr.Send(context.Background(), childID, "again"); err != nil {
		t.Fatalf("Send follow-up: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	// True cumulative total is 100 (50+50) — still under the 120 budget.
	// If finalizeTurn had double-counted (e.g. 50 then 100, landing on
	// 150 total, or worse 50+150=200), this would already be refused.
	if _, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child2")}); err != nil {
		t.Errorf("Spawn at true-total 100/120 budget: want nil error, got %v (budget accounting likely double-counted the follow-up turn)", err)
	}
}

// TestSpawnBudgetDeltaAccountingSurvivesReapAndReadopt is the regression
// test for a live review finding distinct from the one above: THAT test
// covers two finalizeTurn calls against the SAME sessionNode (a plain
// Send follow-up, node never destroyed). This one covers a child that is
// REAPED between its two turns — Reap deletes its sessionNode entirely
// (usageByRoot survives; only a root-shaped node's usageByRoot entry is
// ever cleared, see Reap's own doc comment), so the follow-up turn goes
// through AdoptReloaded, which calls adoptLocked and builds a BRAND NEW
// sessionNode. Proves that new node's budgetedUsage is seeded from the
// warm session's own already-cumulative Usage(), not left at zero — the
// bug this closes would double the first turn's spend into usageByRoot a
// second time.
func TestSpawnBudgetDeltaAccountingSurvivesReapAndReadopt(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	mgr.SetMaxTreeTokens(120)
	childProv := &scriptedProvider{name: "child1", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "first"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "second"}),
	}}
	childProv.turns[0][0].Usage = provider.Usage{InputTokens: 40, OutputTokens: 30} // 70
	childProv.turns[1][0].Usage = provider.Usage{InputTokens: 10, OutputTokens: 20} // 30 more; cumulative 100
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		childProv,
		scriptedTurns("child2", nil),
	))
	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child1")})
	if err != nil {
		t.Fatalf("Spawn child1: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	childSess, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("child not tracked after its first turn")
	}
	if got := childSess.Usage(); got.InputTokens+got.OutputTokens != 70 {
		t.Fatalf("test setup: child usage after turn 1 = %+v, want 70 total", got)
	}

	// Reap the now-terminal, childless leaf — usageByRoot[root.ID] stays
	// at 70 (only a root-shaped node's entry is ever cleared by Reap).
	if n := mgr.Reap(); n != 1 {
		t.Fatalf("Reap() = %d, want 1", n)
	}
	if _, ok := mgr.Info(childID); ok {
		t.Fatal("child still tracked after Reap")
	}

	// Re-adopt the SAME warm *Session object (still carries its own
	// cumulative Usage()=70 in memory) — the exact shape a real
	// claimForPrompt cold-reload-then-run, or a direct AdoptReloaded
	// call, produces for a legitimate follow-up to a reaped child.
	// Config.TaskParentID survives on the object itself (set once at
	// Spawn), so no LoadSession/SessionDir round-trip is needed here to
	// exercise the same adoptLocked construction path.
	if err := mgr.AdoptReloaded(childSess); err != nil {
		t.Fatalf("AdoptReloaded: %v", err)
	}
	if _, err := mgr.Send(context.Background(), childID, "again"); err != nil {
		t.Fatalf("Send follow-up: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	// True cumulative total is 100 (70+30) — still under the 120 budget.
	// If the reaped-then-readopted node's budgetedUsage had started at
	// zero, this follow-up's finalizeTurn would have added the full 100
	// on top of the 70 usageByRoot already carried across the reap,
	// landing at 170 and refusing this Spawn.
	if _, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child2")}); err != nil {
		t.Errorf("Spawn at true-total 100/120 budget: want nil error, got %v (budget accounting likely double-counted the reaped child's prior turn)", err)
	}
}

// waitForStatus blocks until id reaches want, or fails the test once
// timeout elapses.
//
// It blocks on SessionManager.Changed — the manager's own "a node's state
// settled" signal — and never samples on an interval, so nothing here
// guesses how long a transition takes. Changed is armed BEFORE each Info
// read, so a transition that lands between the read and the wait is still
// delivered. timeout is a failure bound only: the happy path returns on
// the very transition that satisfies it.
func waitForStatus(t *testing.T, mgr *SessionManager, id string, want SessionStatus, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var last SessionStatus
	for {
		changed := mgr.Changed()
		info, ok := mgr.Info(id)
		if !ok {
			t.Fatalf("Info(%s): not found", id)
		}
		last = info.Status
		if last == want {
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("Info(%s).Status = %s after %s, want %s", id, last, timeout, want)
		}
	}
}

// waitForReap blocks until Reap calls have collected at least want nodes in
// total, or fails the test once timeout elapses. Same Changed-driven,
// sample-free shape as waitForStatus: a node becomes reapable only once
// finalizeTurn has marked it finalized, which is itself a state settle
// Changed reports.
//
// The count accumulates across calls, and the test is "at least want", not
// "exactly want on one call". Reap collects EVERY currently-reapable node
// in one call and removes it, so an interleave that makes two nodes
// reapable before the first Reap lands returns 2 where the caller asked
// for 1 — after which every later Reap returns 0. An equality test would
// then never match and would block to the timeout reporting "never became
// reapable", while nodes had in fact been reaped.
func waitForReap(t *testing.T, mgr *SessionManager, want int, timeout time.Duration, msg string) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	got := 0
	for {
		changed := mgr.Changed()
		got += mgr.Reap()
		if got >= want {
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("%s (Reap collected %d node(s), want at least %d, within %s)", msg, got, want, timeout)
		}
	}
}

// TestUnlockAndFlushPersistRunsThunksAfterReleasingLock is the regression
// test for a live review finding: session-log disk writes (task-
// notification queued/delivered records, the task-spawn audit record)
// used to run WHILE m.mu — the single lock guarding every session in the
// tree, taken by Info/Reap/Spawn/Send/finalize alike — was held, on
// finalizeTurn/Spawn/recoverInterruptedTurnLocked's own hot paths. A slow
// or contended disk on one session's notification could stall every
// OTHER session's own Info/Reap/Spawn/finalize call in the same process.
// deferPersist/unlockAndFlushPersist close this by queuing durable-write
// thunks while m.mu is held and running them only after it is released.
//
// Proves the ordering directly: a thunk that tries to re-acquire m.mu
// via TryLock (which would fail if m.mu were still held when the thunk
// runs) must succeed — i.e., unlockAndFlushPersist really does release
// the lock BEFORE running any queued thunk, not after.
func TestUnlockAndFlushPersistRunsThunksAfterReleasingLock(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	ran := false
	mgr.mu.Lock()
	mgr.deferPersist(func() {
		if !mgr.mu.TryLock() {
			t.Error("deferPersist thunk ran while m.mu was still held — disk I/O is still running inside the critical section")
			return
		}
		mgr.mu.Unlock()
		ran = true
	})
	mgr.unlockAndFlushPersist()
	if !ran {
		t.Error("deferPersist thunk never ran at all")
	}
}

// TestUnlockAndFlushPersistPreservesQueueOrder is the regression test for
// deferPersist/unlockAndFlushPersist's FIFO ordering guarantee — load-
// bearing for recoverInterruptedTurnLocked's own crash-window fix (see
// TestRecoverInterruptedTurnSurvivesACrashBetweenDeliveryAndHistoryClose),
// which relies on its notify/forwarded delivery thunks running BEFORE its
// closing-message persist thunk, in the exact order they were queued.
func TestUnlockAndFlushPersistPreservesQueueOrder(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	var order []int
	mgr.mu.Lock()
	for i := 0; i < 5; i++ {
		i := i
		mgr.deferPersist(func() { order = append(order, i) })
	}
	mgr.unlockAndFlushPersist()
	want := []int{0, 1, 2, 3, 4}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i, v := range want {
		if order[i] != v {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}
