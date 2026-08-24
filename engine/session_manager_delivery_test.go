package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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

// TestReAdoptedCanceledChildRestoresStatusCanceledNotFailed is the
// regression test for a live review finding: restoreKnownStatusLocked
// used to restore ANY committed outcome as StatusDone or StatusFailed —
// collapsing a genuinely CANCELED child (Cancel()/cancel_tree, which
// marks n.status StatusCanceled directly, atomically, before
// finalizeTurn ever runs — see cancelOneNodeLocked's own doc comment)
// into StatusFailed the moment it was re-adopted after a restart,
// silently rewriting history: a parent or the UI reading this status
// afterward could no longer distinguish "this child was deliberately
// stopped" from "this child genuinely failed."
//
// Fixed via taskNotification.Canceled (taskdelivery.go) — a distinct,
// durably-committed signal, set ONLY by finalizeTurn's alreadyCanceled
// branch, read by nodeStatusForOutcome and applied by
// restoreKnownStatusLocked (and recoverInterruptedTurnLocked, for the
// analogous in-flight-crash-of-an-already-canceled-turn case) —
// deliberately NOT inferred from FailReason=="canceled": classifySpawnError
// can ALSO produce that exact text for a genuinely FAILED "caught in the
// crossfire" descendant of an AbortTurn call (cancel_tree's own doc
// comment covers that distinction) — string-matching FailReason would
// have wrongly resurrected that case as StatusCanceled too.
func TestReAdoptedCanceledChildRestoresStatusCanceledNotFailed(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", nil)
	childProv := &signaledBlockingProvider{name: "child", started: make(chan struct{}), release: make(chan struct{})}
	reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 3, 0)
	root1 := mgr1.NewRoot(rootCfg)
	childID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-childProv.started // child now genuinely mid-turn

	if err := mgr1.Cancel(childID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitForStatus(t, mgr1, childID, StatusCanceled, 2*time.Second)

	// Wait for finalizeTurn's own alreadyCanceled branch to durably
	// commit and settle before this test reloads the log fresh below —
	// same "settle poll" discipline as every other test in this file
	// exercising a restart.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := LoadSession(Config{Providers: reg, SessionDir: dir}, childID)
		if err != nil {
			t.Fatalf("LoadSession (settle poll): %v", err)
		}
		if !s.hasUnfinalizedTurn() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test setup: child's settled marker never landed durably")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Fresh process: root2 gets its OWN provider instances — same
	// shared-object race avoidance as every other test in this file.
	rootProv2 := scriptedTurns("root", nil)
	childProv2 := scriptedTurns("child", nil)
	reg2 := provider.Registry{rootProv2.Name(): rootProv2, childProv2.Name(): childProv2}
	rootCfg2 := Config{Providers: reg2, Model: modelFor("root"), SessionDir: dir}

	mgr2 := NewSessionManager(context.Background(), 3, 0)
	root2, err := LoadSession(rootCfg2, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root: %v", err)
	}
	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}

	childInfo, ok := mgr2.Info(childID)
	if !ok {
		t.Fatal("child not tracked after AdoptRoot")
	}
	if childInfo.Status != StatusCanceled {
		t.Errorf("child.Status = %q after restart-recovery re-adoption, want %q — a genuinely canceled child must not be rewritten to failed", childInfo.Status, StatusCanceled)
	}
	if childInfo.FailReason != "" {
		t.Errorf("child.FailReason = %q, want empty — mirrors cancelOneNodeLocked's own live bookkeeping, which never sets it for a canceled node", childInfo.FailReason)
	}
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

