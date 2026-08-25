package engine

import (
	"sync"
	"testing"
)

// This file holds the waiting scaffolding the crash-recovery tests in
// session_manager_delivery_test.go share. It owns MECHANICS only — hook
// wiring and the two block-then-recheck loop shapes. Every condition, every
// provider script, and every assertion stays in the test that names it, so
// each test still fails for its own mechanism and nothing here can make one
// pass for another test's reason.
//
// Two rules the loops below encode, learned one flake at a time:
//
//  1. A flush signal alone is never proof. testFlushDoneHook fires when ONE
//     unlockAndFlushPersist call finishes ALL of its queued thunks — not
//     necessarily the call the test cares about. So every wait re-checks the
//     real condition after each signal instead of counting signals.
//  2. In-memory state and durable state settle at different instants.
//     unlockAndFlushPersist releases m.mu — making a status change visible to
//     Info() and a forwarded notification visible on the live *Session —
//     strictly BEFORE the deferred thunks that record it durably actually
//     run. A test that reads memory and a test that re-reads the log
//     therefore need opposite loop orders; waitAfterFlush and loadUntil are
//     those two orders, named so a caller picks deliberately.

// flushSignal reports each completed SessionManager persist flush. It
// installs testFlushDoneHook on mgr — see that field's own doc comment in
// session_manager.go.
//
// The channel is buffered generously: these tests drive a handful of
// Spawn/finalizeTurn/Adopt calls, each at most one flush, and a send must
// never block the production goroutine that runs the hook.
type flushSignal struct {
	ch chan struct{}
}

// newFlushSignal arms mgr's flush hook. Install it BEFORE the calls whose
// persists the test waits on, so no flush is missed.
func newFlushSignal(t *testing.T, mgr *SessionManager) *flushSignal {
	t.Helper()
	f := &flushSignal{ch: make(chan struct{}, 64)}
	mgr.testFlushDoneHook = func() { f.ch <- struct{}{} }
	return f
}

// waitAfterFlush blocks until cond reports true, re-checking it after each
// completed flush and never before the first one.
//
// Wait first, then check: this is the order for a condition on IN-MEMORY
// state that the very critical section under test already made visible (a
// node's status plus the notification its finalizeTurn forwarded, both
// applied under one m.mu hold). Checking first would let the loop exit on
// state that is visible but not yet persisted, which is the ambiguity these
// tests exist to remove. The exit condition here is precisely "that call's
// flush has now run".
//
// No timeout wrapper: a genuine hang is the test binary's own timeout to
// catch.
func (f *flushSignal) waitAfterFlush(t *testing.T, cond func() bool) {
	t.Helper()
	for {
		<-f.ch
		if cond() {
			return
		}
	}
}

// waitUntil blocks until cond reports true, checking it BEFORE the first
// flush signal and again after each one.
//
// Check first, then wait: the order for a condition that may already hold
// when the caller reaches it — a resume that already settled, say — where
// blocking for one more flush would wait for a signal nothing is left to
// send. Use waitAfterFlush instead whenever the state under test becomes
// visible strictly before the persist the caller actually needs, because
// this order would exit on that visible-but-unpersisted state.
func (f *flushSignal) waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for !cond() {
		<-f.ch
	}
}

// waitUntilMsg is waitUntil with a failure message for the case where the
// wait never completes. waitUntil blocks forever on its channel, which is
// the right shape for a condition whose absence a reader can diagnose from
// the test name alone. A setup precondition cannot: "the grandchild's
// queued notification never landed on mid's own log" names which of a
// test's several waits hung, and msg carries it into the goroutine dump the
// binary's own timeout prints.
func (f *flushSignal) waitUntilMsg(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	t.Logf("waiting: %s", msg)
	f.waitUntil(t, cond)
}

// loadUntil re-reads id's log from disk with LoadSession until cond accepts
// the reloaded session, waiting for the next flush between attempts.
//
// Check first, then wait: the disk read IS the check, and the write it looks
// for may already have landed. A flush the caller observed earlier proves
// some flush finished, never that THIS session's write was in it — an
// unrelated flush (a root's own idle-notify bookkeeping) satisfies a signal
// just as well. Once the read succeeds, the persist it depends on has
// unconditionally already run: LoadSession's disk read cannot race a write
// from a goroutine that already returned from its flush call.
//
// what names the read in a failure message ("mid after the crash"), so two
// loads in one test stay distinguishable.
func (f *flushSignal) loadUntil(t *testing.T, cfg Config, id, what string, cond func(*Session) bool) *Session {
	t.Helper()
	for {
		sess, err := LoadSession(cfg, id)
		if err != nil {
			t.Fatalf("LoadSession(%s) [%s]: %v", id, what, err)
		}
		if cond(sess) {
			return sess
		}
		<-f.ch
	}
}

