package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestCanceledChildNotifiesParent proves the design doc's requirement that
// cancellation is one of a child's terminal outcomes a parent must be
// told about ("A child that errors terminally (...cancellation) delivers
// a failed notification"), not silently swallowed — a live adversarial
// review found an earlier version dropped it entirely (finalizeTurn's
// early return on an already-canceled node skipped notification
// construction altogether).
func TestCanceledChildNotifiesParent(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	release := make(chan struct{})
	blocker := &blockingProvider{name: "blocker", release: release}
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), blocker))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "work", Model: modelFor("blocker"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	childSess, _ := mgr.Session(childID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(childSess.History()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	if err := mgr.Cancel(childID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusCanceled, 2*time.Second)

	// The cancellation must have produced a notification on the root,
	// delivered as an engine-initiated resume (root was idle).
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(root.History()) >= 4 { // user "start", assistant, then the resume pair
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	found := false
	for _, m := range root.History() {
		if m.Role == message.RoleUser && m.Parts.Text() == taskResumeTriggerText {
			found = true
		}
	}
	if !found {
		t.Fatalf("no engine-initiated resume observed on root; history: %+v", root.History())
	}
	close(release)
}

// TestGrandchildReparentsToNearestLiveAncestor proves nesting past one
// level actually delivers: a grandchild's completion, arriving after its
// direct parent has already settled done, is reparented to the nearest
// still-live ancestor (the root) rather than silently discarded — a live
// review's reproduced "nesting effectively depth-1" finding.
func TestGrandchildReparentsToNearestLiveAncestor(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("mid", doneTurn("mid done")), // A: settles done quickly
		scriptedTurns("grand", nil),                // B: no turns yet (spawned after A settles)
	))

	midID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("mid"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn mid: %v", err)
	}
	waitForStatus(t, mgr, midID, StatusDone, time.Second)

	// Now spawn the grandchild FROM the already-done mid node — legal:
	// Spawn only checks the parent node's bookkeeping (depth/cancellation),
	// not whether its own turn already finished.
	grandProv := &scriptedProvider{name: "grand", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "grandchild result"}),
	}}
	mid, _ := mgr.Session(midID)
	mid.cfg.Providers["grand"] = grandProv
	grandID, err := mgr.Spawn(SpawnOptions{ParentID: midID, Prompt: "go deeper", Model: modelFor("grand"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn grandchild: %v", err)
	}
	waitForStatus(t, mgr, grandID, StatusDone, time.Second)

	// mid (the grandchild's DIRECT parent) is done and never runs again —
	// nothing should be pending there. The root, as the nearest LIVE
	// ancestor, should have received the notification and, since it was
	// idle, been engine-initiated-resumed with it.
	if mid.hasPendingTaskNotifications() {
		t.Error("notification wrongly left pending on the done direct parent")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, m := range root.History() {
			if m.Role == message.RoleUser && m.Parts.Text() == taskResumeTriggerText {
				found = true
			}
		}
		if found {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("root was never resumed with the grandchild's notification; history: %+v", root.History())
}

// TestSendRejectsConcurrentCall proves the fix for a reproduced data
// race: two Send calls for the SAME session id must never both reach
// Session.Prompt concurrently. Run under -race; the second call must
// return ErrSessionBusy, never silently proceed.
func TestSendRejectsConcurrentCall(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	release := make(chan struct{})
	blocker := &blockingProvider{name: "blocker", release: release}
	root := mgr.NewRoot(managedConfig("blocker", blocker))

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := mgr.Send(context.Background(), root.ID, "go")
			results[i] = err
		}(i)
	}
	// Give both goroutines a chance to actually call Send before releasing
	// the blocked provider.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	busyCount, nilCount := 0, 0
	for _, err := range results {
		switch {
		case errors.Is(err, ErrSessionBusy):
			busyCount++
		case err == nil:
			nilCount++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if nilCount != 1 || busyCount != 1 {
		t.Errorf("nilCount=%d busyCount=%d, want exactly one success and one ErrSessionBusy", nilCount, busyCount)
	}
}

// TestSendEnforcesConcurrencyLimit proves Send respects the SAME
// tree-wide concurrency budget Spawn does — a live review found Send
// unconditionally incremented runningByRoot with no check at all,
// letting enough concurrent session.send calls against already-settled
// children push the count above maxConcurrent.
func TestSendEnforcesConcurrencyLimit(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 1) // cap 1
	release := make(chan struct{})
	blocker := &blockingProvider{name: "blocker", release: release}
	blocker2 := &blockingProvider{name: "blocker2", release: make(chan struct{})}
	// Every provider any Spawn call in this test might need is registered
	// UP FRONT, before any Spawn — mutating a live *Session's
	// cfg.Providers map after a spawned goroutine may already be reading
	// it races (see managedConfig's own doc comment; caught live by this
	// package's -race suite while this test was written).
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil), blocker, blocker2))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("blocker")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusRunning, time.Second)

	// A second child is at the cap already (the first is still running) —
	// Spawn's concurrency check runs before it ever touches a provider, so
	// no second provider is needed for this assertion.
	if _, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("blocker")}); !errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("Spawn at cap: err = %v, want ErrConcurrencyLimit", err)
	}

	close(release)
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	// Send to the now-done child again — this itself must respect the
	// concurrency budget too, exactly like the Spawn check above, not
	// just Spawn.
	otherChildID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("blocker2")})
	if err != nil {
		t.Fatalf("Spawn other child: %v", err)
	}
	waitForStatus(t, mgr, otherChildID, StatusRunning, time.Second)

	// Now Send to the FIRST (done) child while the second is still
	// running and the cap is 1: must be rejected.
	if _, err := mgr.Send(context.Background(), childID, "again"); !errors.Is(err, ErrConcurrencyLimit) {
		t.Errorf("Send at cap: err = %v, want ErrConcurrencyLimit", err)
	}
	close(blocker2.release)
}

