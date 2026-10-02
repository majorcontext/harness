// Package testpoll provides the deadline-bounded poll helper for test code
// that must observe state outside the test process.
//
// A real OS process's exit, a file a grandchild process flushed, or /proc's
// view of a zombie cannot use a testing/synctest bubble or an in-process
// channel. Use this package ONLY for such state. In-process state has a
// channel, a long-poll endpoint, or a seam that production code can add; use
// one of those instead.
package testpoll

import (
	"testing"
	"time"
)

// Interval is the default gap between attempts. It is short enough that a
// fast machine is not meaningfully delayed, and it never appears in an
// assertion, so no test depends on its value.
//
// It suits a cheap local check — a Status read, a small file read. A check
// that costs a real network round trip should pass its own coarser
// interval (see Until's every argument): at 2ms, a wait for a turn that
// streams for tens of seconds would issue thousands of requests, and those
// requests compete with the very work the wait is timing.
const Interval = 2 * time.Millisecond

// Until calls check until it reports true, then returns. It fails the test
// with msg once timeout elapses.
//
// timeout is a failure bound, never a synchronization delay: the happy path
// returns as soon as check reports true, so a generous timeout costs a slow
// machine nothing and a loaded machine no flake. Pick it far above any
// plausible real latency.
//
// every overrides Interval for a check that is expensive to run. Pass at
// most one; a non-positive value falls back to Interval.
func Until(t *testing.T, timeout time.Duration, msg string, check func() bool, every ...time.Duration) {
	t.Helper()
	if !UntilNoT(timeout, check, every...) {
		t.Fatalf("%s (after %s)", msg, timeout)
	}
}

// UntilNoT is Until for a goroutine that is not the test's own goroutine,
// where calling t.Fatalf is illegal. It reports whether check succeeded
// before the timeout; the caller propagates the failure.
func UntilNoT(timeout time.Duration, check func() bool, every ...time.Duration) bool {
	interval := Interval
	if len(every) > 0 && every[0] > 0 {
		interval = every[0]
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(interval)
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