// resumeClaims reports every engine-initiated resume run-slot claim by
// target id. It installs testResumeClaimedHook on mgr — see that field's
// own doc comment in session_manager.go for why a claim signal is the only
// unambiguous marker that a resume has STARTED.
//
// A pending-notification count is not a substitute: "delivered, resume
// goroutine not yet scheduled" and "resume already ran to completion" read
// identically from outside.
//
// The hook now fires from triggerResumeLocked, the ONE place a claim
// happens, so it covers every claim path — fireIdleResumeAsync's `go`
// launch, finalizeTurn's turn-tail re-trigger, and finalizeTurn's
// ancestor delivery onto an idle target. Two consequences shape this type:
//
//  1. A claim for a DIFFERENT target is now routine, so claims are recorded
//     and matched by id rather than asserted against the next value on a
//     channel. Waiting on one target must never consume or reject another's.
//  2. The hook body runs under m.mu, so it must never block. Recording is a
//     slice append plus a broadcast wakeup — never a channel send, which
//     would deadlock the manager once a buffer filled.
type resumeClaims struct {
	mu     sync.Mutex
	claims []string
	// sig is a broadcast, the same close-and-replace shape
	// SessionManager.Changed uses: record closes it and installs a fresh
	// one, so every waiter wakes and re-reads. A single buffered token
	// would let a waiter on one id swallow the wakeup for another's claim
	// and then sleep on with it already spent.
	sig chan struct{}
}

// newResumeClaims arms mgr's resume-claim hook. Install it before the call
// that can trigger the resume.
func newResumeClaims(t *testing.T, mgr *SessionManager) *resumeClaims {
	t.Helper()
	c := &resumeClaims{sig: make(chan struct{})}
	mgr.testResumeClaimedHook = c.record
	return c
}

func (c *resumeClaims) record(targetID string) {
	c.mu.Lock()
	c.claims = append(c.claims, targetID)
	close(c.sig)
	c.sig = make(chan struct{})
	c.mu.Unlock()
}

// take consumes one recorded claim for id and reports whether it found one.
// Claims for other targets stay recorded.
func (c *resumeClaims) take(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, got := range c.claims {
		if got == id {
			c.claims = append(c.claims[:i], c.claims[i+1:]...)
			return true
		}
	}
	return false
}

// wake returns the channel that closes on the next recorded claim. Arm it
// before reading claims, never after, so a claim landing between the read
// and the block still wakes the waiter.
func (c *resumeClaims) wake() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sig
}

// wantClaim blocks until a resume has been claimed for want, and consumes
// that claim.
//
// No timeout wrapper: a genuine hang is the test binary's own timeout to
// catch.
func (c *resumeClaims) wantClaim(t *testing.T, want string) {
	t.Helper()
	for {
		wake := c.wake()
		if c.take(want) {
			return
		}
		<-wake
	}
}

// waitSettled blocks until a resume turn for id has been claimed AND has
// run to completion — the claimed-then-idle pair.
//
// Both halves are needed. The claim alone proves the turn started, not that
// it finished. The idle status alone is satisfied by the target never
// having started at all. Together they bracket exactly one resume turn, so
// everything that turn produces — its trigger message, its requeued or
// committed notifications — is settled when this returns.
func (c *resumeClaims) waitSettled(t *testing.T, mgr *SessionManager, id string) {
	t.Helper()
	c.wantClaim(t, id)
	waitForStatus(t, mgr, id, StatusIdle)
}

// TestResumeClaimWakeupsAreBroadcastNotStolen pins the property
// resumeClaims depends on: ONE recorded claim wakes EVERY armed waiter,
// not just the first one to reach the channel.
//
// Claims are matched per target id, so two waiters can be blocked on one
// resumeClaims at the same time, each looking for a different target. A
// wakeup implemented as a single buffered token cannot serve them: the
// first waiter to receive consumes it, finds no claim for ITS id, and
// re-blocks — while the waiter whose claim actually landed sleeps on with
// the wakeup already spent. Before testResumeClaimedHook moved into
// triggerResumeLocked a claim for another target was not even reachable
// from most tests, so this only became a real shape once the hook started
// covering every claim path.
//
// The test arms two waiters, records once, and requires both released.
// Against a buffered-token record only the first receive succeeds, so the
// second check falls through to its default and fails.
func TestResumeClaimWakeupsAreBroadcastNotStolen(t *testing.T) {
	c := &resumeClaims{sig: make(chan struct{})}
	first, second := c.wake(), c.wake()
	c.record("ses_a")
	for i, wake := range []<-chan struct{}{first, second} {
		select {
		case <-wake:
		default:
			t.Errorf("waiter %d was not woken by a recorded claim — the wakeup was stolen by another waiter", i)
		}
	}
	if !c.take("ses_a") {
		t.Error("take(ses_a) = false, want the recorded claim")
	}
	if c.take("ses_b") {
		t.Error("take(ses_b) = true, want false — a claim must only satisfy its own target")
	}
}
