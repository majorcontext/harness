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
	// The canceled child's own goroutine has been blocked on <-release
	// (blockingProvider's Stream call) since Spawn — releasing it here
	// unblocks child.Prompt, which finishes and calls finalizeTurn in a
	// goroutine this test never otherwise waits for. Without settling
	// before returning, that goroutine can outlive this test and race a
	// LATER test's freshly allocated provider.Registry map landing at
	// the same reused memory address — a live -race flake caught under
	// repeated runs (pre-existing, unrelated to this session's own
	// changes; surfaced by stress-testing while verifying them).
	time.Sleep(100 * time.Millisecond)
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
	// mid's own completion also delivers a notification to root (idle at
	// that point) and fires an active resume — Config.Providers is
	// inherited BY REFERENCE across this whole tree (root/mid/grand all
	// share ONE map), so that resume's own streamTurn call can still be
	// reading it. waitForStatus above only confirms mid's own status,
	// not that root's downstream resume has settled — give it a moment
	// before mutating the shared map below, or root's own concurrent
	// Registry.For() read can race this test's own write — a live -race
	// flake caught under repeated stress runs (pre-existing, unrelated
	// to this session's own changes).
	time.Sleep(100 * time.Millisecond)

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

// TestReportTurnStartMigrationDoesNotDoublePersistANewlyRaceEnqueuedNotification
// is the regression test for a live review finding on
// enqueueTaskNotificationMigrated's OTHER branch — the append (genuinely
// new, not a dedup match) case, exercising the "narrower race" the
// method's own doc comment describes: LoadSession runs OUTSIDE m.mu, so a
// notification can be durably enqueued onto the evicted OLD object in the
// gap between that load completing and ReportTurnStart reacquiring the
// lock — meaning the fold never saw it, so the dedup check does not skip
// it. But that notification's recTaskNotifyQueued record is ALREADY on
// the shared log (old.enqueueTaskNotification wrote it durably at the
// moment it arrived, to the SAME log sess shares) — enqueueTaskNotificationMigrated's
// append branch used to ALSO persist a fresh recTaskNotifyQueued for it,
// producing two queued records with no matching delivered one. A later
// reload's queued-minus-delivered fold nets ONE PHANTOM PENDING COPY,
// double-delivering the same child completion after a restart. Proves a
// fresh reload after the migration restores exactly one copy, not two.
func TestReportTurnStartMigrationDoesNotDoublePersistANewlyRaceEnqueuedNotification(t *testing.T) {
	dir := t.TempDir()
	reg := provider.Registry{"root": scriptedTurns("root", nil)}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr := NewSessionManager(context.Background(), 0, 0)
	old := mgr.NewRoot(rootCfg)
	// Force the log file to exist before anything is durably written to
	// it — NewRoot/NewSession never write anything at construction, and
	// LoadSession requires the file to already exist.
	if err := old.ensureLog(); err != nil {
		t.Fatalf("ensureLog: %v", err)
	}

	// The resume's own LoadSession call happens FIRST here, deliberately
	// BEFORE anything is enqueued — reproducing the race window: sess's
	// fold cannot possibly see a notification that does not exist yet.
	sess, err := LoadSession(Config{Providers: reg, SessionDir: dir}, old.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if sess.hasPendingTaskNotifications() {
		t.Fatal("test setup: sess should not have restored anything yet")
	}

	// NOW a background child finishes and enqueues durably onto old —
	// strictly after sess's own load, exactly the race window
	// enqueueTaskNotificationMigrated's own doc comment describes.
	old.enqueueTaskNotification(taskNotification{ChildID: "ses_y", Status: StatusDone, Result: "race"})

	// ReportTurnStart's migration runs: old != sess, so it drains old
	// (same-log, no persist) and migrates onto sess — the append branch,
	// since sess's own queue is still empty.
	mgr.ReportTurnStart(sess)
	if !sess.hasPendingTaskNotifications() {
		t.Fatal("test setup: the race-enqueued notification was not migrated onto sess at all")
	}

	// A fresh reload from the same durable log must restore EXACTLY one
	// copy — two would mean the append branch durably double-wrote a
	// record that was already backed on disk by old's own original
	// enqueue.
	reloadedAgain, err := LoadSession(Config{Providers: reg, SessionDir: dir}, old.ID)
	if err != nil {
		t.Fatalf("LoadSession (after migration): %v", err)
	}
	reloadedAgain.mu.Lock()
	defer reloadedAgain.mu.Unlock()
	if len(reloadedAgain.taskNotifications) != 1 {
		t.Errorf("reloadedAgain.taskNotifications = %+v, want exactly 1 — the migration's append branch must not durably double-write a record old's own enqueue already backed on the shared log", reloadedAgain.taskNotifications)
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
	if !childSess.hasUnfinalizedTurn() {
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
	found := false
	for time.Now().Before(deadline) {
		for _, m := range root2.History() {
			if m.Role == message.RoleUser && m.Parts.Text() == taskResumeTriggerText {
				found = true
			}
		}
		if found {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !found {
		t.Fatalf("root never resumed with the dangling child's synthetic notification; history: %+v", root2.History())
	}
	// Wait for the resume TURN itself to fully settle (not just its
	// trigger message, appended synchronously before the turn even
	// starts) before this test returns — fireIdleResumeAsync's own
	// goroutine is otherwise still running (mid Stream() call on
	// rootProv) when the test function returns, unsynchronized with
	// whatever the NEXT test does with a freshly allocated object that
	// can land at the same address — a live-caught -race flake under
	// repeated runs.
	waitForStatus(t, mgr2, root1.ID, StatusIdle, 2*time.Second)
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
// never mutated the child's history, so the dangling-turn signal (now
// hasUnfinalizedTurn()/turnUnsettled — see their own doc comments;
// originally a trailing-message-role heuristic named hasUnansweredTurn,
// since replaced) stayed true FOREVER — every later re-adoption of the
// same id (a Reap, then a
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
	if !childSess.hasUnfinalizedTurn() {
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

	// finalizeTurn's own durable writes (the forwarded notification's
	// recTaskNotifyQueued on root's log, and — the mechanism under test —
	// the recTaskNotifyDelivered on mid's own log) are now queued via
	// deferPersist and run AFTER m.mu releases (see
	// SessionManager.deferPersist/unlockAndFlushPersist's own doc
	// comment) — decoupled from n.status becoming visible to another
	// goroutine's Info() call, which happens earlier, while m.mu is
	// still held. waitForStatus seeing StatusDone therefore does NOT
	// guarantee the durable write has landed yet; poll the reload itself
	// rather than reading it exactly once.
	deadline := time.Now().Add(2 * time.Second)
	var reloadedMid *Session
	for {
		var err error
		reloadedMid, err = LoadSession(Config{Providers: reg, SessionDir: dir}, midID)
		if err != nil {
			t.Fatalf("LoadSession: %v", err)
		}
		if !reloadedMid.hasPendingTaskNotifications() || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
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

	// root's own turn-1 completion ALSO legitimately delivered an
	// ordinary "done" notification here (root was idle when childProv1
	// finished, so finalizeTurn both enqueued it and fired an active
	// resume) — unrelated to what this test checks, but not guaranteed
	// to have already been checked out and committed off
	// root.taskNotifications by the time this assertion runs (that
	// resume is itself async, racing this goroutine under load). Find
	// the recovery notification specifically by ChildID+StatusFailed,
	// rather than asserting the queue's total length — a live -race/
	// timing flake caught under repeated stress runs.
	root.mu.Lock()
	defer root.mu.Unlock()
	var found *taskNotification
	for i := range root.taskNotifications {
		if n := &root.taskNotifications[i]; n.ChildID == childID && n.Status == StatusFailed {
			found = n
			break
		}
	}
	if found == nil {
		t.Fatalf("root.taskNotifications = %+v, want a StatusFailed recovery notification for %s", root.taskNotifications, childID)
	}
	got := found.Usage
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
	// rootProv has ZERO scripted turns, deliberately: root2 is genuinely
	// idle when recovery delivers to it, so recoverInterruptedTurnLocked
	// fires a REAL active resume (go m.fireIdleResumeAsync) — a scripted
	// turn that could SUCCEED would let that resume checkout-and-commit
	// the very notifications this test wants to inspect, racing the
	// test's own read of root2.taskNotifications against that background
	// goroutine. Zero turns means the resume's own Stream() call fails
	// immediately, so streamTurn's failure path requeues instead of
	// committing — the pending notifications this test asserts on
	// survive regardless of how that race resolves.
	rootProv := scriptedTurns("root", nil)
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

	// root2 is genuinely idle when recovery delivers to it, so this
	// triggers a REAL active resume (go m.fireIdleResumeAsync) racing
	// this test's own read — rootProv's zero scripted turns make that
	// resume's own Stream() call fail immediately, requeuing the
	// notifications back onto root2.taskNotifications, but the checkout
	// (into taskNotificationsInFlight) and requeue both happen on that
	// OTHER goroutine, asynchronously. Poll rather than read once, and
	// count BOTH sets — pending plus in-flight is the true "delivered to
	// root2" total regardless of which side of that race is caught mid-
	// flight.
	countAll := func() int {
		root2.mu.Lock()
		defer root2.mu.Unlock()
		return len(root2.taskNotifications) + len(root2.taskNotificationsInFlight)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && countAll() != 2 {
		time.Sleep(5 * time.Millisecond)
	}
	// Also give the requeue itself a moment to settle back into
	// taskNotifications specifically, so the content check below reads a
	// stable, non-in-flight set.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		root2.mu.Lock()
		n := len(root2.taskNotifications)
		root2.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	root2.mu.Lock()
	defer root2.mu.Unlock()
	if len(root2.taskNotifications) != 2 {
		t.Fatalf("root2.taskNotifications = %+v (in-flight: %+v), want 2 settled entries (mid's own failure notify + the forwarded grandchild notify)", root2.taskNotifications, root2.taskNotificationsInFlight)
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

// TestRecoverInterruptedTurnSurvivesACrashBetweenDeliveryAndHistoryClose is
// the regression test for a live review finding: recoverInterruptedTurnLocked
// used to durably close the interrupted child's history and mark it
// settled BEFORE delivering its failure notification to the ancestor. A
// crash landing between those two durable writes permanently lost the
// notification — the child's own log already looked "recovered," so no
// later re-adoption would ever retry, but the ancestor's log never got
// the recTaskNotifyQueued record. The parent would wait forever for a
// notification a crash ate in transit.
//
// Simulates that exact crash: drives recovery manually, then executes
// ONLY the first queued deferred-persist thunk (the notification
// delivery) — never any later ones, including the closing-message
// persist and the recChildTurnSettled marker write — mimicking a process
// death right after that first durable write lands. Proves: (1) the
// ancestor's log durably has the notification despite the "crash," and
// (2) the child's own log still shows the turn unfinalized
// (hasUnfinalizedTurn()/turnUnsettled — see their own doc comments), so
// a later restart can genuinely retry — the fix this test exists to
// prove, replacing what used to be silent, permanent loss.
func TestRecoverInterruptedTurnSurvivesACrashBetweenDeliveryAndHistoryClose(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", nil)
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
	<-childProv.started

	// root2 gets its OWN "root"-named provider instance, not rootProv
	// again: t.Cleanup releases childProv at the end of this test, which
	// unblocks mgr1's own still-in-flight child goroutine and lets ITS
	// finalizeTurn wake root1 (mgr1) too — a SEPARATE, independent async
	// resume from the one this test triggers on root2 (mgr2) below.
	// Sharing the exact same *scriptedProvider object between root1 and
	// root2 would let those two independent async resumes race on the
	// fixture's own unsynchronized internal state — a live -race flake
	// caught under repeated runs, same root cause as (and same fix as)
	// other tests in this file that adopt a root into a second manager.
	rootProv2 := scriptedTurns("root", nil)
	reg2 := provider.Registry{rootProv2.Name(): rootProv2, childProv.Name(): childProv}
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

	// Drive adoption manually (same shape as AdoptReloaded, minus its own
	// unlockAndFlushPersist) so the queued thunks can be inspected and
	// only PARTIALLY run below, simulating the crash.
	mgr2.mu.Lock()
	mgr2.adoptReloadedLocked(reloadedChild, true)
	pending := mgr2.pendingPersist
	mgr2.pendingPersist = nil
	mgr2.mu.Unlock()

	if len(pending) < 2 {
		t.Fatalf("recovery queued %d deferred persists, want at least 2 (notification delivery, then the closing-message append)", len(pending))
	}
	// Run ONLY the first thunk — the notification delivery — never any
	// later one. This is the crash: everything after this line simply
	// never happened, exactly as if the process had died right here.
	pending[0]()

	reloadedRootAgain, err := LoadSession(Config{Providers: reg, SessionDir: dir}, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root (after simulated crash): %v", err)
	}
	if !reloadedRootAgain.hasPendingTaskNotifications() {
		t.Error("the ancestor's log lost the notification across the simulated crash — recovery must deliver durably before it closes the child's own history, not after")
	}

	reloadedChildAgain, err := LoadSession(Config{Providers: reg, SessionDir: dir}, childID)
	if err != nil {
		t.Fatalf("LoadSession child (after simulated crash): %v", err)
	}
	if !reloadedChildAgain.hasUnfinalizedTurn() {
		t.Error("the child's history was already closed even though the simulated crash landed before that specific write — a real restart could never retry recovery for this child again")
	}

	// recoverInterruptedTurnLocked also fired go m.fireIdleResumeAsync
	// for root2 (idle when adopted) as a side effect, independent of the
	// deferred-persist truncation this test exercises above. A
	// status-based wait here is itself racy (root2 can still legitimately
	// read StatusIdle at the very first poll, before that goroutine has
	// even acquired m.mu to start — a false "already settled" signal) —
	// a flat settle sleep, long enough that rootProv's zero-scripted-turn
	// Stream() call (which fails immediately once the goroutine does
	// run) has certainly completed by the time this test returns, avoids
	// racing that goroutine's own timing entirely. Without this, the
	// goroutine can outlive this test and race a LATER test's freshly
	// allocated scriptedProvider landing at the same reused memory
	// address — a live -race flake caught under repeated runs.
	time.Sleep(300 * time.Millisecond)
}

// TestHasUnfinalizedTurnIgnoresTrailingMessageShape is the regression
// test replacing an earlier, unreliable trailing-message-role heuristic
// (hasUnansweredTurn, since removed) that a live review proved wrong in
// BOTH directions: it MISSED a genuine mid-tool-loop crash (a trailing
// RoleTool or unresolved-ToolCall shape the original RoleUser-only check
// never examined), and — once widened to also check those — it then
// MISFIRED on an already-SETTLED ordinary failure, since several
// legitimate, fully-settled paths (a plain provider error appending
// nothing at all; appendUnexecutedToolCallResults/interruptedToolResults
// synthesizing a trailing RoleTool on an otherwise-done turn) leave the
// exact same trailing shapes a genuine crash would.
//
// turnUnsettled/hasUnfinalizedTurn (engine.go) sidestep trailing-shape
// guessing entirely: true from the moment ANY message is appended,
// regardless of role, false only once markTurnSettled explicitly runs.
// Proves this directly: three different trailing shapes (mid-tool-loop
// RoleTool, unresolved-ToolCall RoleAssistant, and a bare RoleUser) are
// ALL detected as unfinalized until markTurnSettled runs, at which point
// ALL of them settle — the mechanism cares only about
// append-vs-markTurnSettled ordering, never message role.
func TestHasUnfinalizedTurnIgnoresTrailingMessageShape(t *testing.T) {
	cases := []struct {
		name    string
		history func(s *Session)
	}{
		{"trailing RoleUser", func(s *Session) {
			s.append(message.Message{ID: "u1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}})
		}},
		{"trailing RoleTool (mid-tool-loop crash shape)", func(s *Session) {
			s.append(message.Message{ID: "u1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}})
			s.append(message.Message{ID: "a1", Role: message.RoleAssistant, Parts: message.Parts{
				&message.Text{Text: "running"},
				toolCall("tc1", "bash", `{"command":"echo hi"}`),
			}})
			s.append(message.Message{ID: "t1", Role: message.RoleTool, Parts: message.Parts{
				&message.ToolResult{CallID: "tc1", Content: message.Parts{&message.Text{Text: "hi"}}},
			}})
		}},
		{"trailing unresolved ToolCall", func(s *Session) {
			s.append(message.Message{ID: "u1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}})
			s.append(message.Message{ID: "a1", Role: message.RoleAssistant, Parts: message.Parts{
				&message.Text{Text: "running"},
				toolCall("tc1", "bash", `{"command":"echo hi"}`),
			}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSession(managedConfig("test", scriptedTurns("test", nil)))
			if s.hasUnfinalizedTurn() {
				t.Fatal("a fresh session with no history must not report an unfinalized turn")
			}
			tc.history(s)
			if !s.hasUnfinalizedTurn() {
				t.Errorf("hasUnfinalizedTurn() = false after appending (%s), want true — no markTurnSettled call has happened yet", tc.name)
			}
			s.markTurnSettled()
			if s.hasUnfinalizedTurn() {
				t.Errorf("hasUnfinalizedTurn() = true after markTurnSettled(), want false — trailing shape (%s) must not matter once explicitly settled", tc.name)
			}
		})
	}
}

// TestHasUnfinalizedTurnSurvivesReloadForBothPolarities proves the
// durable side of turnUnsettled/markTurnSettled: recChildTurnSettled's
// own fold in LoadSession (store.go) correctly restores BOTH "still
// dangling" (no settled marker was ever durably written — the genuine
// crash case) and "properly settled" (the marker WAS durably written)
// across a reload, matching the in-memory behavior exactly.
func TestHasUnfinalizedTurnSurvivesReloadForBothPolarities(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Providers: provider.Registry{"test": scriptedTurns("test", nil)}, Model: modelFor("test"), SessionDir: dir}

	danglingID := func() string {
		s := NewSession(cfg)
		s.append(message.Message{ID: "u1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}})
		return s.ID
	}()
	reloadedDangling, err := LoadSession(cfg, danglingID)
	if err != nil {
		t.Fatalf("LoadSession (dangling): %v", err)
	}
	if !reloadedDangling.hasUnfinalizedTurn() {
		t.Error("reloaded session lost its unfinalized-turn state — no recChildTurnSettled record was ever written, so this must still read true")
	}

	settledID := func() string {
		s := NewSession(cfg)
		s.append(message.Message{ID: "u1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}})
		s.markTurnSettled()
		s.persistTurnSettled()
		return s.ID
	}()
	reloadedSettled, err := LoadSession(cfg, settledID)
	if err != nil {
		t.Fatalf("LoadSession (settled): %v", err)
	}
	if reloadedSettled.hasUnfinalizedTurn() {
		t.Error("reloaded session reports an unfinalized turn despite a durably-written recChildTurnSettled record")
	}
}

// TestRecoverInterruptedTurnFiresForChildCrashedMidToolLoop is the
// integration-level companion to the two unit tests above: proves
// recoverInterruptedTurnLocked actually fires (not just hasUnfinalizedTurn
// in isolation) for a child whose durable history ends on a RoleTool
// message. See TestRecoverInterruptedTurnDoesNotRefireForASettledFailure
// below for the equally important negative case: recovery must NOT fire
// for a child whose last turn already reached finalizeTurn.
func TestRecoverInterruptedTurnFiresForChildCrashedMidToolLoop(t *testing.T) {
	dir := t.TempDir()
	reg := provider.Registry{"root": scriptedTurns("root", doneTurn("resumed")), "child": scriptedTurns("child", nil)}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 0, 0)
	root1 := mgr1.NewRoot(rootCfg)

	childID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr1, childID, StatusFailed, time.Second) // scriptedTurns("child", nil) has zero turns, so this Spawn's own turn fails immediately

	childSess, ok := mgr1.Session(childID)
	if !ok {
		t.Fatal("child not tracked")
	}
	// Manually append the exact "crashed mid-tool-loop" shape onto the
	// child's own durable log, on top of whatever its own failed Spawn
	// turn already wrote — simulating a LATER turn (e.g. a session.send
	// follow-up) that itself got interrupted after appending tool
	// results but before the process crashed.
	childSess.append(message.Message{ID: "a2", Role: message.RoleAssistant, Parts: message.Parts{
		&message.Text{Text: "running"},
		toolCall("tc2", "bash", `{"command":"echo hi"}`),
	}})
	childSess.append(message.Message{ID: "t2", Role: message.RoleTool, Parts: message.Parts{
		&message.ToolResult{CallID: "tc2", Content: message.Parts{&message.Text{Text: "hi"}}},
	}})
	if !childSess.hasUnfinalizedTurn() {
		t.Fatal("test setup: manually appended history does not end on the trailing-RoleTool shape")
	}

	// root2 gets its OWN "root"-named provider instance, not rootCfg's
	// shared one: childID's own Spawn call above also wakes root1 (mgr1)
	// with its own failure notification (childID's turn fails
	// immediately, and root1 was idle) — a SEPARATE, independent async
	// resume from the one this test triggers on root2 (mgr2) below.
	// Sharing the exact same *scriptedProvider object between root1 and
	// root2 would let those two independent async resumes race on the
	// fixture's own unsynchronized internal state — a live -race flake
	// caught under repeated runs, same root cause/fix as other tests in
	// this file that adopt a root into a second manager.
	reg2 := provider.Registry{"root": scriptedTurns("root", doneTurn("resumed")), "child": scriptedTurns("child", nil)}
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
	if !reloadedChild.hasUnfinalizedTurn() {
		t.Fatal("test setup: reloaded child lost the trailing-RoleTool shape across LoadSession")
	}
	if err := mgr2.AdoptReloaded(reloadedChild); err != nil {
		t.Fatalf("AdoptReloaded: %v", err)
	}

	info, ok := mgr2.Info(childID)
	if !ok {
		t.Fatal("child not tracked after AdoptReloaded")
	}
	if info.Status != StatusFailed {
		t.Errorf("status = %q, want %q — recovery must fire for a child crashed mid-tool-loop, not just one crashed right after its user message", info.Status, StatusFailed)
	}
	if !strings.Contains(info.FailReason, "restart") {
		t.Errorf("fail_reason = %q, want it to mention the restart", info.FailReason)
	}
}

// TestRecoverInterruptedTurnDoesNotRefireForASettledFailure is the
// regression test for a live review finding: a child's ORDINARY,
// PROPERLY-SETTLED provider-error failure — finalizeTurn ran to
// completion, marked it StatusFailed, and durably delivered its
// notification to the ancestor — used to be indistinguishable from a
// genuine crash by the trailing-message-role heuristic
// recoverInterruptedTurnLocked's guard relied on: runAgenticLoop's plain
// (non-interruptedTurnError) provider-error path appends nothing at all,
// leaving history ending on the bare RoleUser directive, byte-identical
// to what a real crash leaves. A LATER cold reload of this
// ALREADY-SETTLED child (handleSpawnChild cold-loading a reaped/restart-
// forgotten parent to attach a new child under it, or cmd -resume) would
// then misfire recovery: re-marking an already-correctly-failed child
// StatusFailed with a FALSE "lost to restart" reason, and enqueuing a
// SECOND, duplicate StatusFailed notification to an ancestor that
// already durably has the first, correct one.
//
// turnUnsettled/markTurnSettled (engine.go) close this: finalizeTurn
// marks the turn settled REGARDLESS of outcome or trailing message
// shape, so a properly-settled failure reads hasUnfinalizedTurn()=false
// both in memory and after a reload — recovery's guard at the top of
// recoverInterruptedTurnLocked correctly declines to run at all.
func TestRecoverInterruptedTurnDoesNotRefireForASettledFailure(t *testing.T) {
	dir := t.TempDir()
	reg := provider.Registry{"root": scriptedTurns("root", nil), "child": scriptedTurns("child", nil)} // zero turns for both — child's own Spawn turn fails immediately
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 0, 0)
	root1 := mgr1.NewRoot(rootCfg)

	childID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr1, childID, StatusFailed, time.Second)

	childSess, ok := mgr1.Session(childID)
	if !ok {
		t.Fatal("child not tracked")
	}
	// finalizeTurn's own status/turnUnsettled updates both happen
	// synchronously under m.mu, in the SAME critical section
	// waitForStatus's own Info() call observes StatusFailed in — so this
	// in-memory check is reliable immediately, unlike the DURABLE write
	// below (deferred, and not guaranteed to have landed yet).
	if childSess.hasUnfinalizedTurn() {
		t.Fatal("test setup: child's OWN finalizeTurn call should have already marked this turn settled")
	}

	// root2 gets its OWN provider instance — same shared-object race
	// avoidance as the sibling test above.
	reg2 := provider.Registry{"root": scriptedTurns("root", nil), "child": scriptedTurns("child", nil)}
	rootCfg2 := Config{Providers: reg2, Model: modelFor("root"), SessionDir: dir}

	mgr2 := NewSessionManager(context.Background(), 0, 0)
	root2 := NewSession(rootCfg2)
	root2.ID = root1.ID
	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}

	// finalizeTurn's own persistTurnSettled write is deferred (runs
	// after m.mu releases, see unlockAndFlushPersist's own doc comment)
	// — poll the reload rather than assume it has already landed by the
	// time waitForStatus above returned.
	deadline := time.Now().Add(2 * time.Second)
	var reloadedChild *Session
	for {
		reloadedChild, err = LoadSession(Config{Providers: reg2, SessionDir: dir}, childID)
		if err != nil {
			t.Fatalf("LoadSession: %v", err)
		}
		if !reloadedChild.hasUnfinalizedTurn() || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if reloadedChild.hasUnfinalizedTurn() {
		t.Fatal("test setup: reloaded child's settled marker never landed durably")
	}

	if err := mgr2.AdoptReloaded(reloadedChild); err != nil {
		t.Fatalf("AdoptReloaded: %v", err)
	}

	root2.mu.Lock()
	notifCount := len(root2.taskNotifications)
	root2.mu.Unlock()
	if notifCount != 0 {
		t.Errorf("root2.taskNotifications = %d entries, want 0 — recovery must not re-deliver a duplicate notification for an already-settled child", notifCount)
	}
	if info, ok := mgr2.Info(childID); ok && strings.Contains(info.FailReason, "restart") {
		t.Errorf("fail_reason = %q, want it to NOT be re-labeled as a restart loss for a child that failed for an ordinary, already-settled reason", info.FailReason)
	}
}
