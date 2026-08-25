// Package testpoll provides the ONE sanctioned deadline-bounded poll
// helper for test code that must observe genuinely out-of-process state.
//
// AGENTS.md bans time.Sleep in tests without exception, and names exactly
// two sanctioned time mechanisms: a testing/synctest bubble, or an injected
// fake clock/timer seam. Both require the state under observation to live
// inside the test process. A real OS process's exit, a file a grandchild
// process flushed, or /proc's view of a zombie is none of those: fake time
// does not govern kernel scheduling, and no in-process channel crosses a
// process boundary. AGENTS.md's cross-process carve-out covers exactly that
// case, and requires the wait to go "through its deadline-bounded poll
// helper — never a bare sleep loop written inline". This package is that
// helper, shared so every such wait reads the same and no test grows its
// own inline sleep loop.
//
// Use it ONLY for state that crosses an OS process boundary. In-process
// state — a manager's status field, a server's session state, a queue
// depth — has a channel, a production long-poll endpoint, or a seam that
// can be added to the production code. Reach for one of those instead; a
// poll loop over in-process state is the guessed-deadline flakiness this
// package exists to keep contained, not to bless.
package testpoll

import (
	"testing"
	"time"
)

// Interval is the gap between attempts. It is short enough that a fast
// machine is not meaningfully delayed, and it never appears in an
// assertion, so no test depends on its value.
const Interval = 2 * time.Millisecond

// Until calls check until it reports true, then returns. It fails the test
// with msg once timeout elapses.
//
// timeout is a failure bound, never a synchronization delay: the happy path
// returns as soon as check reports true, so a generous timeout costs a slow
// machine nothing and a loaded machine no flake. Pick it far above any
// plausible real latency.
func Until(t *testing.T, timeout time.Duration, msg string, check func() bool) {
	t.Helper()
	if !UntilNoT(timeout, check) {
		t.Fatalf("%s (after %s)", msg, timeout)
	}
}

// UntilNoT is Until for a goroutine that is not the test's own goroutine,
// where calling t.Fatalf is illegal. It reports whether check succeeded
// before the timeout; the caller propagates the failure.
func UntilNoT(timeout time.Duration, check func() bool) bool {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for {
		if check() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		<-ticker.C
	}
}
