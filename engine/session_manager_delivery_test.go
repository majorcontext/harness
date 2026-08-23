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
	mgr.SetExternalRunner(func(id, text string) RunnerOutcome {
		mu.Lock()
		calls = append(calls, id)
		mu.Unlock()
		return RunnerUnknown // let the manager fall back to driving it directly
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

// TestTriggerResumeRevertsCentrallyOnRunnerRefused is the regression test
// for a follow-up finding ("ExternalRunner tri-state"): the revert
// RunnerRefused implies now happens inside triggerResumeLocked's own
// closure, centrally — NOT left to each ExternalRunner implementation to
// remember. The fake runner installed here returns RunnerRefused and
// does NOTHING else — no call to RevertResumeIfStillRunning of its own —
// proving the revert this test asserts on could only have come from
// SessionManager's own code, not this fake's. Under the OLD bool-typed
// contract this exact implementation shape (a runner that "handles" a
// refusal without separately reverting) would have left the root stuck
// StatusRunning forever.
func TestTriggerResumeRevertsCentrallyOnRunnerRefused(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	mgr.SetExternalRunner(func(id, text string) RunnerOutcome {
		return RunnerRefused // deliberately does nothing else
	})

	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", nil),
		scriptedTurns("child", doneTurn("child done")),
	))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("child")})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	// The child's completion notification triggers a resume attempt on
	// the idle root, which the fake ExternalRunner above refuses. If the
	// revert did not happen, root would be stuck StatusRunning forever
	// (queue-or-resume dead for it) — waitForStatus would time out.
	waitForStatus(t, mgr, root.ID, StatusIdle, time.Second)
}

// TestReportTurnStartAdoptsUnknownSession proves the fix for reloaded
// sessions after a restart or eviction: ReportTurnStart, given a session
// SessionManager has never seen before, adopts it as a fresh root rather
// than silently no-op'ing — the gap that left `task` permanently broken
// ("parent session no longer tracked") on a session revived by a cold
// disk reload in an earlier version of this package.
// TestReportTurnStartReattachesReloadedSession proves ReportTurnStart
// updates n.session on EVERY call, not only when first adopting an
// unknown id — the case a session evicted from residency and later
// reloaded into a NEW *engine.Session object (same id) hits. Without
// this, a background child completing in the gap enqueues its
// notification onto the OLD, now-orphaned object, which the live
// reloaded session's own checkout never reads: the result silently
// vanishes. A live review caught this exact failure mode.
func TestReportTurnStartReattachesReloadedSession(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	first := NewSession(managedConfig("root", scriptedTurns("root", nil)))
	mgr.ReportTurnStart(first)
	if got, _ := mgr.Session(first.ID); got != first {
		t.Fatal("did not attach to the first session on initial adopt")
	}

	// Simulate an eviction + cold reload: a SECOND, independent *Session
	// object built fresh (as engine.LoadSession would), forced to the
	// SAME id.
	second := NewSession(managedConfig("root", scriptedTurns("root", nil)))
	second.ID = first.ID

	mgr.ReportTurnStart(second)
	got, ok := mgr.Session(first.ID)
	if !ok {
		t.Fatal("session no longer tracked after re-sighting")
	}
	if got != second {
		t.Error("ReportTurnStart did not re-attach to the newly reloaded session object — a notification enqueued now would land on the orphaned first object")
	}

	// A notification enqueued after re-attachment must land on the LIVE
	// (second) object, not the orphaned first one.
	got.enqueueTaskNotification(taskNotification{ChildID: "ses_x", Status: StatusDone, Result: "hi"})
	if first.hasPendingTaskNotifications() {
		t.Error("notification landed on the orphaned first session object")
	}
	if !second.hasPendingTaskNotifications() {
		t.Error("notification did not land on the live second session object")
	}
}

// TestReportTurnStartMigratesNotificationEnqueuedBeforeReattach is the
// regression test for a review finding distinct from (and layered on
// top of) TestReportTurnStartReattachesReloadedSession above: that test
// only proves a notification enqueued AFTER re-attachment lands on the
// live object. This proves the harder, more important case — the
// notification that TRIGGERS the resume in the first place is enqueued
// BEFORE the cold-loaded object exists at all (an idle root can be
// evicted from server residency while a background child is still
// running — evictResidentLocked only protects a session the SERVER
// considers running, a different bit than SessionManager's own — so the
// child's completion enqueues onto the about-to-be-orphaned OLD object).
// An earlier revision of ReportTurnStart's re-attach silently dropped
// exactly that notification: the fresh object's own queue starts empty,
// so the resume turn it drives has no engine context to act on at all.
func TestReportTurnStartMigratesNotificationEnqueuedBeforeReattach(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 0, 0)
	first := NewSession(managedConfig("root", scriptedTurns("root", nil)))
	mgr.ReportTurnStart(first)

	// The notification a background child's completion delivers lands on
	// the CURRENTLY live object — first — before anything reloads.
	first.enqueueTaskNotification(taskNotification{ChildID: "ses_x", Status: StatusDone, Result: "hi"})
	if !first.hasPendingTaskNotifications() {
		t.Fatal("test setup: notification did not land on first")
	}

	// Simulate the eviction + cold reload THIS notification's own resume
	// triggers: a second, independent *Session object, same id.
	second := NewSession(managedConfig("root", scriptedTurns("root", nil)))
	second.ID = first.ID
	mgr.ReportTurnStart(second)

	if first.hasPendingTaskNotifications() {
		t.Error("notification still stranded on the orphaned first object after re-attach")
	}
	if !second.hasPendingTaskNotifications() {
		t.Error("notification was not migrated onto the live second object — the resume it triggered would have run with no engine context to act on")
	}
}