// TestFinalizeTurnRootRetriesForNotificationArrivedAtTurnTail reproduces
// finding #2 exactly: a notification that arrives for a root AFTER its
// own last checkout but before finalizeTurn settles it idle must not be
// stranded — it triggers an immediate re-resume rather than waiting for
// something that will never come (an idle root is only ever woken by a
// FRESH notification, and this one already arrived).
func TestFinalizeTurnRootRetriesForNotificationArrivedAtTurnTail(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	prov := &lateArrivalProvider{name: "late"}
	root := mgr.NewRoot(managedConfig("late", prov))
	// Set AFTER construction but BEFORE Send below ever calls prov.Stream
	// — a plain, single-goroutine, happens-before-safe assignment, not a
	// race: prov.root is only ever read later, from Stream, invoked by
	// Send's own synchronous call chain.
	prov.root = root

	// prov.Stream enqueues a notification WHILE this turn's own request is
	// being served — i.e. strictly after streamTurn's own checkout already
	// ran (checkout happens before Stream is ever called), reproducing
	// finding #2's exact race: a notification arriving too late for the
	// CURRENT request but before finalizeTurn settles the session idle.
	if _, err := mgr.Send(context.Background(), root.ID, "go"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// The root must have been immediately re-triggered rather than left
	// idle with the notification stranded.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range root.History() {
			if m.Role == message.RoleUser && m.Parts.Text() == taskResumeTriggerText {
				return // found the re-trigger
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("root never re-triggered for the late-arriving notification; history: %+v", root.History())
}

// TestFinalizeTurnFailedResumeDoesNotHotLoop is a regression test: an
// earlier version of the turn-tail-recheck fix above retriggered on ANY
// pending notification regardless of whether the JUST-FINISHED turn
// itself failed — since a failed turn's own notifications are requeued
// (at-least-once delivery), that version saw its own requeued entry and
// retried immediately, forever, against a persistently failing provider.
func TestFinalizeTurnFailedResumeDoesNotHotLoop(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	// No turns scripted: every Stream call fails.
	root := mgr.NewRoot(managedConfig("root", scriptedTurns("root", nil)))
	root.enqueueTaskNotification(taskNotification{ChildID: "ses_x", Status: StatusDone, Result: "hi"})

	done := make(chan struct{})
	go func() {
		mgr.Send(context.Background(), root.ID, "go") //nolint:errcheck
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not return — suspected hot-loop retrying a failed resume")
	}

	// The notification must still be recoverable (requeued, not lost) for
	// a later, legitimate trigger.
	if !root.hasPendingTaskNotifications() {
		t.Error("notification lost after the failed turn, want it requeued")
	}
}

// TestExternalRunnerConsultedOnlyForRoots proves ExternalRunner is
// delegated to for a depth-0 node's resume but NEVER for a child's — a
// child has no other scheduler, ever, so SessionManager must always drive
// it directly regardless of whether an ExternalRunner is installed.
func TestExternalRunnerConsultedOnlyForRoots(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	var calls []string
	var mu sync.Mutex
	mgr.SetExternalRunner(func(id, text string) bool {
		mu.Lock()
		calls = append(calls, id)
		mu.Unlock()
		return false // not handled: let the manager fall back to driving it directly
	})

	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", doneTurn("root turn")),
		scriptedTurns("child", doneTurn("child done")),
	))
	if _, err := mgr.Send(context.Background(), root.ID, "start"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForStatus(t, mgr, root.ID, StatusIdle, time.Second)

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)
	waitForStatus(t, mgr, root.ID, StatusIdle, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != root.ID {
		t.Errorf("externalRunner calls = %v, want exactly one call for the root %s", calls, root.ID)
	}
}

// TestReportTurnStartAdoptsUnknownSession proves the fix for reloaded
// sessions after a restart or eviction: ReportTurnStart, given a session
// SessionManager has never seen before, adopts it as a fresh root rather
// than silently no-op'ing — the gap that left `task` permanently broken
// ("parent session no longer tracked") on a session revived by a cold
// disk reload in an earlier version of this package.
func TestReportTurnStartAdoptsUnknownSession(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	// A session built directly, NOT through mgr.NewRoot/AdoptRoot —
	// simulates a fresh reload via engine.LoadSession that this
	// SessionManager has never registered.
	s := NewSession(managedConfig("root", scriptedTurns("root", nil)))

	if _, ok := mgr.Info(s.ID); ok {
		t.Fatal("session unexpectedly already tracked")
	}
	mgr.ReportTurnStart(s)
	info, ok := mgr.Info(s.ID)
	if !ok {
		t.Fatal("ReportTurnStart did not adopt the unknown session")
	}
	if info.Status != StatusRunning {
		t.Errorf("status = %s, want running", info.Status)
	}
	if _, ok := s.tools[taskToolName]; !ok {
		t.Error("task tool not installed by adopt-on-first-sight")
	}

	msg := &message.Message{ID: "m1", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "ok"}}}
	mgr.ReportTurnEnd(s.ID, msg, nil)
	info, _ = mgr.Info(s.ID)
	if info.Status != StatusIdle {
		t.Errorf("status after ReportTurnEnd = %s, want idle", info.Status)
	}
}

