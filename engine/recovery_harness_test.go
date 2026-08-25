package engine

import "testing"

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

// resumeClaims reports each idle-resume run-slot claim by target id. It
// installs testResumeClaimedHook on mgr — see that field's own doc comment
// in session_manager.go for why a claim signal is the only unambiguous
// marker for an ASYNCHRONOUS resume (fireIdleResumeAsync's `go` launch).
//
// A pending-notification count is not a substitute: "delivered, resume
// goroutine not yet scheduled" and "resume already ran to completion" read
// identically from outside. A resume that finalizeTurn triggers
// synchronously (triggerResumeLocked) never fires this hook, so a test
// waiting on that one needs no claim step at all.
type resumeClaims struct {
	ch chan string
}

// newResumeClaims arms mgr's resume-claim hook. Install it before the call
// that can trigger the resume.
func newResumeClaims(t *testing.T, mgr *SessionManager) *resumeClaims {
	t.Helper()
	c := &resumeClaims{ch: make(chan string, 4)}
	mgr.testResumeClaimedHook = func(targetID string) { c.ch <- targetID }
	return c
}

// wantClaim blocks for the next claim and fails unless it names want. The
// caller then waits for whatever settled state the resume produces, which is
// unambiguous once the claim is known to have happened.
func (c *resumeClaims) wantClaim(t *testing.T, want string) {
	t.Helper()
	if got := <-c.ch; got != want {
		t.Fatalf("resume claimed for %q, want %q", got, want)
	}
}