// TestReportTurnStartMigrationDoesNotDoubleDeliverAlongsideDurableFold is
// the regression test for a live review finding: the migration above
// (old.drainAllTaskNotifications() -> sess.enqueueTaskNotification) and
// LoadSession's own durable queued-minus-delivered fold (store.go) now
// overlap for the exact same notification. old durably enqueues (writes
// recTaskNotifyQueued); a resume cold-loads a fresh session via
// LoadSession, whose OWN fold already restores that just-written record
// as "copy 1"; this method's migration then drains the SAME notification
// off old (never touched since) and re-enqueues it as "copy 2" — the
// parent model would be told the same child completion twice.
func TestReportTurnStartMigrationDoesNotDoubleDeliverAlongsideDurableFold(t *testing.T) {
	dir := t.TempDir()
	reg := provider.Registry{"root": scriptedTurns("root", nil)}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr := NewSessionManager(context.Background(), 0, 0)
	old := mgr.NewRoot(rootCfg)

	// A background child finishes: enqueue durably onto old (the
	// currently tracked, soon-to-be-evicted object) — mirrors
	// finalizeTurn's own enqueueTaskNotification call exactly, including
	// its durable recTaskNotifyQueued write.
	old.enqueueTaskNotification(taskNotification{ChildID: "ses_x", Status: StatusDone, Result: "hi"})

	// Simulate the eviction + cold reload this notification's own resume
	// triggers: LoadSession reads the SAME durable log old just wrote to
	// — folding the notification in as "copy 1", exactly like a real
	// claimForPrompt cold-load would, strictly BEFORE ReportTurnStart
	// ever runs its migration below.
	reloaded, err := LoadSession(Config{Providers: reg, SessionDir: dir}, old.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if !reloaded.hasPendingTaskNotifications() {
		t.Fatal("test setup: LoadSession's own durable fold did not restore the notification")
	}

	// n.session == old still (unchanged since NewRoot); old != reloaded —
	// the migration path runs.
	mgr.ReportTurnStart(reloaded)

	reloaded.mu.Lock()
	if len(reloaded.taskNotifications) != 1 {
		reloaded.mu.Unlock()
		t.Fatalf("reloaded.taskNotifications = %+v, want exactly 1 — the durable fold and the migration must not both deliver the same notification", reloaded.taskNotifications)
	}
	reloaded.mu.Unlock()
}

// TestReportTurnStartMigrationDoesNotDurablyLoseAStillPendingNotification
// is the regression test for a live review finding on the FIX above: the
// migration's drain must NOT use the newly-persisting
// drainAllTaskNotifications — old and sess there are two in-memory
// objects for the SAME durable session id, sharing the SAME log (unlike
// finalizeTurn/recoverInterruptedTurnLocked's forward-to-a-different-
// ancestor callers). Persisting recTaskNotifyDelivered there durably
// cancels the notification's ORIGINAL recTaskNotifyQueued entry on that
// shared log — and since enqueueTaskNotificationMigrated's own dedup
// correctly declines to write a compensating fresh queued record for
// something LoadSession's fold already restored, the log would end up
// showing a balanced (queued+delivered) notification that is STILL, in
// fact, only in-memory pending and undelivered. A second eviction before
// it is ever checked out would then have the NEXT LoadSession fold it as
// genuinely delivered and silently drop it. Proves a second reload still
// restores the notification.
func TestReportTurnStartMigrationDoesNotDurablyLoseAStillPendingNotification(t *testing.T) {
	dir := t.TempDir()
	reg := provider.Registry{"root": scriptedTurns("root", nil)}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr := NewSessionManager(context.Background(), 0, 0)
	old := mgr.NewRoot(rootCfg)
	old.enqueueTaskNotification(taskNotification{ChildID: "ses_x", Status: StatusDone, Result: "hi"})

	// First eviction + cold reload + reattach — the exact sequence
	// TestReportTurnStartMigrationDoesNotDoubleDeliverAlongsideDurableFold
	// already covers for the double-delivery angle; this test cares about
	// what got left on DISK afterward.
	reloaded, err := LoadSession(Config{Providers: reg, SessionDir: dir}, old.ID)
	if err != nil {
		t.Fatalf("LoadSession (1st): %v", err)
	}
	mgr.ReportTurnStart(reloaded)

	// Simulate a SECOND eviction, before this notification was ever
	// checked out (checkoutTaskNotificationsSegment never ran — no turn
	// was driven against reloaded in between). A fresh LoadSession from
	// the same durable log must still restore it: if the migration's own
	// drain durably wrote a recTaskNotifyDelivered for it (the bug this
	// test guards against), the notification would now be permanently,
	// silently gone.
	reloadedAgain, err := LoadSession(Config{Providers: reg, SessionDir: dir}, old.ID)
	if err != nil {
		t.Fatalf("LoadSession (2nd): %v", err)
	}
	if !reloadedAgain.hasPendingTaskNotifications() {
		t.Error("notification silently lost across a second reload — the migration's drain durably (and wrongly) marked it delivered on the shared log")
	}
}

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

// TestFinalizeTurnGrandchildNotStrandedWhenParentSettlesRightAfterEnqueue
// proves finding #5 is fully closed: SessionManager's own mutex serializes
// a child's finalizeTurn call against its grandchild's, so there is no
// interleaving where a grandchild's notification can be enqueued onto a
// child that has ALREADY been marked done and forgotten. This reproduces
// the failure sequence the review described directly: the grandchild
// completes and enqueues onto the child WHILE the child's own turn is
// still in flight, so the child's own finalizeTurn call (which runs
// strictly afterward) is the one that must notice and forward it.
func TestFinalizeTurnGrandchildNotStrandedWhenParentSettlesRightAfterEnqueue(t *testing.T) {
	mgr := NewSessionManager(context.Background(), 3, 0)
	release := make(chan struct{})
	childBlocker := &blockingProvider{name: "childblock", release: release}
	root := mgr.NewRoot(managedConfig("root",
		scriptedTurns("root", doneTurn("root resumed")),
		childBlocker,
		scriptedTurns("grand", doneTurn("grandchild result")),
	))

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("childblock"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn child: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusRunning, time.Second)

	// The grandchild is spawned and runs to completion WHILE the child's
	// own turn is still blocked — its notification lands on the child
	// well before the child's own finalizeTurn runs.
	grandID, err := mgr.Spawn(SpawnOptions{ParentID: childID, Prompt: "go deeper", Model: modelFor("grand"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn grandchild: %v", err)
	}
	waitForStatus(t, mgr, grandID, StatusDone, time.Second)

	child, _ := mgr.Session(childID)
	if !child.hasPendingTaskNotifications() {
		t.Fatal("test setup: grandchild notification not yet enqueued on child")
	}

	// Now let the child's OWN turn finish — its finalizeTurn call must
	// forward the already-pending grandchild notification to the root
	// rather than dropping it once it settles done.
	close(release)
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range root.History() {
			if m.Role == message.RoleUser && m.Parts.Text() == taskResumeTriggerText {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("root never resumed with the grandchild's notification; history: %+v", root.History())
}

// TestReloadedChildWithDanglingTurnNotifiesParent is the regression test
// for a follow-up finding: "in-flight-children restart semantics." A
// child whose turn was genuinely IN FLIGHT when the process stopped has
// no live goroutine left, in a fresh process, to ever call finalizeTurn
// for it — before this fix it cold-reloaded as StatusIdle, indistinguishable
// from a child that never received a turn, and its parent waited forever
// for a notification that could never arrive.
//
// Simulates this for real, across a genuine process boundary, not the
// in-memory NewSession(cfg) shortcut most reload tests use: an actual
// child session is spawned and its turn allowed to durably append its
// user-role message (via signaledBlockingProvider's started signal —
// closed the instant streaming begins, which is strictly AFTER Prompt's
// own durable append point) before being abandoned mid-flight (the
// blocked goroutine is never released — matching a real crash, where
// nothing ever completes that turn). A brand-new SessionManager (mgr2,
// simulating a fresh process) then reloads the child from disk via
// LoadSession + AdoptReloaded, with its root independently re-adopted
// first (AdoptRoot, on a fresh *Session forced to the root's original
// id — the standard reload-simulation technique this file's other tests
// already use) so there is a live ancestor to actually deliver to.
func TestReloadedChildWithDanglingTurnNotifiesParent(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", doneTurn("resumed"))
	childProv := &signaledBlockingProvider{name: "childprov", started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(childProv.release) })
	reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 0, 0)
	root1 := mgr1.NewRoot(rootCfg)

	childID, err := mgr1.Spawn(SpawnOptions{
		ParentID: root1.ID, Prompt: "go", Model: modelFor("childprov"), AgentType: AgentGeneralPurpose,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-childProv.started // the child's turn has genuinely begun: its user message is now durably appended

	childSess, ok := mgr1.Session(childID)
	if !ok {
		t.Fatal("child not found before simulated crash")
	}
	if !childSess.hasUnansweredTurn() {
		t.Fatal("test setup: child's turn did not leave a dangling user message")
	}

	// Simulate a fresh process: a brand-new SessionManager, and a
	// brand-new root *Session object forced to root1's own id (mgr1's
	// blocked child goroutine is simply abandoned here — nothing in mgr2
	// or its own tree knows about mgr1 at all, exactly like a real crash
	// leaves no live goroutine behind in the NEW process).
	mgr2 := NewSessionManager(context.Background(), 0, 0)
	root2 := NewSession(rootCfg)
	root2.ID = root1.ID
	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}

	reloadedChild, err := LoadSession(Config{Providers: reg, SessionDir: dir}, childID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if err := mgr2.AdoptReloaded(reloadedChild); err != nil {
		t.Fatalf("AdoptReloaded: %v", err)
	}

	info, ok := mgr2.Info(childID)
	if !ok {
		t.Fatal("child not tracked after AdoptReloaded")
	}
	if info.Status != StatusFailed {
		t.Errorf("reloaded dangling child status = %q, want %q", info.Status, StatusFailed)
	}
	if !strings.Contains(info.FailReason, "restart") {
		t.Errorf("reloaded dangling child fail_reason = %q, want it to mention the restart", info.FailReason)
	}

	// The root (live, idle) must have been actively woken with the
	// synthetic notification — same delivery path, same observable
	// signature (the resume-trigger text in history) every other
	// terminal-outcome notification uses.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range root2.History() {
			if m.Role == message.RoleUser && m.Parts.Text() == taskResumeTriggerText {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("root never resumed with the dangling child's synthetic notification; history: %+v", root2.History())
}

// TestReloadedChildWithDanglingTurnFoldsUsageIntoTreeBudget is the
// regression test for a live review finding: recoverInterruptedTurnLocked
// built a "lost to restart" notification carrying Usage but never folded
// that usage into m.usageByRoot the way finalizeTurn's own three
// terminal-outcome branches always do — an interrupted child's real spend
// escaped SetMaxTreeTokens entirely, letting a later Spawn silently
// exceed the tree budget. Gives the child a REAL completed first turn
// (real usage), then interrupts a SECOND, follow-up turn (the actual
// "restart" case) — a single-turn dangling child never accumulates any
// usage at all in this test harness (the blocking provider stalls before
// ever returning a Usage-bearing event), so this is the only shape that
// can actually prove folding happened.
func TestReloadedChildWithDanglingTurnFoldsUsageIntoTreeBudget(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", nil)
	childProv1 := scriptedTurns("childprov1", doneTurnWithUsage("first turn done", provider.Usage{InputTokens: 40, OutputTokens: 30})) // 70
	childProv2 := &signaledBlockingProvider{name: "childprov2", started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(childProv2.release) })
	reg := provider.Registry{rootProv.Name(): rootProv, childProv1.Name(): childProv1, childProv2.Name(): childProv2}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 0, 0)
	root1 := mgr1.NewRoot(rootCfg)

	childID, err := mgr1.Spawn(SpawnOptions{
		ParentID: root1.ID, Prompt: "go", Model: modelFor("childprov1"), AgentType: AgentGeneralPurpose,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr1, childID, StatusDone, time.Second)
	childSess, _ := mgr1.Session(childID)
	if got := childSess.Usage(); got.InputTokens+got.OutputTokens == 0 {
		t.Fatal("test setup: child's first turn recorded no usage")
	}

	// Restart the child on a DIFFERENT (blocking) provider for a
	// follow-up turn, and let it dangle mid-flight — the actual
	// "interrupted by restart" shape.
	childSess.SetModel(modelFor("childprov2"))
	go mgr1.Send(context.Background(), childID, "follow up") //nolint:errcheck // deliberately abandoned mid-flight
	<-childProv2.started

	// Simulate a fresh process: a brand-new SessionManager, root
	// re-adopted first (same technique as TestReloadedChildWithDanglingTurnNotifiesParent).
	//
	// root2 gets its OWN "root"-named provider instance (rootProv2), not
	// rootProv again: childProv1's turn completing above already wakes
	// root1 for its own async resume on mgr1 (rootProv has zero
	// scripted turns, so that resume fails immediately) — that goroutine
	// can still be in flight here. Sharing the exact same
	// *scriptedProvider object between root1 and root2 would let those
	// two independent async resumes race on the fixture's own
	// unsynchronized internal bookkeeping (call count, requests slice);
	// that's a test-fixture race, not a product one, so the fix is
	// giving each root its own provider rather than serializing product
	// code that has no reason to serialize.
	rootProv2 := scriptedTurns("root", nil)
	reg2 := provider.Registry{rootProv2.Name(): rootProv2, childProv1.Name(): childProv1, childProv2.Name(): childProv2}
	rootCfg2 := Config{Providers: reg2, Model: modelFor("root"), SessionDir: dir}

	mgr2 := NewSessionManager(context.Background(), 0, 0)
	root2 := NewSession(rootCfg2)
	root2.ID = root1.ID
	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}
	reloadedChild, err := LoadSession(Config{Providers: reg2, SessionDir: dir}, childID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if err := mgr2.AdoptReloaded(reloadedChild); err != nil {
		t.Fatalf("AdoptReloaded: %v", err)
	}

	// recoverInterruptedTurnLocked folds usage synchronously before
	// AdoptReloaded returns, but delivering its notification can also
	// wake the root (here immediately failing, since rootProv is
	// scripted with zero turns) on its own async fireIdleResumeAsync
	// goroutine — which folds the root's OWN usage delta into the same
	// map on completion. Read usageByRoot under the manager's lock, like
	// every production call site does, rather than racing that
	// goroutine with a bare map index.
	readUsage := func() provider.Usage {
		mgr2.mu.Lock()
		defer mgr2.mu.Unlock()
		return mgr2.usageByRoot[root1.ID]
	}
	if got := readUsage(); got.InputTokens+got.OutputTokens == 0 {
		t.Error("usageByRoot has no usage folded in after recovering an interrupted child that had a real completed prior turn — the interrupted child's spend escaped the tree budget")
	}
}

// TestReloadedChildWithDanglingTurnIsIdempotentAcrossReapAndReload is the
// regression test for a live review finding: recoverInterruptedTurnLocked
// never mutated the child's history, so hasUnansweredTurn() stayed true
// FOREVER — every later re-adoption of the same id (a Reap, then a
// legitimate follow-up touching it again) re-ran the whole method and
// re-enqueued a SECOND, duplicate "lost to restart" notification for the
// SAME child. Proves recovery fires exactly once: reap the recovered
// child, re-adopt it a second time, and assert the ancestor's message
// history shows exactly ONE resume-trigger turn, not two.
func TestReloadedChildWithDanglingTurnIsIdempotentAcrossReapAndReload(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", doneTurn("resumed"))
	childProv := &signaledBlockingProvider{name: "childprov", started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(childProv.release) })
	reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 0, 0)
	root1 := mgr1.NewRoot(rootCfg)
	childID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("childprov"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-childProv.started

	mgr2 := NewSessionManager(context.Background(), 0, 0)
	root2 := NewSession(rootCfg)
	root2.ID = root1.ID
	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}

	// FIRST reload: recovery fires, delivers, and — critically — closes
	// the dangling turn in history (see recoverInterruptedTurnLocked's
	// own doc comment).
	load := func() *Session {
		s, err := LoadSession(Config{Providers: reg, SessionDir: dir}, childID)
		if err != nil {
			t.Fatalf("LoadSession: %v", err)
		}
		return s
	}
	if err := mgr2.AdoptReloaded(load()); err != nil {
		t.Fatalf("AdoptReloaded (1st): %v", err)
	}
	waitForStatus(t, mgr2, childID, StatusFailed, time.Second)

	// Reap the now-terminal, childless, finalized child, then reload it
	// a SECOND time — exactly the sequence a real Reap-ticker-plus-later-
	// follow-up would produce.
	if n := mgr2.Reap(); n != 1 {
		t.Fatalf("Reap() = %d, want 1", n)
	}
	if err := mgr2.AdoptReloaded(load()); err != nil {
		t.Fatalf("AdoptReloaded (2nd): %v", err)
	}

	// Count resume-trigger turns on the root: must be exactly one. Two
	// would mean the child's dangling turn was "recovered" twice.
	//
	// Both deliveries (the real one and, if the fix regresses, the
	// duplicate) go through the same async fireIdleResumeAsync path, so
	// a naive "return the instant count reaches 1" loop is a race: it
	// can win against the duplicate's append and report a false pass.
	// Wait for the first delivery, then wait out an additional settle
	// window and take one final count — that window must comfortably
	// exceed the scheduling delay between the two async deliveries.
	countTriggers := func() int {
		count := 0
		for _, m := range root2.History() {
			if m.Role == message.RoleUser && m.Parts.Text() == taskResumeTriggerText {
				count++
			}
		}
		return count
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && countTriggers() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if countTriggers() == 0 {
		t.Fatalf("root never resumed at all; history: %+v", root2.History())
	}
	time.Sleep(500 * time.Millisecond)
	if count := countTriggers(); count != 1 {
		t.Fatalf("root has %d resume-trigger turns, want exactly 1 (recovery ran more than once for the same child): history: %+v", count, root2.History())
	}
}

// TestReloadedChildWithUntrackedParentIsEventuallyReapable is the
// regression test for a live review finding: an interrupted child whose
// OWN parent could not be found tracked (adoptReloadedLocked's "true
// depth is unrecoverable" case) ends up with parentID == "" purely as a
// bookkeeping side effect — indistinguishable from a genuine root to
// Reap, which skips every parentID == "" node unconditionally. Before
// this fix such a node leaked as a StatusFailed pseudo-root forever.
// Proves it is instead collected by Reap, via the same pendingForget
// mechanism ForgetRoot uses.
func TestReloadedChildWithUntrackedParentIsEventuallyReapable(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", nil)
	childProv := &signaledBlockingProvider{name: "childprov", started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(childProv.release) })
	reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 3, 0)
	root1 := mgr1.NewRoot(rootCfg)
	childID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("childprov"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-childProv.started

	// Fresh process, but this time the ROOT is NEVER re-adopted — the
	// child's own TaskParentID names an id mgr2 has never seen, exactly
	// the "true depth is unrecoverable" case adoptReloadedLocked's own
	// doc comment describes.
	mgr2 := NewSessionManager(context.Background(), 3, 0)
	reloadedChild, err := LoadSession(Config{Providers: reg, SessionDir: dir}, childID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if err := mgr2.AdoptReloaded(reloadedChild); err != nil {
		t.Fatalf("AdoptReloaded: %v", err)
	}

	info, ok := mgr2.Info(childID)
	if !ok {
		t.Fatal("child not tracked after AdoptReloaded")
	}
	if info.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", info.Status, StatusFailed)
	}
	if info.ParentID != "" {
		t.Fatalf("test setup: expected parentID \"\" (untracked-parent case), got %q", info.ParentID)
	}

	if n := mgr2.Reap(); n != 1 {
		t.Errorf("Reap() = %d, want 1 (the untracked-parent interrupted child must be collected, not leak as a pseudo-root)", n)
	}
	if _, ok := mgr2.Info(childID); ok {
		t.Error("untracked-parent interrupted child still tracked after Reap")
	}
}

// TestReportTurnStartDoesNotFalselyReportChildDeadWhenContinuingIt is the
// regression test for a live review finding: ReportTurnStart's own
// adopt-on-first-sight branch used to call adoptReloadedLocked exactly
// the way AdoptReloaded does — including firing recoverInterruptedTurnLocked
// for a child with a dangling turn. But ReportTurnStart's very next lines
// unconditionally set n.status = StatusRunning and n.finalized = false to
// actually drive a fresh turn on that SAME node — so the old behavior was
// self-contradicting within one call: mark the child StatusFailed, append
// a synthetic "lost to restart" message to its own transcript, and
// durably notify a live ancestor it died, immediately before running it
// again. Proves ReportTurnStart's cold-reload path leaves no trace of
// recovery at all (no synthetic message, no ancestor notification) and
// puts the node straight into StatusRunning — exactly as if the node had
// already been tracked.
func TestReportTurnStartDoesNotFalselyReportChildDeadWhenContinuingIt(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", doneTurn("resumed"))
	childProv := &signaledBlockingProvider{name: "childprov", started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(childProv.release) })
	reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 0, 0)
	root1 := mgr1.NewRoot(rootCfg)
	childID, err := mgr1.Spawn(SpawnOptions{
		ParentID: root1.ID, Prompt: "go", Model: modelFor("childprov"), AgentType: AgentGeneralPurpose,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-childProv.started // the child's turn has genuinely begun: its user message is now durably appended

	childSess, ok := mgr1.Session(childID)
	if !ok {
		t.Fatal("child not found before simulated crash")
	}
	if !childSess.hasUnansweredTurn() {
		t.Fatal("test setup: child's turn did not leave a dangling user message")
	}

	// Simulate a fresh process, same technique as
	// TestReloadedChildWithDanglingTurnNotifiesParent.
	mgr2 := NewSessionManager(context.Background(), 0, 0)
	root2 := NewSession(rootCfg)
	root2.ID = root1.ID
	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}

	reloadedChild, err := LoadSession(Config{Providers: reg, SessionDir: dir}, childID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	// This is the exact shape claimForPrompt's cold-load-then-run path
	// produces: a session about to be driven by an external scheduler,
	// bracketed by ReportTurnStart/ReportTurnEnd — NOT a passive
	// AdoptReloaded call. The node is not yet tracked at all, so this
	// exercises adoptReloadedLocked's recover=false branch.
	mgr2.ReportTurnStart(reloadedChild)

	info, ok := mgr2.Info(childID)
	if !ok {
		t.Fatal("child not tracked after ReportTurnStart")
	}
	if info.Status != StatusRunning {
		t.Errorf("status after ReportTurnStart = %q, want %q (recovery must not have fired)", info.Status, StatusRunning)
	}
	if info.FailReason != "" {
		t.Errorf("fail_reason after ReportTurnStart = %q, want empty", info.FailReason)
	}
	for _, m := range reloadedChild.History() {
		if m.Role == message.RoleAssistant && m.Parts.Text() == lostToRestartText {
			t.Errorf("child history contains the synthetic lost-to-restart message even though ReportTurnStart is about to continue this exact turn: %+v", reloadedChild.History())
		}
	}

	// Give any wrongly-fired async resume delivery a real window to land,
	// then confirm the root never saw one — mirrors the settle-window
	// technique in TestReloadedChildWithDanglingTurnIsIdempotentAcrossReapAndReload,
	// for the same reason: a false negative here would be a race, not a
	// pass.
	time.Sleep(300 * time.Millisecond)
	for _, m := range root2.History() {
		if m.Role == message.RoleUser && m.Parts.Text() == taskResumeTriggerText {
			t.Fatalf("root was falsely notified the child died while ReportTurnStart was about to continue it; history: %+v", root2.History())
		}
	}

	// The bracket completes normally: this is a real, legitimate
	// continuation, not an abandoned turn.
	doneMsg := &message.Message{ID: "msg_done", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "done"}}}
	if resume := mgr2.ReportTurnEnd(childID, doneMsg, nil); resume != nil {
		resume()
	}
	waitForStatus(t, mgr2, childID, StatusDone, time.Second)
}

// TestDrainAllTaskNotificationsPersistsDeliverySoReloadDoesNotResurrectIt
// is the regression test for a live review finding: finalizeTurn forwards
// a terminal child's own pending (grandchild) notifications to the
// nearest live ancestor via drainAllTaskNotifications — a pre-existing
// mechanism this PR did not change — but drainAllTaskNotifications itself
// never wrote recTaskNotifyDelivered for what it drained, unlike its
// sibling commitTaskNotifications. The forwarded notification IS
// correctly re-enqueued (a fresh recTaskNotifyQueued) on the ancestor's
// own log, but the CHILD's own log kept an unmatched recTaskNotifyQueued
// record for it forever. A later LoadSession of that same child (e.g. a
// session.send re-run of a done/failed child) folded that unmatched
// record back in as a phantom pending notification — the child would
// have re-delivered to itself a result its ancestor already received,
// breaking the queued-minus-delivered durability invariant this whole
// mechanism depends on.
func TestDrainAllTaskNotificationsPersistsDeliverySoReloadDoesNotResurrectIt(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", doneTurn("resumed"))
	midProv := &signaledBlockingProvider{name: "mid", started: make(chan struct{}), release: make(chan struct{})}
	grandProv := scriptedTurns("grand", doneTurn("grandchild done"))
	reg := provider.Registry{rootProv.Name(): rootProv, midProv.Name(): midProv, grandProv.Name(): grandProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(rootCfg)

	midID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("mid"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn mid: %v", err)
	}
	<-midProv.started // mid is now StatusRunning, blocked mid-turn — its one and only checkoutTaskNotificationsSegment call already happened

	grandID, err := mgr.Spawn(SpawnOptions{ParentID: midID, Prompt: "go deeper", Model: modelFor("grand"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn grandchild: %v", err)
	}
	waitForStatus(t, mgr, grandID, StatusDone, time.Second)

	// The grandchild's notification lands on mid — the nearest LIVE
	// ancestor (mid.status == StatusRunning, not Done/Failed/Canceled) —
	// and sits pending: mid is busy with its own single blocked turn, so
	// it never checks the queue out again before this test releases it.
	midSess, ok := mgr.Session(midID)
	if !ok {
		t.Fatal("mid not tracked")
	}
	if !midSess.hasPendingTaskNotifications() {
		t.Fatal("test setup: mid does not have the grandchild's notification pending")
	}

	// Release mid — its own turn completes normally. finalizeTurn forwards
	// mid's still-pending queue (the grandchild's notification) to root via
	// drainAllTaskNotifications — the mechanism under test, not the
	// separate forwarding-destination logic TestGrandchildReparentsToNearestLiveAncestor
	// already covers.
	close(midProv.release)
	waitForStatus(t, mgr, midID, StatusDone, time.Second)

	// Reload mid fresh from disk — the exact shape a later session.send
	// re-run of this done child produces.
	reloadedMid, err := LoadSession(Config{Providers: reg, SessionDir: dir}, midID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if reloadedMid.hasPendingTaskNotifications() {
		t.Errorf("reloaded mid has a resurrected pending notification for the grandchild it already forwarded to root")
	}
}

// TestRecoverInterruptedTurnReportsTotalUsageNotDelta is the regression
// test for a live review finding: recoverInterruptedTurnLocked's notify
// carried Usage: delta (the not-yet-credited portion, correct for folding
// into usageByRoot) instead of Usage: total (n.session.Usage(), the full
// cumulative spend) — every one of finalizeTurn's own three notify-
// building branches uses the full total. A child recovered on a SECOND
// interrupted turn, after a FIRST turn's spend was already credited to
// THIS manager (surviving an intervening Reap via budgetedByChild), would
// under-report its total usage to the parent relative to an ordinarily
// failed child.
func TestRecoverInterruptedTurnReportsTotalUsageNotDelta(t *testing.T) {
	rootProv := scriptedTurns("root", doneTurn("resumed"))
	childProv1 := scriptedTurns("childprov1", doneTurnWithUsage("first turn done", provider.Usage{InputTokens: 40, OutputTokens: 30})) // 70
	childProv2 := &signaledBlockingProvider{name: "childprov2", started: make(chan struct{}), release: make(chan struct{})}
	reg := provider.Registry{rootProv.Name(): rootProv, childProv1.Name(): childProv1, childProv2.Name(): childProv2}
	rootCfg := Config{Providers: reg, Model: modelFor("root")}

	mgr := NewSessionManager(context.Background(), 3, 0)
	root := mgr.NewRoot(rootCfg)

	childID, err := mgr.Spawn(SpawnOptions{ParentID: root.ID, Prompt: "go", Model: modelFor("childprov1"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr, childID, StatusDone, time.Second)

	childSess, ok := mgr.Session(childID)
	if !ok {
		t.Fatal("child not tracked after turn 1")
	}
	// budgetedByChild[childID] == 70 now, credited by finalizeTurn — and,
	// critically, this survives even if the node itself is later removed
	// (see budgetedByChild's own doc comment).

	// Start a SECOND turn and let it dangle — the actual "interrupted by
	// restart" shape. Its blocking provider never returns a usage-bearing
	// event, so s.Usage() stays at exactly 70 throughout — the same value
	// budgetedByChild already credited. delta would therefore compute to
	// ZERO here; only total (70) is the value a parent should actually be
	// told.
	childSess.SetModel(modelFor("childprov2"))
	go mgr.Send(context.Background(), childID, "again") //nolint:errcheck // deliberately abandoned mid-flight
	<-childProv2.started

	// Simulate this same manager forgetting the node (Reap, or — as here,
	// directly — the same observable state) while budgetedByChild
	// survives regardless, exactly as it would across a real Reap.
	mgr.mu.Lock()
	delete(mgr.nodes, childID)
	mgr.mu.Unlock()

	if err := mgr.AdoptReloaded(childSess); err != nil {
		t.Fatalf("AdoptReloaded: %v", err)
	}

	root.mu.Lock()
	defer root.mu.Unlock()
	if len(root.taskNotifications) != 1 {
		t.Fatalf("root.taskNotifications = %+v, want exactly 1", root.taskNotifications)
	}
	got := root.taskNotifications[0].Usage
	if got.InputTokens+got.OutputTokens != 70 {
		t.Errorf("recovered notify Usage = %+v, want the full cumulative total (70), not the tree-budget delta (0) — a parent should be told this child's real total spend, matching every finalizeTurn branch", got)
	}
}

// TestRecoverInterruptedTurnForwardsGrandchildNotifications is the
// regression test for a live review finding: recoverInterruptedTurnLocked
// delivered only its own failure notify, never forwarding any of n's OWN
// pending notifications (a grandchild that completed and was queued on n
// while n's turn was still in flight, never checked out before the
// crash) — unlike finalizeTurn, which forwards a terminal child's pending
// queue via drainAllTaskNotifications alongside its own notify. Without
// forwarding, a grandchild's result is silently stranded on a node that
// will never read its queue again.
func TestRecoverInterruptedTurnForwardsGrandchildNotifications(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", doneTurn("resumed"))
	midProv := &signaledBlockingProvider{name: "mid", started: make(chan struct{}), release: make(chan struct{})}
	grandProv := scriptedTurns("grand", doneTurn("grandchild done"))
	reg := provider.Registry{rootProv.Name(): rootProv, midProv.Name(): midProv, grandProv.Name(): grandProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 3, 0)
	root1 := mgr1.NewRoot(rootCfg)

	midID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("mid"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn mid: %v", err)
	}
	<-midProv.started // mid is now StatusRunning, blocked mid-turn

	grandID, err := mgr1.Spawn(SpawnOptions{ParentID: midID, Prompt: "go deeper", Model: modelFor("grand"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn grandchild: %v", err)
	}
	waitForStatus(t, mgr1, grandID, StatusDone, time.Second)

	midSess, ok := mgr1.Session(midID)
	if !ok {
		t.Fatal("mid not tracked")
	}
	if !midSess.hasPendingTaskNotifications() {
		t.Fatal("test setup: mid does not have the grandchild's notification pending")
	}

	// Simulate a crash: mid's blocked goroutine is simply abandoned,
	// never released — nothing in a fresh process knows about it, same
	// technique as every other dangling-turn test in this file.

	// Fresh process: a brand-new SessionManager, root re-adopted at the
	// same id.
	mgr2 := NewSessionManager(context.Background(), 3, 0)
	root2 := NewSession(rootCfg)
	root2.ID = root1.ID
	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}

	reloadedMid, err := LoadSession(Config{Providers: reg, SessionDir: dir}, midID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if !reloadedMid.hasPendingTaskNotifications() {
		t.Fatal("test setup: reloaded mid did not restore the grandchild's pending notification from its own durable log")
	}

	if err := mgr2.AdoptReloaded(reloadedMid); err != nil {
		t.Fatalf("AdoptReloaded: %v", err)
	}

	root2.mu.Lock()
	defer root2.mu.Unlock()
	if len(root2.taskNotifications) != 2 {
		t.Fatalf("root2.taskNotifications = %+v, want 2 (mid's own failure notify + the forwarded grandchild notify)", root2.taskNotifications)
	}
	var sawMidFailure, sawGrandchildDone bool
	for _, n := range root2.taskNotifications {
		switch {
		case n.ChildID == midID && n.Status == StatusFailed:
			sawMidFailure = true
		case n.ChildID == grandID && n.Status == StatusDone:
			sawGrandchildDone = true
		}
	}
	if !sawMidFailure {
		t.Errorf("root2.taskNotifications missing mid's own failure notify: %+v", root2.taskNotifications)
	}
	if !sawGrandchildDone {
		t.Errorf("root2.taskNotifications missing the forwarded grandchild notification — stranded on the recovered, never-to-run-again mid node: %+v", root2.taskNotifications)
	}
}