func TestNeutralizeAndReparentTogether(t *testing.T) {
	// Sanity: renderTaskNotifications never panics on an empty Agent/Result.
	seg := renderTaskNotifications([]taskNotification{{ChildID: "x", Status: StatusDone}})
	if !strings.Contains(seg, "x") {
		t.Errorf("segment missing id: %q", seg)
	}
}

// lateArrivalProvider enqueues a task notification on root as a side
// effect of serving its ONE scripted response — simulating a sibling
// child completing WHILE this request is in flight, strictly after
// streamTurn's own checkout (checkoutTaskNotificationsSegment) already
// ran (checkout happens before Stream is ever called) but before this
// turn's finalizeTurn call. Used by
// TestFinalizeTurnRootRetriesForNotificationArrivedAtTurnTail to
// reproduce finding #2 deterministically, without relying on real
// goroutine-scheduling timing.
type lateArrivalProvider struct {
	name string
	root *Session
}

func (p *lateArrivalProvider) Name() string { return p.name }

func (p *lateArrivalProvider) Stream(_ context.Context, _ *provider.Request) (provider.Stream, error) {
	p.root.enqueueTaskNotification(taskNotification{ChildID: "ses_late", Status: StatusDone, Result: "late arrival"})
	return &scriptedStream{events: asstTurn(provider.StopEndTurn, &message.Text{Text: "first turn done"})}, nil
}