// TestRecoverInterruptedTurnDoesNotFalselyMarkForwardedNotificationDelivered
// is the regression test for a live review finding on
// recoverInterruptedTurnLocked's own target==nil branch (see
// nearestLiveAncestorLocked's "no reachable ancestor" case in that
// method's doc comment): an earlier version of this method called
// persistDeliveredTaskNotifications(forwarded) UNCONDITIONALLY, even when
// target was nil and forwarded was actually being dropped, not delivered
// (see the else branch just above that call in recoverInterruptedTurnLocked
// — "forwarded is simply dropped here"). That durably wrote a
// recTaskNotifyDelivered record, on mid's OWN log, for a grandchild
// notification nobody ever received — LoadSession's queued-minus-delivered
// fold would then treat it as resolved forever, permanently and silently
// hiding the fact that it was actually lost. finalizeTurn's own sibling
// block never had this bug (its persistDeliveredTaskNotifications call
// already lives strictly inside its own `target != nil` branch) — this
// fix makes recoverInterruptedTurnLocked match that exactly.
//
// Engineered the same three-level root/mid/grand setup
// TestRecoverInterruptedTurnForwardsGrandchildNotifications uses, but the
// fresh process deliberately never re-adopts root at all before adopting
// mid — so when recovery runs for mid, mid's own parentID is untracked,
// nearestLiveAncestorLocked returns nil immediately, and forwarded is
// dropped rather than delivered. Reloading mid's log a THIRD time (the
// ground truth any later process would see) must still show the
// grandchild's notification as pending, not falsely resolved.
func TestRecoverInterruptedTurnDoesNotFalselyMarkForwardedNotificationDelivered(t *testing.T) {
	dir := t.TempDir()
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

	// Simulate a crash: mid's blocked goroutine is simply abandoned. The
	// fresh process below never adopts root at all, unlike the sibling
	// forwarding test — mid's own durable TaskParentID still names root's
	// id, but mgr2 has no node tracked under it, so
	// nearestLiveAncestorLocked(mid) will find nothing and return nil.
	mgr2 := NewSessionManager(context.Background(), 3, 0)

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

	// Give recoverInterruptedTurnLocked's deferred persists a moment to
	// flush (see unlockAndFlushPersist's own doc comment) before reading
	// the durable ground truth back.
	time.Sleep(100 * time.Millisecond)

	reloadedAgain, err := LoadSession(Config{Providers: reg, SessionDir: dir}, midID)
	if err != nil {
		t.Fatalf("second LoadSession: %v", err)
	}
	if !reloadedAgain.hasPendingTaskNotifications() {
		t.Fatal("grandchild's forwarded notification was falsely persisted as delivered (recTaskNotifyDelivered written with no live target) — it should still read back as pending/lost, not silently resolved")
	}
	var sawGrand bool
	for _, n := range reloadedAgain.taskNotifications {
		if n.ChildID == grandID {
			sawGrand = true
		}
	}
	if !sawGrand {
		t.Errorf("reloadedAgain.taskNotifications = %+v, want the grandchild's notification still present (undelivered)", reloadedAgain.taskNotifications)
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

	if len(pending) < 3 {
		t.Fatalf("recovery queued %d deferred persists, want at least 3 (commit-outcome, then notification delivery, then the closing-message append)", len(pending))
	}
	// Run ONLY the first TWO thunks — commitOutcomeLocked's own commit
	// persist (step 1 of the crash-window table on
	// recoverInterruptedTurnLocked's own doc comment), then the
	// notification delivery (step 2) — never any later one. This is the
	// crash: everything after this line simply never happened, exactly
	// as if the process had died right here.
	pending[0]()
	pending[1]()

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
	// The child's own first (immediately-failing) turn already ran
	// finalizeTurn, which durably queues its persistTurnSettled() write
	// via m.deferPersist — that write runs on finalizeTurn's OWN
	// goroutine, entirely independent of (and racing) this test's own
	// goroutine below. Wait for it to actually land on disk before this
	// test appends anything further to the SAME log: otherwise the two
	// goroutines' writes to childID's log file can interleave out of
	// order, letting the settled-marker record land AFTER the manual
	// appends below and falsify the very shape this test means to set
	// up — a live flake caught under repeated -count runs.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := LoadSession(Config{Providers: reg, SessionDir: dir}, childID)
		if err != nil {
			t.Fatalf("LoadSession (settle poll): %v", err)
		}
		if !s.hasUnfinalizedTurn() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test setup: child's first-turn settled marker never landed durably")
		}
		time.Sleep(5 * time.Millisecond)
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

// TestRecoverInterruptedTurnDeliversRealResultInsteadOfFalseFailure is the
// regression test for a live review finding on
// recoverInterruptedTurnLocked's OTHER direction from the sibling test
// above: a turn that genuinely, naturally FINISHED — the model's last
// response is a plain final answer with no pending tool call, appended to
// the child's own durable history via appendWithUsage — but the process
// crashed before finalizeTurn ever ran (or ran far enough to durably write
// the child_turn.settled marker): the "notify->settled window" a live
// review named directly. An earlier version of recoverInterruptedTurnLocked
// always synthesized a StatusFailed "lost to restart" notification for
// ANY detected crash, with no attempt to tell this case apart from a
// genuine mid-turn crash — so a parent whose child actually succeeded was
// durably, permanently told it failed. The review judged that worse than a
// merely-late notification: a lost notification is honestly absent, but a
// false failure actively misinforms with nothing left to correct it.
//
// settledSuccessResult (engine.go) closes this for the one unambiguous
// shape it covers — see its own doc comment for exactly which shape and
// why. This test manually engineers that exact shape (a trailing
// RoleAssistant message with real text and no ToolCall part) on top of an
// otherwise-unsettled child, the same "manually append, then reload"
// technique TestRecoverInterruptedTurnFiresForChildCrashedMidToolLoop
// uses for its own negative case.
func TestRecoverInterruptedTurnDeliversRealResultInsteadOfFalseFailure(t *testing.T) {
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
	// The child's own first (immediately-failing) turn already ran
	// finalizeTurn, which durably queues its persistTurnSettled() write
	// via m.deferPersist — that write runs on finalizeTurn's OWN
	// goroutine, entirely independent of (and racing) this test's own
	// goroutine below. Wait for it to actually land on disk before this
	// test appends anything further to the SAME log: otherwise the two
	// goroutines' writes to childID's log file can interleave out of
	// order, letting the settled-marker record land AFTER the manual
	// appends below and falsify the very shape this test means to set
	// up — a live flake caught under repeated -count runs.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := LoadSession(Config{Providers: reg, SessionDir: dir}, childID)
		if err != nil {
			t.Fatalf("LoadSession (settle poll): %v", err)
		}
		if !s.hasUnfinalizedTurn() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test setup: child's first-turn settled marker never landed durably")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Manually append the exact shape a LATER, genuinely-successful
	// follow-up turn leaves in history — a user directive, then the
	// model's own plain final answer, no tool calls — WITHOUT ever
	// calling finalizeTurn/markTurnSettled for it, simulating a crash
	// that struck after the append but before that bookkeeping landed.
	const finalAnswer = "the real, successful result"
	childSess.append(message.Message{ID: "u2", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "follow up"}}})
	childSess.append(message.Message{ID: "a2", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: finalAnswer}}})
	if !childSess.hasUnfinalizedTurn() {
		t.Fatal("test setup: manually appended history should still read as unsettled (no markTurnSettled call was made)")
	}

	// root2 gets its OWN provider instance — same shared-object race
	// avoidance as the sibling tests in this file.
	reg2 := provider.Registry{"root": scriptedTurns("root", nil), "child": scriptedTurns("child", nil)}
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
		t.Fatal("test setup: reloaded child lost the trailing clean-completion shape across LoadSession")
	}
	if err := mgr2.AdoptReloaded(reloadedChild); err != nil {
		t.Fatalf("AdoptReloaded: %v", err)
	}

	info, ok := mgr2.Info(childID)
	if !ok {
		t.Fatal("child not tracked after AdoptReloaded")
	}
	if info.Status != StatusDone {
		t.Errorf("status = %q, want %q — a child whose turn genuinely finished must be reported DONE, not a false restart failure", info.Status, StatusDone)
	}
	if info.Result != finalAnswer {
		t.Errorf("result = %q, want the child's real final answer %q", info.Result, finalAnswer)
	}
	if strings.Contains(info.FailReason, "restart") {
		t.Errorf("fail_reason = %q, want no restart-loss label on a child that actually succeeded", info.FailReason)
	}

	// The ancestor-facing notification must match: StatusDone with the
	// real result, never a StatusFailed "lost to restart" one. root2 is
	// genuinely idle when recovery delivers to it, so this triggers a
	// REAL active resume (go m.fireIdleResumeAsync) racing this test's
	// own read — rootProv's zero scripted turns make that resume's own
	// Stream() call fail immediately, requeuing the notification back
	// onto root2.taskNotifications, but the checkout (into
	// taskNotificationsInFlight) and requeue both happen on that OTHER
	// goroutine, asynchronously — same race and same fix as
	// TestRecoverInterruptedTurnForwardsGrandchildNotifications above.
	countAll := func() int {
		root2.mu.Lock()
		defer root2.mu.Unlock()
		return len(root2.taskNotifications) + len(root2.taskNotificationsInFlight)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && countAll() != 1 {
		time.Sleep(5 * time.Millisecond)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		root2.mu.Lock()
		n := len(root2.taskNotifications)
		root2.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	root2.mu.Lock()
	defer root2.mu.Unlock()
	if len(root2.taskNotifications) != 1 {
		t.Fatalf("root2.taskNotifications = %+v (in-flight: %+v), want exactly 1 settled entry", root2.taskNotifications, root2.taskNotificationsInFlight)
	}
	got := root2.taskNotifications[0]
	if got.Status != StatusDone || got.Result != finalAnswer {
		t.Errorf("notification = %+v, want Status=%q Result=%q", got, StatusDone, finalAnswer)
	}
}

// removeLogRecords removes every matching record from id's own on-disk
// session log in dir, simulating "the process died before this record
// ever landed durably" — a more direct, implementation-independent way to
// manufacture a specific partial-crash durable state than intercepting
// SessionManager.pendingPersist mid-flush (which only works for a
// manually-driven method call, not a real finalizeTurn run happening on
// its own goroutine). Requires each record to be exactly one line ending
// in a newline, true of every record this package writes (store.go's
// writeRecord). Matched by TYPE (and, for a task-notify record, by
// ChildID) rather than by POSITION — root's own log in particular is not
// guaranteed to end with the record a test wants gone: delivering to an
// idle root can trigger its own async resume attempt, which may append
// further records afterward, so "drop the last line" is only reliable
// for a log nothing else ever touches again (a terminal child's own).
func removeLogRecords(t *testing.T, dir, id string, match func(record) bool) {
	t.Helper()
	path := sessionPath(dir, id)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var kept []string
	removed := 0
	for _, line := range lines {
		if line == "" {
			continue
		}
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if match(rec) {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if removed == 0 {
		t.Fatalf("removeLogRecords: no matching record found in %s", path)
	}
	out := strings.Join(kept, "\n")
	if len(kept) > 0 {
		out += "\n"
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatalf("rewrite log %s: %v", path, err)
	}
}

// TestFinalizeTurnCrashBeforeDeliveryStillDeliversViaRecovery is the
// regression test for the crash-window table's step 1 (on
// recoverInterruptedTurnLocked's own doc comment): a crash landing
// between finalizeTurn's own commit-outcome persist and its notification-
// delivery persist. Lets a REAL finalizeTurn run to full, natural
// completion for a failed child (so the on-disk log is genuinely correct
// end to end), then removes the trailing records that a real crash at
// this exact point would have prevented from ever landing — the
// notification never reaching the ancestor's log, and (necessarily, since
// it is queued strictly AFTER delivery) the settled marker never reaching
// the child's own log either. Proves recovery, on a fresh reload,
// correctly DELIVERS the notification anyway — no loss — using the
// already-committed record rather than needing to guess.
func TestFinalizeTurnCrashBeforeDeliveryStillDeliversViaRecovery(t *testing.T) {
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
	origInfo, ok := mgr1.Info(childID)
	if !ok {
		t.Fatal("child not tracked")
	}
	if origInfo.FailReason == "" || strings.Contains(origInfo.FailReason, "restart") {
		t.Fatalf("test setup: unexpected original fail_reason %q", origInfo.FailReason)
	}

	// finalizeTurn's own deferred persists are asynchronous relative to
	// waitForStatus's in-memory read above — poll until the child's log
	// shows it fully settled (all four steps landed) before manufacturing
	// the partial-crash state on top of that complete, correct log.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := LoadSession(Config{Providers: reg, SessionDir: dir}, childID)
		if err != nil {
			t.Fatalf("LoadSession (settle poll): %v", err)
		}
		if !s.hasUnfinalizedTurn() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test setup: child's settled marker never landed durably")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Simulate the crash: strip the child's own settled marker (step 4)
	// and the notification this real run delivered to root's log (step
	// 2) — both records a crash landing before delivery would have
	// prevented. The commit record (step 1) stays, exactly as a real
	// crash at this point would leave it.
	removeLogRecords(t, dir, childID, func(r record) bool { return r.Type == recChildTurnSettled })
	removeLogRecords(t, dir, root1.ID, func(r record) bool {
		return r.Type == recTaskNotifyQueued && r.TaskNotify != nil && r.TaskNotify.ChildID == childID
	})

	// The reloaded side gets its OWN provider instances, not reg's shared
	// ones: mgr1's own root1 can independently still be settling/retrying
	// in the background (it was ALSO idle when the child's real failure
	// notification arrived) using reg's objects — sharing them with mgr2
	// below would let those two independent goroutines race on the
	// fixture's own unsynchronized internal state, a live -race flake
	// caught under repeated runs, same root cause/fix as every other test
	// in this file that adopts a root into a second manager.
	reg2 := provider.Registry{"root": scriptedTurns("root", nil), "child": scriptedTurns("child", nil)}
	reloadedRoot, err := LoadSession(Config{Providers: reg2, SessionDir: dir}, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root: %v", err)
	}
	if reloadedRoot.hasPendingTaskNotifications() {
		t.Fatal("test setup: root's log still shows the notification after dropping its trailing line")
	}

	mgr2 := NewSessionManager(context.Background(), 0, 0)
	// Adopted via the low-level adoptLocked, NOT the public AdoptRoot --
	// AdoptRoot now also runs recoverCrashedChildrenLocked (a live prod
	// finding, see that method's own doc comment), which would recover
	// the crashed child automatically, right here, before this test gets
	// a chance to drive its own fine-grained, step-by-step simulation of
	// the exact crash window below. Depth 0, no parentID, matches
	// adoptRootLocked's own registration exactly, minus the sweep.
	mgr2.mu.Lock()
	reloadedRoot.cfg.SessionManager = mgr2
	mgr2.adoptLocked(reloadedRoot, "", 0)
	mgr2.mu.Unlock()
	reloadedChild, err := LoadSession(Config{Providers: reg2, SessionDir: dir}, childID)
	if err != nil {
		t.Fatalf("LoadSession child: %v", err)
	}
	if !reloadedChild.hasUnfinalizedTurn() {
		t.Fatal("test setup: reloaded child does not read as unsettled after dropping its trailing line")
	}
	if err := mgr2.AdoptReloaded(reloadedChild); err != nil {
		t.Fatalf("AdoptReloaded: %v", err)
	}

	// reloadedRoot is genuinely idle when recovery delivers to it, so this
	// triggers a REAL active resume (go m.fireIdleResumeAsync) racing
	// this test's own read below — rootProv's zero scripted turns make
	// that resume's own Stream() call fail immediately, requeuing the
	// notification back onto reloadedRoot.taskNotifications, but the
	// checkout (into taskNotificationsInFlight) and requeue both happen
	// on that OTHER goroutine, asynchronously — same race and same fix
	// as TestRecoverInterruptedTurnForwardsGrandchildNotifications.
	countAll := func() int {
		reloadedRoot.mu.Lock()
		defer reloadedRoot.mu.Unlock()
		return len(reloadedRoot.taskNotifications) + len(reloadedRoot.taskNotificationsInFlight)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && countAll() != 1 {
		time.Sleep(5 * time.Millisecond)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		reloadedRoot.mu.Lock()
		n := len(reloadedRoot.taskNotifications)
		reloadedRoot.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	reloadedRoot.mu.Lock()
	defer reloadedRoot.mu.Unlock()
	if len(reloadedRoot.taskNotifications) != 1 {
		t.Fatalf("root.taskNotifications = %+v (in-flight: %+v), want exactly 1 entry — recovery must deliver, not lose, the notification a crash-before-delivery left uncommitted to the ancestor's log", reloadedRoot.taskNotifications, reloadedRoot.taskNotificationsInFlight)
	}
	got := reloadedRoot.taskNotifications[0]
	if got.ChildID != childID || got.Status != StatusFailed || got.FailReason != origInfo.FailReason {
		t.Errorf("notification = %+v, want ChildID=%q Status=%q FailReason=%q (the ORIGINAL classified reason)", got, childID, StatusFailed, origInfo.FailReason)
	}
}

// TestFinalizeTurnCrashAfterDeliveryReplaysIdenticalFailureNotDivergent is
// the regression test for the crash-window table's step 2 and the exact
// live review finding this whole mechanism exists to close: a crash
// landing between finalizeTurn's own notification-delivery persist and
// its settled-marker persist. Before committedOutcome existed, recovery
// reconstructed a FRESH notify from trailing-history shape on a re-adopt
// — for an ordinary failure (which appends nothing to history), that was
// ALWAYS the generic "lost to restart" text, never the real classified
// reason finalizeTurn had already durably delivered. Since the two
// payloads differ, enqueueTaskNotificationMemoryOnlyDeduped's exact-`==`
// dedup could not recognize them as the same event: the ancestor ended up
// told the same child both failed for its real reason AND was
// "lost to restart" — two different accounts of one failure.
//
// Same "let a real finalizeTurn run, then strip only what a crash at
// this exact point would have prevented" technique as the sibling test
// above — here only the child's own trailing settled-marker line is
// dropped; the real, already-delivered notification on root's log is
// left untouched, exactly as a crash AFTER delivery landed would leave
// it.
func TestFinalizeTurnCrashAfterDeliveryReplaysIdenticalFailureNotDivergent(t *testing.T) {
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
	origInfo, ok := mgr1.Info(childID)
	if !ok {
		t.Fatal("child not tracked")
	}
	if origInfo.FailReason == "" || strings.Contains(origInfo.FailReason, "restart") {
		t.Fatalf("test setup: unexpected original fail_reason %q", origInfo.FailReason)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := LoadSession(Config{Providers: reg, SessionDir: dir}, childID)
		if err != nil {
			t.Fatalf("LoadSession (settle poll): %v", err)
		}
		if !s.hasUnfinalizedTurn() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test setup: child's settled marker never landed durably")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Simulate the crash: strip ONLY the child's own settled marker
	// (step 4) — the real notification (step 2) already durably landed
	// on root's log and stays there, exactly as a crash landing AFTER
	// delivery but before settling would leave it.
	removeLogRecords(t, dir, childID, func(r record) bool { return r.Type == recChildTurnSettled })

	// The reloaded side gets its OWN provider instances, not reg's shared
	// ones — same shared-object race avoidance as the sibling test above.
	reg2 := provider.Registry{"root": scriptedTurns("root", nil), "child": scriptedTurns("child", nil)}
	reloadedRoot, err := LoadSession(Config{Providers: reg2, SessionDir: dir}, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root: %v", err)
	}
	if !reloadedRoot.hasPendingTaskNotifications() {
		t.Fatal("test setup: root's log lost the real, already-delivered notification")
	}

	mgr2 := NewSessionManager(context.Background(), 0, 0)
	// Adopted via the low-level adoptLocked, NOT the public AdoptRoot --
	// AdoptRoot now also runs recoverCrashedChildrenLocked (a live prod
	// finding, see that method's own doc comment), which would recover
	// the crashed child automatically, right here, before this test gets
	// a chance to drive its own fine-grained, step-by-step simulation of
	// the exact crash window below. Depth 0, no parentID, matches
	// adoptRootLocked's own registration exactly, minus the sweep.
	mgr2.mu.Lock()
	reloadedRoot.cfg.SessionManager = mgr2
	mgr2.adoptLocked(reloadedRoot, "", 0)
	mgr2.mu.Unlock()
	reloadedChild, err := LoadSession(Config{Providers: reg2, SessionDir: dir}, childID)
	if err != nil {
		t.Fatalf("LoadSession child: %v", err)
	}
	if !reloadedChild.hasUnfinalizedTurn() {
		t.Fatal("test setup: reloaded child does not read as unsettled after dropping its trailing line")
	}
	if err := mgr2.AdoptReloaded(reloadedChild); err != nil {
		t.Fatalf("AdoptReloaded: %v", err)
	}

	// reloadedRoot is genuinely idle when AdoptReloaded runs, so this can
	// trigger a REAL active resume (go m.fireIdleResumeAsync) racing this
	// test's own read below, the same as the sibling test above — poll
	// rather than read once.
	countAll := func() int {
		reloadedRoot.mu.Lock()
		defer reloadedRoot.mu.Unlock()
		return len(reloadedRoot.taskNotifications) + len(reloadedRoot.taskNotificationsInFlight)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && countAll() != 1 {
		time.Sleep(5 * time.Millisecond)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		reloadedRoot.mu.Lock()
		n := len(reloadedRoot.taskNotifications)
		reloadedRoot.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	reloadedRoot.mu.Lock()
	defer reloadedRoot.mu.Unlock()
	// The crux of the fix: exactly ONE notification for this child, with
	// the REAL classified reason — never a second, divergent "lost to
	// restart" entry alongside it.
	var matching []taskNotification
	for _, n := range reloadedRoot.taskNotifications {
		if n.ChildID == childID {
			matching = append(matching, n)
		}
	}
	if len(matching) != 1 {
		t.Fatalf("root.taskNotifications for childID = %+v (in-flight: %+v), want exactly 1 — a divergent duplicate means the parent is told the same child both failed for its real reason and was separately \"lost to restart\"", matching, reloadedRoot.taskNotificationsInFlight)
	}
	if matching[0].FailReason != origInfo.FailReason {
		t.Errorf("fail_reason = %q, want the ORIGINAL classified reason %q, not a re-derived generic one", matching[0].FailReason, origInfo.FailReason)
	}
	if strings.Contains(matching[0].FailReason, "restart") {
		t.Errorf("fail_reason = %q, want it to NOT be re-labeled as a restart loss for a child that already durably delivered its real reason", matching[0].FailReason)
	}
}

// TestRecoveryCrashBetweenClosingMessageAndSettleDoesNotMisreportSuccess
// is the regression test for the crash-window table's step 3 and the
// OTHER divergent-duplicate shape a live review found: a crash landing
// INSIDE recoverInterruptedTurnLocked's own delivery/settle sequence,
// between its synthetic lostToRestartText closing-message append and its
// own settled-marker persist. Before committedOutcome existed, a SECOND
// recovery attempt would re-run settledSuccessResult(), which — by the
// time the FIRST attempt's closing message has landed durably — now sees
// a trailing RoleAssistant message with plain text and no ToolCall part:
// exactly the shape settledSuccessResult treats as an unambiguous natural
// completion. It would misread the FIRST attempt's own "this turn was
// interrupted" marker as a genuine successful answer, and deliver a
// SECOND, divergent DONE{Result: the marker text} notification alongside
// the FAILED one already queued.
//
// Simulates this directly: drives adoptReloadedLocked(recover=true)
// manually (the same technique
// TestRecoverInterruptedTurnSurvivesACrashBetweenDeliveryAndHistoryClose
// uses), runs every queued thunk EXCEPT the last (the settled-marker
// persist) to simulate the FIRST recovery attempt crashing right after
// its own closing-message append landed, then drives recovery a SECOND
// time on a fresh reload and asserts it replays the IDENTICAL FAILED
// outcome — never a spurious DONE.
func TestRecoveryCrashBetweenClosingMessageAndSettleDoesNotMisreportSuccess(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", nil)
	childProv := &signaledBlockingProvider{name: "child", started: make(chan struct{}), release: make(chan struct{})}
	reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 0, 0)
	root1 := mgr1.NewRoot(rootCfg)
	childID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-childProv.started // mid-turn, genuinely crash-shaped: abandon it, never release

	// First recovery attempt: fresh manager, manually driven so its own
	// deferred persists can be selectively run.
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

	mgr2.mu.Lock()
	mgr2.adoptReloadedLocked(reloadedChild, true)
	pending := mgr2.pendingPersist
	mgr2.pendingPersist = nil
	mgr2.mu.Unlock()
	if len(pending) < 4 {
		t.Fatalf("recovery queued %d deferred persists, want at least 4 (commit, delivery, closing-message, settled-marker)", len(pending))
	}
	// Run every thunk EXCEPT the last one (the settled-marker persist) —
	// the crash lands right after the closing-message append's own
	// durable write, before this recovery attempt could settle the turn.
	for _, fn := range pending[:len(pending)-1] {
		fn()
	}

	origInfo, ok := mgr2.Info(childID)
	if !ok {
		t.Fatal("child not tracked after first recovery attempt")
	}
	if origInfo.Status != StatusFailed || !strings.Contains(origInfo.FailReason, "restart") {
		t.Fatalf("test setup: first recovery attempt status = %+v, want a generic restart failure", origInfo)
	}

	// Third process: a fresh reload sees the closing message durably in
	// history, but no settled marker — the exact "step 3" crash-window
	// state, and the shape that used to fool settledSuccessResult().
	rootProv3 := scriptedTurns("root", nil)
	reg3 := provider.Registry{rootProv3.Name(): rootProv3, childProv.Name(): childProv}
	rootCfg3 := Config{Providers: reg3, Model: modelFor("root"), SessionDir: dir}

	mgr3 := NewSessionManager(context.Background(), 0, 0)
	root3 := NewSession(rootCfg3)
	root3.ID = root1.ID
	if err := mgr3.AdoptRoot(root3); err != nil {
		t.Fatalf("AdoptRoot (third process): %v", err)
	}
	reloadedChildAgain, err := LoadSession(Config{Providers: reg3, SessionDir: dir}, childID)
	if err != nil {
		t.Fatalf("LoadSession (third process): %v", err)
	}
	if !reloadedChildAgain.hasUnfinalizedTurn() {
		t.Fatal("test setup: reloaded child already reads as settled — the simulated crash truncation did not work")
	}
	if !reloadedChildAgain.hasTrailingLostToRestartMarker() {
		t.Fatal("test setup: reloaded child's trailing message is not the synthetic closing marker — first recovery attempt's own append did not survive the reload")
	}

	if err := mgr3.AdoptReloaded(reloadedChildAgain); err != nil {
		t.Fatalf("AdoptReloaded (third process): %v", err)
	}

	info, ok := mgr3.Info(childID)
	if !ok {
		t.Fatal("child not tracked after second recovery attempt")
	}
	if info.Status != StatusFailed {
		t.Errorf("status = %q, want %q — a second recovery pass must not misread its own synthetic closing message as a genuine successful completion", info.Status, StatusFailed)
	}
	if info.Result != "" {
		t.Errorf("result = %q, want empty — a DONE result here would mean the synthetic \"[harness: interrupted...]\" marker text was mistaken for a real answer", info.Result)
	}
	if !strings.Contains(info.FailReason, "restart") {
		t.Errorf("fail_reason = %q, want it to still mention the restart", info.FailReason)
	}

	// root3 is genuinely idle when AdoptReloaded runs recovery a second
	// time, so this can trigger a REAL active resume (go
	// m.fireIdleResumeAsync) racing this test's own read below, the same
	// as the sibling tests above — poll rather than read once.
	countAll := func() int {
		root3.mu.Lock()
		defer root3.mu.Unlock()
		return len(root3.taskNotifications) + len(root3.taskNotificationsInFlight)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && countAll() != 1 {
		time.Sleep(5 * time.Millisecond)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		root3.mu.Lock()
		n := len(root3.taskNotifications)
		root3.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	root3.mu.Lock()
	defer root3.mu.Unlock()
	var matching []taskNotification
	for _, n := range root3.taskNotifications {
		if n.ChildID == childID {
			matching = append(matching, n)
		}
	}
	if len(matching) != 1 {
		t.Fatalf("root.taskNotifications for childID = %+v (in-flight: %+v), want exactly 1 — a divergent duplicate means the parent is told the same child both failed AND succeeded (with the interrupt marker text as its \"result\")", matching, root3.taskNotificationsInFlight)
	}
	if matching[0].Status != StatusFailed {
		t.Errorf("notification.Status = %q, want %q", matching[0].Status, StatusFailed)
	}
}

// TestFinalizeTurnSettlesADurablyParentedButUntrackedNode is the
// regression test for a live review finding: finalizeTurn's own
// settled-marker (and, now, commit-outcome) gate used to check the
// IN-MEMORY sessionNode.parentID, but adoptReloadedLocked's own
// root/non-root branch — which decides whether a reloaded node is a
// recovery CANDIDATE at all — checks the DURABLE TaskParentID() instead.
// The two normally agree, except for adoptReloadedLocked's own "true
// depth is unrecoverable" case (its own doc comment): a child whose real
// parent is not tracked in THIS process gets adopted with an in-memory
// parentID of "" (root-shaped) even though it durably DOES have a real
// TaskParentID. Gating the settled-marker on the in-memory pointer meant
// such a node's turns were NEVER marked settled, even on a completely
// ordinary, successful completion — hasUnfinalizedTurn() stayed true
// forever, and a LATER re-adoption spuriously ran recovery against a
// turn that had already finished cleanly (adoptReloadedLocked's own
// root/non-root branch does NOT treat this node as a root, since it
// checks the durable field, so recovery genuinely does fire for it).
//
// Reproduces the degraded shape directly (same technique as
// TestRecoverInterruptedTurnDoesNotFalselyMarkForwardedNotificationDelivered
// earlier in this file): adopt a child alone into a fresh manager that
// never tracks its parent, drive one MORE ordinary, successful turn on
// it via Send, and prove the turn actually gets marked settled — both in
// memory immediately and durably on a THIRD reload.
func TestFinalizeTurnSettlesADurablyParentedButUntrackedNode(t *testing.T) {
	dir := t.TempDir()
	childProv := scriptedTurns("child", doneTurn("first turn done"))
	reg := provider.Registry{"root": scriptedTurns("root", nil), "child": childProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 3, 0)
	root1 := mgr1.NewRoot(rootCfg)
	childID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr1, childID, StatusDone, time.Second)

	// finalizeTurn's own deferred persists are asynchronous relative to
	// waitForStatus's in-memory read above — poll until the child's log
	// shows it fully settled before reloading it into a second manager
	// below, otherwise the settled marker can land AFTER this test's own
	// reload, falsely reading as still-unsettled — a live flake caught
	// under repeated -count runs.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := LoadSession(Config{Providers: reg, SessionDir: dir}, childID)
		if err != nil {
			t.Fatalf("LoadSession (settle poll): %v", err)
		}
		if !s.hasUnfinalizedTurn() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test setup: child's first-turn settled marker never landed durably")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A fresh manager that never adopts root1 at all — mid's own
	// parentID is untracked, the exact "true depth is unrecoverable"
	// degraded shape: adoptReloadedLocked's root/non-root branch (durable
	// TaskParentID()) still treats it as non-root, but the in-memory node
	// it builds gets parentID == "".
	childProv2 := scriptedTurns("child", doneTurn("second turn also done"))
	reg2 := provider.Registry{"child": childProv2}
	reloadedChild, err := LoadSession(Config{Providers: reg2, SessionDir: dir}, childID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if reloadedChild.hasUnfinalizedTurn() {
		t.Fatal("test setup: reloaded child already reads as unsettled before this test even drives a new turn")
	}

	mgr2 := NewSessionManager(context.Background(), 3, 0)
	if err := mgr2.AdoptReloaded(reloadedChild); err != nil {
		t.Fatalf("AdoptReloaded: %v", err)
	}

	// Drive ONE MORE ordinary, successful turn on this degraded node —
	// finalizeTurn's own settled-marker/commit-outcome gate is what is
	// under test here.
	if _, err := mgr2.Send(context.Background(), childID, "one more thing"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	childSess, ok := mgr2.Session(childID)
	if !ok {
		t.Fatal("child not tracked after Send")
	}
	if childSess.hasUnfinalizedTurn() {
		t.Error("hasUnfinalizedTurn() = true after an ordinary, successful Send turn on a durably-parented-but-untracked node — finalizeTurn's settled-marker gate did not fire for it")
	}

	// Durable proof, not just in-memory: a THIRD, completely fresh
	// process reload must ALSO read this turn as settled — otherwise a
	// later AdoptReloaded(recover=true) would spuriously run recovery
	// against a turn that actually finished cleanly.
	reloadedAgain, err := LoadSession(Config{Providers: reg2, SessionDir: dir}, childID)
	if err != nil {
		t.Fatalf("second LoadSession: %v", err)
	}
	if reloadedAgain.hasUnfinalizedTurn() {
		t.Error("reloadedAgain.hasUnfinalizedTurn() = true — the settled marker never durably landed for this degraded node's turn")
	}
}

// TestAdoptRootRecoversCrashedChildNeverTouchedDirectly is the regression
// test for a live prod finding (a restartPolicy:Always box, harness serve
// as PID 1, kill -9 mid-child-turn): recoverInterruptedTurnLocked only
// ever fires reactively, on next touch of the CRASHED child's own id (see
// its own "purely reactive" doc section) — a caller whose only
// post-restart traffic touches the ROOT (a read-only transcript/session
// GET, or a later follow-up turn on the root itself) never independently
// reloads the crashed child, so that trigger never fires and the root
// waits forever for a notification that was always detectable the moment
// the root itself was adopted again.
//
// Proves the fix (SessionManager.recoverCrashedChildrenLocked, wired into
// adoptRootLocked/adoptReloadedLocked): AdoptRoot on the reloaded root —
// with NO AdoptReloaded/Send/any other call ever touching the crashed
// child's own id — is enough, by itself, to discover the child (via its
// durable Session.SpawnedChildIDs fold) and recover it, delivering the
// lost-to-restart notification to the root.
func TestAdoptRootRecoversCrashedChildNeverTouchedDirectly(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", nil)
	childProv := &signaledBlockingProvider{name: "child", started: make(chan struct{}), release: make(chan struct{})}
	reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 3, 0)
	root1 := mgr1.NewRoot(rootCfg)
	childID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-childProv.started // child now genuinely mid-turn, blocked — simulate "kill -9 1" by simply abandoning it, never releasing

	// Fresh process: a brand-new SessionManager, sharing only the on-disk
	// store. root2 gets its OWN provider instances — same shared-object
	// race avoidance as every other test in this file that adopts a root
	// into a second manager.
	rootProv2 := scriptedTurns("root", nil)
	childProv2 := scriptedTurns("child", nil)
	reg2 := provider.Registry{rootProv2.Name(): rootProv2, childProv2.Name(): childProv2}
	rootCfg2 := Config{Providers: reg2, Model: modelFor("root"), SessionDir: dir}

	mgr2 := NewSessionManager(context.Background(), 3, 0)
	root2, err := LoadSession(rootCfg2, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root: %v", err)
	}

	// The ONLY call this test makes against mgr2 at all — no
	// AdoptReloaded, no Send, nothing ever names childID directly. If
	// this alone doesn't recover the child, nothing in this test's own
	// flow ever will.
	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}

	info, ok := mgr2.Info(childID)
	if !ok {
		t.Fatal("child not tracked after AdoptRoot — recoverCrashedChildrenLocked did not adopt it")
	}
	if info.Status != StatusFailed || !strings.Contains(info.FailReason, "restart") {
		t.Errorf("child info = %+v, want StatusFailed with a restart-loss fail_reason", info)
	}

	// root2 is genuinely idle when AdoptRoot's own recoverCrashedChildrenLocked
	// delivers to it, so this triggers a REAL active resume (go
	// m.fireIdleResumeAsync) racing this test's own read below — rootProv2's
	// zero scripted turns make that resume's own Stream() call fail
	// immediately, requeuing the notification back onto
	// root2.taskNotifications, but the checkout (into
	// taskNotificationsInFlight) and requeue both happen on that OTHER
	// goroutine, asynchronously — same race and same fix as
	// TestRecoverInterruptedTurnForwardsGrandchildNotifications elsewhere
	// in this file.
	countAll := func() int {
		root2.mu.Lock()
		defer root2.mu.Unlock()
		return len(root2.taskNotifications) + len(root2.taskNotificationsInFlight)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && countAll() != 1 {
		time.Sleep(5 * time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		root2.mu.Lock()
		n := len(root2.taskNotifications)
		root2.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	root2.mu.Lock()
	defer root2.mu.Unlock()
	var matching []taskNotification
	for _, n := range root2.taskNotifications {
		if n.ChildID == childID {
			matching = append(matching, n)
		}
	}
	if len(matching) != 1 {
		t.Fatalf("root2.taskNotifications for childID = %+v (in-flight: %+v), want exactly 1 — the root must receive the crashed child's lost-to-restart notification purely from being adopted itself, with nothing ever touching the child's own id directly", matching, root2.taskNotificationsInFlight)
	}
	if matching[0].Status != StatusFailed {
		t.Errorf("notification.Status = %q, want %q", matching[0].Status, StatusFailed)
	}
}

// TestAdoptRootDoesNotRecoverAnAlreadySettledChild is
// TestAdoptRootRecoversCrashedChildNeverTouchedDirectly's negative
// counterpart: a child that finished BEFORE the restart IS still adopted
// by the sweep (unconditionally — see recoverCrashedChildrenLocked's own
// doc comment for why a settled intermediate cannot just be skipped, or a
// crashed GRANDCHILD beneath it would never be discovered), but must NOT
// be misreported as freshly recovered — no spurious second notification,
// and its status/result restored accurately (StatusDone, not the bare
// StatusIdle adoptLocked would otherwise leave uncorrected — see
// SessionManager.restoreKnownStatusLocked).
func TestAdoptRootDoesNotRecoverAnAlreadySettledChild(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", nil)
	childProv := scriptedTurns("child", doneTurn("child done"))
	reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 3, 0)
	root1 := mgr1.NewRoot(rootCfg)
	childID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, mgr1, childID, StatusDone, time.Second)

	// Give finalizeTurn's own deferred settled-marker persist a moment to
	// land durably before this test reloads the same log fresh below.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := LoadSession(Config{Providers: reg, SessionDir: dir}, childID)
		if err != nil {
			t.Fatalf("LoadSession (settle poll): %v", err)
		}
		if !s.hasUnfinalizedTurn() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test setup: child's settled marker never landed durably")
		}
		time.Sleep(5 * time.Millisecond)
	}

	rootProv2 := scriptedTurns("root", nil)
	childProv2 := scriptedTurns("child", nil)
	reg2 := provider.Registry{rootProv2.Name(): rootProv2, childProv2.Name(): childProv2}
	rootCfg2 := Config{Providers: reg2, Model: modelFor("root"), SessionDir: dir}

	mgr2 := NewSessionManager(context.Background(), 3, 0)
	root2, err := LoadSession(rootCfg2, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root: %v", err)
	}
	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}

	childInfo, tracked := mgr2.Info(childID)
	if !tracked {
		t.Fatal("child not tracked after AdoptRoot — recoverCrashedChildrenLocked must still adopt an already-settled child, to reach any crashed descendant beneath it")
	}
	if childInfo.Status != StatusDone || childInfo.Result != "child done" {
		t.Errorf("child info = %+v, want StatusDone with the real result restored (restoreKnownStatusLocked), not the bare StatusIdle adoptLocked leaves by default", childInfo)
	}
	// root2.taskNotifications legitimately has ONE entry here already —
	// the child's own real, normal StatusDone completion, durably queued
	// by finalizeTurn during mgr1's run and never checked out (root1
	// never ran a turn of its own to consume it) — restored by
	// LoadSession's ordinary queued-minus-delivered fold, nothing to do
	// with recoverCrashedChildrenLocked. The assertion here is that
	// there is EXACTLY that one, real notification — not a second,
	// spurious StatusFailed one the sweep would add if it wrongly
	// treated this already-settled child as crashed.
	root2.mu.Lock()
	defer root2.mu.Unlock()
	if len(root2.taskNotifications) != 1 {
		t.Fatalf("root2.taskNotifications = %+v, want exactly 1 (the child's own real, already-queued StatusDone completion) — a second entry would mean the sweep spuriously re-recovered an already-settled child", root2.taskNotifications)
	}
	if got := root2.taskNotifications[0]; got.Status != StatusDone || got.ChildID != childID {
		t.Errorf("root2.taskNotifications[0] = %+v, want the child's real StatusDone completion, unmodified", got)
	}
}

// TestAdoptRootRecoversCrashedGrandchildTwoLevelsDeep proves
// recoverCrashedChildrenLocked's own recursion claim: adopting a ROOT
// recovers not just its OWN crashed children, but a crashed GRANDCHILD
// too — mid (root's own child) settled normally and is STILL adopted by
// the sweep (unconditionally, so a crashed descendant more than one
// level down can ever be reached at all — see that method's own doc
// comment), and THAT adoption runs the exact same sweep again for mid's
// own children, discovering and recovering the crashed grandchild. A
// single ancestor touch (AdoptRoot on the root, nothing else) converges
// the whole crashed subtree.
//
// The grandchild's own notification lands on the ROOT, not mid: mid is
// terminal (StatusDone) by the time nearestLiveAncestorLocked walks the
// grandchild's ancestor chain, so it is correctly skipped in favor of the
// nearest LIVE ancestor — the same "reparent past a terminal node" rule
// TestGrandchildReparentsToNearestLiveAncestor covers for the live,
// no-restart case.
func TestAdoptRootRecoversCrashedGrandchildTwoLevelsDeep(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", nil)
	midProv := scriptedTurns("mid", doneTurn("mid done"))
	grandProv := &signaledBlockingProvider{name: "grand", started: make(chan struct{}), release: make(chan struct{})}
	reg := provider.Registry{rootProv.Name(): rootProv, midProv.Name(): midProv, grandProv.Name(): grandProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 3, 0)
	root1 := mgr1.NewRoot(rootCfg)

	midID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("mid"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn mid: %v", err)
	}
	waitForStatus(t, mgr1, midID, StatusDone, time.Second)

	grandID, err := mgr1.Spawn(SpawnOptions{ParentID: midID, Prompt: "go deeper", Model: modelFor("grand"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn grandchild: %v", err)
	}
	<-grandProv.started // grandchild now genuinely mid-turn, blocked

	// Give mid's own settled marker a moment to land durably before this
	// test reloads its log fresh below (same discipline as every other
	// "poll before reload" test in this file).
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := LoadSession(Config{Providers: reg, SessionDir: dir}, midID)
		if err != nil {
			t.Fatalf("LoadSession (settle poll): %v", err)
		}
		if !s.hasUnfinalizedTurn() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test setup: mid's settled marker never landed durably")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Fresh process: root2 gets its OWN provider instances — same
	// shared-object race avoidance as every other test in this file.
	rootProv2 := scriptedTurns("root", nil)
	midProv2 := scriptedTurns("mid", nil)
	grandProv2 := scriptedTurns("grand", nil)
	reg2 := provider.Registry{rootProv2.Name(): rootProv2, midProv2.Name(): midProv2, grandProv2.Name(): grandProv2}
	rootCfg2 := Config{Providers: reg2, Model: modelFor("root"), SessionDir: dir}

	mgr2 := NewSessionManager(context.Background(), 3, 0)
	root2, err := LoadSession(rootCfg2, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root: %v", err)
	}

	// The ONLY call this test makes against mgr2 — no AdoptReloaded, no
	// Send, nothing ever names midID or grandID directly.
	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}

	midInfo, ok := mgr2.Info(midID)
	if !ok {
		t.Fatal("mid not tracked after AdoptRoot")
	}
	if midInfo.Status != StatusDone {
		t.Errorf("mid.Status = %q, want %q — mid itself settled normally and must not be misreported", midInfo.Status, StatusDone)
	}
	grandInfo, ok := mgr2.Info(grandID)
	if !ok {
		t.Fatal("grandchild not tracked after AdoptRoot — the recursive sweep (mid's own recoverCrashedChildrenLocked) did not discover it")
	}
	if grandInfo.Status != StatusFailed || !strings.Contains(grandInfo.FailReason, "restart") {
		t.Errorf("grandchild info = %+v, want StatusFailed with a restart-loss fail_reason", grandInfo)
	}

	// root2.taskNotifications ends up with TWO legitimate entries: mid's
	// own real StatusDone completion (durably queued by finalizeTurn
	// during mgr1's run and never checked out, restored by LoadSession's
	// ordinary queued-minus-delivered fold — same as
	// TestAdoptRootDoesNotRecoverAnAlreadySettledChild's own identical
	// case), AND the grandchild's forwarded lost-to-restart notification
	// — mid is TERMINAL (StatusDone), so nearestLiveAncestorLocked walks
	// past it, reparenting the grandchild's notification to the ROOT.
	// The root IS genuinely idle when both land, so this can trigger a
	// real active resume racing this test's own read — poll rather than
	// read once, same pattern as every other test in this file
	// exercising this exact race.
	countAll := func() int {
		root2.mu.Lock()
		defer root2.mu.Unlock()
		return len(root2.taskNotifications) + len(root2.taskNotificationsInFlight)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && countAll() != 2 {
		time.Sleep(5 * time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
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
		t.Fatalf("root2.taskNotifications = %+v (in-flight: %+v), want exactly 2 (mid's own real completion, plus the grandchild's forwarded lost-to-restart notification)", root2.taskNotifications, root2.taskNotificationsInFlight)
	}
	var sawMidDone, sawGrandFailed bool
	for _, n := range root2.taskNotifications {
		switch {
		case n.ChildID == midID && n.Status == StatusDone:
			sawMidDone = true
		case n.ChildID == grandID && n.Status == StatusFailed:
			sawGrandFailed = true
		}
	}
	if !sawMidDone {
		t.Errorf("root2.taskNotifications missing mid's own real StatusDone completion: %+v", root2.taskNotifications)
	}
	if !sawGrandFailed {
		t.Errorf("root2.taskNotifications missing the grandchild's forwarded lost-to-restart notification, reparented past its own terminal parent (mid): %+v", root2.taskNotifications)
	}
}

// TestAdoptRootReparentsGrandchildPastSettledIntermediateWithoutCommittedOutcome
// is the regression test for a live review finding on
// restoreKnownStatusLocked: a node whose own turn settled cleanly but
// which has NO committedOutcome recorded (a session that predates the
// whole committedOutcome mechanism, or one whose commit record was
// otherwise never durably written) used to fall through
// restoreKnownStatusLocked's own guard and get left at adoptLocked's bare
// StatusIdle default, un-finalized — even though it definitely already
// ran at least one full turn, proven by its own SpawnedChildIDs being
// non-empty (Spawn is only ever callable from WITHIN a turn, so a node
// that spawned anything cannot be a genuinely fresh, never-run node).
// nearestLiveAncestorLocked's own walk treats StatusIdle as "still live"
// (its only terminal cases are Done/Failed/Canceled), so a crashed
// GRANDCHILD recovered underneath this exact node was delivered directly
// onto it instead of reparented past it to the next real live ancestor —
// and because it looked idle, delivery also fired an async resume
// (fireIdleResumeAsync) that would have spuriously re-run a real turn on
// a node that had already finished, purely as a side effect of relaying
// a notification onward.
//
// Simulates the "predates the mechanism" case directly, rather than
// hand-building a session from scratch: mid completes one genuine turn
// (spawning a grandchild left crash-blocked) through the ordinary live
// path, exactly like TestAdoptRootRecoversCrashedGrandchildTwoLevelsDeep,
// producing a real recTaskOutcomeCommitted record — then this test
// strips exactly that one record type from mid's own on-disk log before
// the second process reloads it, leaving every OTHER record (including
// recTaskSpawned for grandID, and the settled marker) intact, precisely
// the shape an old, pre-committedOutcome log already has.
func TestAdoptRootReparentsGrandchildPastSettledIntermediateWithoutCommittedOutcome(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", nil)
	midProv := scriptedTurns("mid", doneTurn("mid done"))
	grandProv := &signaledBlockingProvider{name: "grand", started: make(chan struct{}), release: make(chan struct{})}
	reg := provider.Registry{rootProv.Name(): rootProv, midProv.Name(): midProv, grandProv.Name(): grandProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 3, 0)
	root1 := mgr1.NewRoot(rootCfg)

	midID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("mid"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn mid: %v", err)
	}
	waitForStatus(t, mgr1, midID, StatusDone, time.Second)

	grandID, err := mgr1.Spawn(SpawnOptions{ParentID: midID, Prompt: "go deeper", Model: modelFor("grand"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn grandchild: %v", err)
	}
	<-grandProv.started // grandchild now genuinely mid-turn, blocked

	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := LoadSession(Config{Providers: reg, SessionDir: dir}, midID)
		if err != nil {
			t.Fatalf("LoadSession (settle poll): %v", err)
		}
		if !s.hasUnfinalizedTurn() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test setup: mid's settled marker never landed durably")
		}
		time.Sleep(5 * time.Millisecond)
	}

	stripRecordType(t, sessionPath(dir, midID), recTaskOutcomeCommitted)

	// Fresh process, fresh providers — same shared-object race avoidance
	// as every other test in this file. midProv2 is deliberately given NO
	// scripted turns: the fix must never need to actually run one against
	// mid to behave correctly.
	rootProv2 := scriptedTurns("root", nil)
	midProv2 := scriptedTurns("mid", nil)
	grandProv2 := scriptedTurns("grand", nil)
	reg2 := provider.Registry{rootProv2.Name(): rootProv2, midProv2.Name(): midProv2, grandProv2.Name(): grandProv2}
	rootCfg2 := Config{Providers: reg2, Model: modelFor("root"), SessionDir: dir}

	mgr2 := NewSessionManager(context.Background(), 3, 0)
	root2, err := LoadSession(rootCfg2, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root: %v", err)
	}

	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}

	// Checked immediately, synchronously, right after AdoptRoot returns —
	// before any async fireIdleResumeAsync goroutine the pre-fix bug
	// would have fired could possibly run: mid must already be terminal,
	// never left looking idle/live just because its committed-outcome
	// record is missing.
	midInfo, ok := mgr2.Info(midID)
	if !ok {
		t.Fatal("mid not tracked after AdoptRoot")
	}
	if midInfo.Status == StatusIdle || midInfo.Status == StatusRunning {
		t.Fatalf("mid.Status = %q immediately after AdoptRoot, want a terminal status — a node proven (via its own SpawnedChildIDs) to have already run a turn must never be left looking live just because its committed outcome record is missing", midInfo.Status)
	}

	grandInfo, ok := mgr2.Info(grandID)
	if !ok {
		t.Fatal("grandchild not tracked after AdoptRoot — the recursive sweep did not discover it")
	}
	if grandInfo.Status != StatusFailed || !strings.Contains(grandInfo.FailReason, "restart") {
		t.Errorf("grandchild info = %+v, want StatusFailed with a restart-loss fail_reason", grandInfo)
	}

	// The grandchild's forwarded notification must land on ROOT — mid is
	// not a valid live delivery target here, missing committed outcome or
	// not.
	countAll := func() int {
		root2.mu.Lock()
		defer root2.mu.Unlock()
		return len(root2.taskNotifications) + len(root2.taskNotificationsInFlight)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && countAll() != 2 {
		time.Sleep(5 * time.Millisecond)
	}
	// Root is genuinely idle when both notifications land, which can
	// trigger a real active resume racing this test's own read — a
	// SECOND poll, on len(root2.taskNotifications) specifically (not
	// countAll, which stays 2 even once resume checks them out into
	// taskNotificationsInFlight), same pattern as every other test in
	// this file exercising this exact race.
	deadline = time.Now().Add(5 * time.Second)
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
		t.Fatalf("root2.taskNotifications = %+v (in-flight: %+v), want exactly 2 (mid's own real completion, plus the grandchild's forwarded lost-to-restart notification)", root2.taskNotifications, root2.taskNotificationsInFlight)
	}
	var sawGrandFailed bool
	for _, n := range root2.taskNotifications {
		if n.ChildID == grandID && n.Status == StatusFailed {
			sawGrandFailed = true
		}
	}
	if !sawGrandFailed {
		t.Errorf("root2.taskNotifications missing the grandchild's forwarded lost-to-restart notification, reparented past mid: %+v", root2.taskNotifications)
	}
}

// stripRecordType rewrites the session log at path, removing every line
// whose "type" field equals recType — used to simulate an old log
// written before some record type existed, without hand-constructing an
// entire log from scratch.
func stripRecordType(t *testing.T, path, recType string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stripRecordType: read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	kept := lines[:0]
	for _, line := range lines {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("stripRecordType: unmarshal record: %v", err)
		}
		if probe.Type != recType {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("stripRecordType: write %s: %v", path, err)
	}
}

// TestRecoverCrashedChildrenLockedSurvivesConcurrentReapOfJustAdoptedIntermediate
// is the trace-verified answer to a live review finding: restoreKnownStatusLocked
// now marks an already-settled, just-adopted intermediate node BOTH
// finalized AND terminal — which, unlike before this whole mechanism
// existed, makes it immediately Reap-eligible (Reap's own guard: only
// !finalized skips a node; finalized+terminal+childless does not) the
// instant it is adopted, before its OWN children (a crashed grandchild,
// say) have finished being recursively discovered and integrated by THIS
// SAME adoption's own trailing recoverCrashedChildrenLocked call.
//
// The race is real: recoverCrashedChildrenLocked's own 3-phase
// unlock/relock restructure (see its own doc comment) releases m.mu for
// disk I/O TWICE in a two-level crash — root's own sweep discovering mid
// first, then mid's own NESTED sweep (triggered from inside
// adoptReloadedLocked, mid's own adoption, right after
// restoreKnownStatusLocked marks it finalized+terminal) discovering
// grandchild. mid is registered into m.nodes and marked finalized+
// terminal BEFORE that second, nested sweep's own Step 2 unlock — so a
// concurrent Reap() call landing in that exact window legitimately
// collects mid (finalized, terminal, and still childless: grandchild is
// not yet attached as one of mid's children — that only happens once
// grandchild's own adopt runs, which is exactly what this window is
// waiting on).
//
// But it is not a correctness gap: recoverCrashedChildrenLocked's own
// existing revalidation-on-reacquire (`if m.nodes[nID] != n { return }`)
// already anticipates precisely this shape of Reap ("the one shape that
// is possible for an already-terminal, already-finalized node a
// concurrent Reap() call could legitimately collect while unlocked" —
// see that method's own doc comment) and abandons the whole nested
// integration cleanly, rather than attaching a recovered grandchild
// under a node that has already left the tree. Nothing is corrupted or
// silently dropped forever: mid's id stays in root's own
// SpawnedChildIDs — an append-only durable audit trail Reap never
// touches — so a LATER restart-driven re-adoption of root (a fresh
// SessionManager loading the same on-disk session, exactly like every
// OTHER "next touch" this whole feature already relies on — see
// recoverCrashedChildrenLocked's own doc comment's "purely reactive"
// discussion) rediscovers mid, reloads it fresh, and fully recovers the
// subtree this attempt abandoned.
//
// This test proves both halves: the FIRST AdoptRoot call safely
// abandons (grandchild is never adopted on this attempt, no panic, no
// corruption of root's own tree), and a SECOND process's AdoptRoot call
// against the SAME on-disk session fully recovers both mid and
// grandchild.
func TestRecoverCrashedChildrenLockedSurvivesConcurrentReapOfJustAdoptedIntermediate(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", nil)
	midProv := scriptedTurns("mid", doneTurn("mid done"))
	grandProv := &signaledBlockingProvider{name: "grand", started: make(chan struct{}), release: make(chan struct{})}
	reg := provider.Registry{rootProv.Name(): rootProv, midProv.Name(): midProv, grandProv.Name(): grandProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 3, 0)
	root1 := mgr1.NewRoot(rootCfg)

	midID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("mid"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn mid: %v", err)
	}
	waitForStatus(t, mgr1, midID, StatusDone, time.Second)

	grandID, err := mgr1.Spawn(SpawnOptions{ParentID: midID, Prompt: "go deeper", Model: modelFor("grand"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn grandchild: %v", err)
	}
	<-grandProv.started // grandchild now genuinely mid-turn, blocked

	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := LoadSession(Config{Providers: reg, SessionDir: dir}, midID)
		if err != nil {
			t.Fatalf("LoadSession (settle poll): %v", err)
		}
		if !s.hasUnfinalizedTurn() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test setup: mid's settled marker never landed durably")
		}
		time.Sleep(5 * time.Millisecond)
	}

	rootProv2 := scriptedTurns("root", nil)
	midProv2 := scriptedTurns("mid", nil)
	grandProv2 := scriptedTurns("grand", nil)
	reg2 := provider.Registry{rootProv2.Name(): rootProv2, midProv2.Name(): midProv2, grandProv2.Name(): grandProv2}
	rootCfg2 := Config{Providers: reg2, Model: modelFor("root"), SessionDir: dir}

	mgr2 := NewSessionManager(context.Background(), 3, 0)
	root2, err := LoadSession(rootCfg2, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root: %v", err)
	}

	// Fires on EVERY recoverCrashedChildrenLocked unlock window — first
	// root's own (discovering mid), then mid's own NESTED sweep
	// (discovering grandchild). Only the second firing is where mid is
	// both tracked and already finalized+terminal; reap it exactly
	// there, deterministically, rather than relying on incidental
	// goroutine-scheduling luck for a real concurrent Reap() call to
	// land in the same window.
	var hookCalls int
	var reapedN int
	mgr2.testSweepUnlockedHook = func() {
		hookCalls++
		if hookCalls != 2 {
			return
		}
		mgr2.mu.Lock()
		midNode, tracked := mgr2.nodes[midID]
		mgr2.mu.Unlock()
		if !tracked {
			t.Errorf("test setup: mid not yet tracked at the second unlock window")
			return
		}
		if !midNode.finalized || midNode.status != StatusDone {
			t.Errorf("test setup: mid not yet finalized+terminal at the second unlock window: finalized=%v status=%v", midNode.finalized, midNode.status)
			return
		}
		reapedN = mgr2.Reap()
	}

	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}
	if hookCalls < 2 {
		t.Fatalf("test setup: testSweepUnlockedHook only fired %d time(s), want 2 (root's own sweep, then mid's own nested sweep)", hookCalls)
	}
	if reapedN != 1 {
		t.Fatalf("test setup: concurrent Reap() collected %d node(s), want exactly 1 (mid)", reapedN)
	}

	// First attempt: safely abandoned. mid is gone (reaped out from
	// under the nested sweep integrating it), grandchild was never
	// adopted, and nothing panicked or corrupted root's own tree.
	if _, tracked := mgr2.Info(midID); tracked {
		t.Error("mid still tracked after being concurrently reaped — the nested sweep's own revalidation should have found it gone, not re-added it")
	}
	if _, tracked := mgr2.Info(grandID); tracked {
		t.Error("grandchild WAS adopted despite mid having been reaped out from under the nested sweep that was integrating it — should have abandoned instead")
	}

	// Second attempt: a genuinely LATER restart-driven re-adoption of
	// root (a fresh SessionManager, a fresh load of the same on-disk
	// session) rediscovers mid via root's own durable SpawnedChildIDs
	// and fully recovers the whole subtree the first attempt abandoned —
	// the exact reactive, self-healing guarantee the whole feature
	// already rests on.
	rootProv3 := scriptedTurns("root", nil)
	midProv3 := scriptedTurns("mid", nil)
	grandProv3 := scriptedTurns("grand", nil)
	reg3 := provider.Registry{rootProv3.Name(): rootProv3, midProv3.Name(): midProv3, grandProv3.Name(): grandProv3}
	rootCfg3 := Config{Providers: reg3, Model: modelFor("root"), SessionDir: dir}

	mgr3 := NewSessionManager(context.Background(), 3, 0)
	root3, err := LoadSession(rootCfg3, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root (second attempt): %v", err)
	}
	if err := mgr3.AdoptRoot(root3); err != nil {
		t.Fatalf("AdoptRoot (second attempt): %v", err)
	}
	midInfo, ok := mgr3.Info(midID)
	if !ok || midInfo.Status != StatusDone {
		t.Fatalf("mid not recovered on second attempt: info=%+v ok=%v", midInfo, ok)
	}
	grandInfo, ok := mgr3.Info(grandID)
	if !ok || grandInfo.Status != StatusFailed || !strings.Contains(grandInfo.FailReason, "restart") {
		t.Fatalf("grandchild not recovered on second attempt: info=%+v ok=%v", grandInfo, ok)
	}
}

// TestRecoverCrashedChildrenLockedRevalidatesNPerLoopIterationNotJustOnce
// is the regression test for a live review finding on
// TestRecoverCrashedChildrenLockedSurvivesConcurrentReapOfJustAdoptedIntermediate's
// own fix: that earlier fix checked "is n (the node this call is
// integrating candidates FOR) still live" once, before the candidates
// loop — but adoptReloadedLocked's own recursion (called once PER
// candidate, for whichever child was just adopted) can release and
// reacquire m.mu AGAIN, mid-loop, for THAT candidate's own nested sweep.
// A check only before the loop covers candidate #1 but misses n going
// stale during candidate #1's own nested call, before candidate #2 is
// ever reached — silently adopting candidate #2 under a node that has
// already left the tree.
//
// Requires FOUR levels to actually force this shape: root -> mid ->
// {grandA -> greatgrand, grandB}. mid has TWO not-yet-tracked candidates
// (grandA, grandB) when its own sweep runs, so its own Step 3 loop has
// two iterations. grandA is adopted (attached to mid) BEFORE grandA's
// OWN nested sweep runs for greatgrand — so mid already has ONE child
// (grandA) and is not yet Reap-eligible itself. grandA's own nested
// sweep is what actually unlocks m.mu again (to load greatgrand) — and
// at THAT exact moment, grandA itself is finalized+terminal+childless
// (greatgrand not attached yet), so it is legitimately Reap-eligible.
// Reaping grandA there removes it from mid.children too (Reap's own
// parent-cleanup step), which makes mid ITSELF newly childless and so
// ALSO legitimately Reap-eligible for a second Reap() call in the same
// window. By the time mid's own loop reaches candidate #2 (grandB), mid
// has been concurrently reaped out from under it.
func TestRecoverCrashedChildrenLockedRevalidatesNPerLoopIterationNotJustOnce(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", nil)
	midProv := scriptedTurns("mid", doneTurn("mid done"))
	grandAProv := scriptedTurns("grandA", doneTurn("grandA done"))
	grandBProv := scriptedTurns("grandB", doneTurn("grandB done"))
	greatgrandProv := &signaledBlockingProvider{name: "greatgrand", started: make(chan struct{}), release: make(chan struct{})}
	reg := provider.Registry{
		rootProv.Name(): rootProv, midProv.Name(): midProv,
		grandAProv.Name(): grandAProv, grandBProv.Name(): grandBProv,
		greatgrandProv.Name(): greatgrandProv,
	}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 5, 0)
	root1 := mgr1.NewRoot(rootCfg)

	midID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("mid"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn mid: %v", err)
	}
	waitForStatus(t, mgr1, midID, StatusDone, time.Second)

	grandAID, err := mgr1.Spawn(SpawnOptions{ParentID: midID, Prompt: "go A", Model: modelFor("grandA"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn grandA: %v", err)
	}
	waitForStatus(t, mgr1, grandAID, StatusDone, time.Second)

	greatgrandID, err := mgr1.Spawn(SpawnOptions{ParentID: grandAID, Prompt: "go deeper", Model: modelFor("greatgrand"), AgentType: AgentExplore})
	if err != nil {
		t.Fatalf("Spawn greatgrand: %v", err)
	}
	<-greatgrandProv.started // greatgrand now genuinely mid-turn, blocked

	grandBID, err := mgr1.Spawn(SpawnOptions{ParentID: midID, Prompt: "go B", Model: modelFor("grandB"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn grandB: %v", err)
	}
	waitForStatus(t, mgr1, grandBID, StatusDone, time.Second)

	// Settle-poll every non-blocked node before reloading fresh below —
	// same discipline as every other test in this file exercising a
	// restart.
	for _, id := range []string{midID, grandAID, grandBID} {
		deadline := time.Now().Add(2 * time.Second)
		for {
			s, err := LoadSession(Config{Providers: reg, SessionDir: dir}, id)
			if err != nil {
				t.Fatalf("LoadSession (settle poll %s): %v", id, err)
			}
			if !s.hasUnfinalizedTurn() {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("test setup: %s's settled marker never landed durably", id)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	rootProv2 := scriptedTurns("root", nil)
	midProv2 := scriptedTurns("mid", nil)
	grandAProv2 := scriptedTurns("grandA", nil)
	grandBProv2 := scriptedTurns("grandB", nil)
	greatgrandProv2 := scriptedTurns("greatgrand", nil)
	reg2 := provider.Registry{
		rootProv2.Name(): rootProv2, midProv2.Name(): midProv2,
		grandAProv2.Name(): grandAProv2, grandBProv2.Name(): grandBProv2,
		greatgrandProv2.Name(): greatgrandProv2,
	}
	rootCfg2 := Config{Providers: reg2, Model: modelFor("root"), SessionDir: dir}

	mgr2 := NewSessionManager(context.Background(), 5, 0)
	root2, err := LoadSession(rootCfg2, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root: %v", err)
	}

	// Fires on every recoverCrashedChildrenLocked unlock window: (1)
	// root's own sweep (discovering mid), (2) mid's own nested sweep
	// (discovering grandA AND grandB together, in ONE Step 2 batch), (3)
	// grandA's own nested sweep (discovering greatgrand) — this third
	// firing is the exact window mid already has grandA attached as its
	// only child, and grandA itself is finalized+terminal+childless.
	var hookCalls int
	var reapedGrandA, reapedMid int
	mgr2.testSweepUnlockedHook = func() {
		hookCalls++
		if hookCalls != 3 {
			return
		}
		mgr2.mu.Lock()
		grandANode, grandATracked := mgr2.nodes[grandAID]
		midNode, midTracked := mgr2.nodes[midID]
		mgr2.mu.Unlock()
		if !grandATracked || !grandANode.finalized || grandANode.status != StatusDone || len(grandANode.children) != 0 {
			t.Errorf("test setup: grandA not yet finalized+terminal+childless at the third unlock window: tracked=%v node=%+v", grandATracked, grandANode)
			return
		}
		if !midTracked || len(midNode.children) != 1 {
			t.Errorf("test setup: mid does not yet have exactly grandA as its only child at the third unlock window: tracked=%v node=%+v", midTracked, midNode)
			return
		}
		reapedGrandA = mgr2.Reap() // removes grandA — also drops it from mid.children
		reapedMid = mgr2.Reap()    // mid is now childless too — removes mid
	}

	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}
	if hookCalls < 3 {
		t.Fatalf("test setup: testSweepUnlockedHook only fired %d time(s), want at least 3", hookCalls)
	}
	if reapedGrandA != 1 {
		t.Fatalf("test setup: first concurrent Reap() collected %d node(s), want exactly 1 (grandA)", reapedGrandA)
	}
	if reapedMid != 1 {
		t.Fatalf("test setup: second concurrent Reap() collected %d node(s), want exactly 1 (mid) — mid should have gone childless the instant grandA was reaped", reapedMid)
	}

	// First attempt: safely abandoned. mid and grandA are gone (reaped),
	// greatgrand was never adopted (grandA was reaped out from under its
	// own nested sweep), and — the crux of THIS fix specifically — grandB
	// must ALSO never have been adopted: mid's own loop must have
	// re-checked mid's own liveness before processing grandB (its SECOND
	// candidate) and abandoned rather than attaching grandB under a node
	// that had already left the tree.
	if _, tracked := mgr2.Info(midID); tracked {
		t.Error("mid still tracked after being concurrently reaped")
	}
	if _, tracked := mgr2.Info(grandAID); tracked {
		t.Error("grandA still tracked after being concurrently reaped")
	}
	if _, tracked := mgr2.Info(greatgrandID); tracked {
		t.Error("greatgrand WAS adopted despite grandA having been reaped out from under the nested sweep integrating it")
	}
	if _, tracked := mgr2.Info(grandBID); tracked {
		t.Error("grandB WAS adopted despite mid having been reaped out from under the loop integrating it — the per-iteration revalidation should have caught this and abandoned before processing grandB")
	}

	// Second attempt: a genuinely LATER restart-driven re-adoption of
	// root fully recovers the whole subtree the first attempt abandoned.
	rootProv3 := scriptedTurns("root", nil)
	midProv3 := scriptedTurns("mid", nil)
	grandAProv3 := scriptedTurns("grandA", nil)
	grandBProv3 := scriptedTurns("grandB", nil)
	greatgrandProv3 := scriptedTurns("greatgrand", nil)
	reg3 := provider.Registry{
		rootProv3.Name(): rootProv3, midProv3.Name(): midProv3,
		grandAProv3.Name(): grandAProv3, grandBProv3.Name(): grandBProv3,
		greatgrandProv3.Name(): greatgrandProv3,
	}
	rootCfg3 := Config{Providers: reg3, Model: modelFor("root"), SessionDir: dir}

	mgr3 := NewSessionManager(context.Background(), 5, 0)
	root3, err := LoadSession(rootCfg3, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root (second attempt): %v", err)
	}
	if err := mgr3.AdoptRoot(root3); err != nil {
		t.Fatalf("AdoptRoot (second attempt): %v", err)
	}
	for id, wantStatus := range map[string]SessionStatus{
		midID: StatusDone, grandAID: StatusDone, grandBID: StatusDone,
	} {
		info, ok := mgr3.Info(id)
		if !ok || info.Status != wantStatus {
			t.Errorf("%s not recovered on second attempt: info=%+v ok=%v, want status %q", id, info, ok, wantStatus)
		}
	}
	greatgrandInfo, ok := mgr3.Info(greatgrandID)
	if !ok || greatgrandInfo.Status != StatusFailed || !strings.Contains(greatgrandInfo.FailReason, "restart") {
		t.Fatalf("greatgrand not recovered on second attempt: info=%+v ok=%v", greatgrandInfo, ok)
	}
}

// TestRecoverCrashedChildrenLockedSkipsConcurrentlyAdoptedChild is the
// regression test for a live review finding: recoverCrashedChildrenLocked
// used to run its own disk-bound LoadSession replay while m.mu — the
// single lock guarding every session in the tree — was held, the same
// class of problem deferPersist/unlockAndFlushPersist already closed for
// durable WRITES on this same set of call paths. The fix releases m.mu
// for the replay and re-acquires before integrating any result — which
// means the tree is genuinely NOT frozen for the sweep's own duration,
// and a concurrent adoption of the SAME child (another ancestor's own
// sweep sharing it, or an explicit AdoptReloaded racing this one) can
// land in that exact gap.
//
// Uses SessionManager.testSweepUnlockedHook (test-only, nil in
// production) to inject a synchronous "concurrent" AdoptReloaded call —
// on a SEPARATE load of the SAME crashed child — into the precise window
// recoverCrashedChildrenLocked has released m.mu, deterministically
// rather than relying on incidental goroutine-scheduling luck. Proves:
// the child ends up tracked and correctly recovered EXACTLY once (the
// concurrent path's own adoption wins; the sweep's own revalidation-on-
// reacquire finds it already tracked and skips its own redundant
// adopt), with exactly one notification delivered to the root — never a
// double-registration or a duplicate notification.
func TestRecoverCrashedChildrenLockedSkipsConcurrentlyAdoptedChild(t *testing.T) {
	dir := t.TempDir()
	rootProv := scriptedTurns("root", nil)
	childProv := &signaledBlockingProvider{name: "child", started: make(chan struct{}), release: make(chan struct{})}
	reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir}

	mgr1 := NewSessionManager(context.Background(), 3, 0)
	root1 := mgr1.NewRoot(rootCfg)
	childID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-childProv.started // child now genuinely mid-turn, blocked — simulate "kill -9 1" by simply abandoning it

	// Fresh process: root2 gets its OWN provider instances — same
	// shared-object race avoidance as every other test in this file.
	rootProv2 := scriptedTurns("root", nil)
	childProv2 := scriptedTurns("child", nil)
	reg2 := provider.Registry{rootProv2.Name(): rootProv2, childProv2.Name(): childProv2}
	rootCfg2 := Config{Providers: reg2, Model: modelFor("root"), SessionDir: dir}

	mgr2 := NewSessionManager(context.Background(), 3, 0)
	root2, err := LoadSession(rootCfg2, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root: %v", err)
	}

	// The injected "concurrent" adoption: a SEPARATE LoadSession of the
	// SAME crashed child, adopted directly, synchronously, from inside
	// the hook — root2 is already tracked by this point (adoptRootLocked
	// registers it before ever calling recoverCrashedChildrenLocked), so
	// nearestLiveAncestorLocked can already find it as a real delivery
	// target, exactly as it would for any other concurrent adopter.
	var concurrentErr error
	var hookRan bool
	var winnerNode *sessionNode
	mgr2.testSweepUnlockedHook = func() {
		hookRan = true
		mgr2.testSweepUnlockedHook = nil // fire exactly once
		concurrentChild, err := LoadSession(rootCfg2, childID)
		if err != nil {
			concurrentErr = err
			return
		}
		concurrentErr = mgr2.AdoptReloaded(concurrentChild)
		// Captured under m.mu (AdoptReloaded's own critical section has
		// already released it by the time this line runs, but nothing
		// else can be touching m.nodes[childID] between that release and
		// this read — the sweep itself is still mid-unlock, and this
		// hook is its only caller) — the node object the CONCURRENT path
		// actually installed, to compare against whatever is there once
		// the sweep's own (correctly skipped, or incorrectly clobbering)
		// integration finishes below.
		mgr2.mu.Lock()
		winnerNode = mgr2.nodes[childID]
		mgr2.mu.Unlock()
	}

	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}
	if !hookRan {
		t.Fatal("test setup: testSweepUnlockedHook never fired — recoverCrashedChildrenLocked did not release m.mu for its own replay")
	}
	if concurrentErr != nil {
		t.Fatalf("concurrent AdoptReloaded: %v", concurrentErr)
	}
	if winnerNode == nil {
		t.Fatal("test setup: concurrent AdoptReloaded did not register a node for childID")
	}

	// The crux of the fix: the sweep's OWN revalidation-on-reacquire must
	// find childID already tracked and skip its own redundant adopt —
	// proven by NODE IDENTITY, not just observable status (which a
	// naive double-adopt would ALSO end up reporting correctly, via the
	// very durability/idempotency this whole mechanism already provides
	// — see recoverCrashedChildrenLocked's own doc comment on the race
	// semantics). A double-adopt here is still real harm even when
	// status/notifications end up looking fine: adoptLocked builds a
	// FRESH context.WithCancel(parentCtx) and unconditionally overwrites
	// m.nodes[childID] with the new node, silently discarding the
	// winner's own node — including its own cancel func, whose
	// registration in parentCtx's internal children map would then leak
	// for the parent's whole remaining lifetime (see Reap's own doc
	// comment on this exact leak class) — without this test's own
	// pointer-identity check, that leak would pass completely silently.
	mgr2.mu.Lock()
	gotNode := mgr2.nodes[childID]
	mgr2.mu.Unlock()
	if gotNode != winnerNode {
		t.Fatalf("mgr2.nodes[childID] = %p after AdoptRoot returned, want the SAME node the concurrent AdoptReloaded call (%p) installed — the sweep's own revalidation-on-reacquire failed to skip its own redundant, clobbering adopt", gotNode, winnerNode)
	}

	// Exactly one adoption won the race — the child is tracked exactly
	// once, correctly recovered (via whichever path actually won; either
	// is a correct outcome, see recoverCrashedChildrenLocked's own doc
	// comment on the race semantics), never left half-adopted or
	// double-registered.
	info, ok := mgr2.Info(childID)
	if !ok {
		t.Fatal("child not tracked after the race")
	}
	if info.Status != StatusFailed || !strings.Contains(info.FailReason, "restart") {
		t.Errorf("child info = %+v, want StatusFailed with a restart-loss fail_reason", info)
	}

	// root2 is genuinely idle when the winning adoption delivers to it,
	// so this can trigger a real active resume racing this test's own
	// read — poll rather than read once, same pattern as every other
	// test in this file exercising this exact race.
	countAll := func() int {
		root2.mu.Lock()
		defer root2.mu.Unlock()
		return len(root2.taskNotifications) + len(root2.taskNotificationsInFlight)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && countAll() != 1 {
		time.Sleep(5 * time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		root2.mu.Lock()
		n := len(root2.taskNotifications)
		root2.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	root2.mu.Lock()
	defer root2.mu.Unlock()
	if len(root2.taskNotifications) != 1 {
		t.Fatalf("root2.taskNotifications = %+v (in-flight: %+v), want exactly 1 — a second entry would mean BOTH the concurrent adopter and the sweep's own (stale) result delivered a notification for the same child", root2.taskNotifications, root2.taskNotificationsInFlight)
	}
	if got := root2.taskNotifications[0]; got.ChildID != childID || got.Status != StatusFailed {
		t.Errorf("root2.taskNotifications[0] = %+v, want ChildID=%q Status=%q", got, childID, StatusFailed)
	}
}

// TestRecoverCrashedChildrenLockedInheritsFullConfig is the regression
// test for a live review adjudication: a child recovered by
// recoverCrashedChildrenLocked is NOT provably extract-and-discard —
// unlike a ROOT (where ReportTurnStart's own "always re-attach to the
// live object" migration replaces n.session with the server's own
// fully-configured reload on every turn), SessionManager.Send
// (session.send's own sole scheduler for a child — see
// server/session_tree.go's handleSessionSend, "Child: SessionManager is
// its sole scheduler, always safe") reads n.session and calls Prompt on
// it DIRECTLY, with no reload/re-attach step of any kind. A minimal
// Config{Providers, SessionDir} reload — this method's own earlier
// version — would silently strand a recovered child with no WorkDir, no
// OnEvent (its turn would run invisibly, never reaching the server's own
// SSE journal), no Hooks/MCP/Processes, the moment a caller sent it a
// genuinely ordinary session.send follow-up.
//
// Proves the fix (configSnapshot(), not a hand-picked field subset):
// spawns a child under a root configured with a distinctive WorkDir and
// an OnEvent hook, crashes it mid-turn, recovers it purely via AdoptRoot
// (this file's own established "never touch the crashed child directly"
// technique), then drives a REAL session.send-shaped follow-up turn
// (SessionManager.Send) on the recovered child and checks BOTH that its
// WorkDir is the inherited one (not empty) AND that its OnEvent hook
// actually fires for that turn's own events (not silently nil).
func TestRecoverCrashedChildrenLockedInheritsFullConfig(t *testing.T) {
	dir := t.TempDir()
	const wantWorkDir = "/distinctive/inherited/workdir"
	rootProv := scriptedTurns("root", nil)
	childProv := &signaledBlockingProvider{name: "child", started: make(chan struct{}), release: make(chan struct{})}
	reg := provider.Registry{rootProv.Name(): rootProv, childProv.Name(): childProv}
	rootCfg := Config{Providers: reg, Model: modelFor("root"), SessionDir: dir, WorkDir: wantWorkDir}

	mgr1 := NewSessionManager(context.Background(), 3, 0)
	root1 := mgr1.NewRoot(rootCfg)
	childID, err := mgr1.Spawn(SpawnOptions{ParentID: root1.ID, Prompt: "go", Model: modelFor("child"), AgentType: AgentGeneralPurpose})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-childProv.started // child now genuinely mid-turn, blocked — simulate "kill -9 1" by simply abandoning it

	// Fresh process: root2's own Config carries the SAME WorkDir and a
	// fresh OnEvent hook — exactly what a real server's own mkCfg
	// closure supplies on restart (cmd/harness/main.go's serveCmd).
	rootProv2 := scriptedTurns("root", nil)
	childProv2 := scriptedTurns("child", doneTurn("follow-up done"))
	reg2 := provider.Registry{rootProv2.Name(): rootProv2, childProv2.Name(): childProv2}
	var evMu sync.Mutex
	var events []Event
	rootCfg2 := Config{
		Providers: reg2, Model: modelFor("root"), SessionDir: dir, WorkDir: wantWorkDir,
		OnEvent: func(ev Event) {
			evMu.Lock()
			events = append(events, ev)
			evMu.Unlock()
		},
	}

	mgr2 := NewSessionManager(context.Background(), 3, 0)
	root2, err := LoadSession(rootCfg2, root1.ID)
	if err != nil {
		t.Fatalf("LoadSession root: %v", err)
	}
	if err := mgr2.AdoptRoot(root2); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}

	childSess, ok := mgr2.Session(childID)
	if !ok {
		t.Fatal("child not tracked after AdoptRoot — recoverCrashedChildrenLocked did not adopt it")
	}
	if got := childSess.WorkDir(); got != wantWorkDir {
		t.Errorf("recovered child.WorkDir() = %q, want %q (inherited from the live ancestor via configSnapshot(), not a minimal Config{Providers,SessionDir} reload)", got, wantWorkDir)
	}

	// The actual proof this finding demanded: session.send's own real
	// path (SessionManager.Send) reads n.session and drives Prompt on it
	// DIRECTLY — no reload, no re-attach. If the recovered child's own
	// OnEvent were still nil (a minimal-Config reload's own silent
	// failure mode), this turn would run invisibly: no event ever
	// reaches the server's SSE journal, indistinguishable from a hang to
	// any client watching it.
	if _, err := mgr2.Send(context.Background(), childID, "follow up"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	evMu.Lock()
	n := len(events)
	evMu.Unlock()
	if n == 0 {
		t.Error("recovered child's own follow-up turn emitted zero events — OnEvent was silently nil (not inherited from the live ancestor)")
	}
}
