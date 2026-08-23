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
