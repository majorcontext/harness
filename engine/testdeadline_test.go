package engine

import (
	"testing"
	"time"
)

// deadlineReportMargin is how far before the test binary's own timeout a
// wait gives up, so that it can print WHAT it was waiting for instead of
// letting the binary panic with a bare goroutine dump.
//
// The exact value is immaterial. By the time it fires, the wait has
// already consumed the entire run's time budget and the binary was about
// to panic anyway; any value large enough to format one t.Fatalf is
// correct. It is not a synchronization delay — no wait ever spends it on
// the happy path, which returns on the signal it blocks for.
const deadlineReportMargin = 5 * time.Second

// failAfterTestDeadline returns the channel a wait selects on to give up,
// plus the function that releases its timer.
//
// AGENTS.md's "no guessed deadlines" rule says a wait blocks on its signal
// and lets the test binary's timeout catch a hang, rather than wrapping
// the receive in a short arbitrary failsafe that flakes under load. A bare
// receive obeys the rule but throws the diagnostic away: a hung
// waitForStatus would surface only as a goroutine dump, never as "node X
// never reached status Y".
//
// Deriving the bound from t.Deadline() keeps both properties. The value is
// the test binary's OWN -timeout (go test sets 10m by default), so no
// number is guessed at a call site, and it cannot flake under load however
// slow the machine is — reaching it means the run is over regardless.
//
// Two cases have no deadline to derive, and both return a nil channel. A
// nil channel blocks forever in a select, which is exactly the "let the
// test binary timeout catch hangs" behavior the rule asks for.
//
//   - A binary run with -timeout=0 has chosen to have no timeout.
//   - A test running inside a testing/synctest bubble has no wall-clock
//     deadline at all, and needs none: time in a bubble is fake, so a wait
//     that never completes leaves every goroutine durably blocked and the
//     bubble reports a deadlock immediately, with a full stack, at zero
//     wall-clock cost. A real timer would be worse there — it would run on
//     fake time and fire the instant the bubble idled.
func failAfterTestDeadline(t *testing.T) (<-chan time.Time, func()) {
	t.Helper()
	deadline, ok := testDeadline(t)
	if !ok {
		return nil, func() {}
	}
	d := time.Until(deadline) - deadlineReportMargin
	if d < 0 {
		d = 0
	}
	timer := time.NewTimer(d)
	return timer.C, func() { timer.Stop() }
}

// testDeadline is t.Deadline(), reporting "no deadline" instead of
// panicking inside a testing/synctest bubble.
//
// t.Deadline panics there by design ("t.Deadline called inside synctest
// bubble"): a bubble has no wall-clock deadline to report. There is no
// exported way to ask whether the current goroutine is in a bubble, so the
// panic is the only available signal — recovering from it is the test for
// it. Recovering is safe because t.Deadline has no other panic mode: it
// reads a field the testing package set before the test ran.
func testDeadline(t *testing.T) (deadline time.Time, ok bool) {
	t.Helper()
	defer func() {
		if recover() != nil {
			deadline, ok = time.Time{}, false
		}
	}()
	return t.Deadline()
}

// awaitSignal blocks until ch delivers, and returns what it delivered. It
// fails the test with msg once the test binary's own deadline is at hand.
//
// It is the one-shot form of the same rule the loop helpers follow: block
// on the real signal, never on a duration. Use it for a channel a test
// expects to fire exactly once — a turn's completion, a provider observing
// a cancellation, a goroutine reporting one result. A wait that must
// re-read state after each wakeup wants a loop helper (waitForStatus,
// flushWatch.waitUntil) instead. A caller with nothing to assert about the
// value just discards it.
//
// The non-blocking pre-check gives this helper the same "an already
// satisfied wait never fails" property the loop helpers get from testing
// their condition before their own select. Go picks uniformly at random
// among ready select cases, so without it a channel that has ALREADY
// delivered could still lose the coin flip to a ready giveUp and be
// reported as a deadline failure that never happened. That needs giveUp to
// be ready too — failAfterTestDeadline clamps its timer to zero for a wait
// that starts within deadlineReportMargin of the binary's whole -timeout —
// so it is far-fetched for a package that runs in seconds. It is still the
// exact asymmetry these helpers exist to remove.
func awaitSignal[T any](t *testing.T, ch <-chan T, msg string) T {
	t.Helper()
	giveUp, stop := failAfterTestDeadline(t)
	defer stop()
	return awaitSignalUntil(t, ch, giveUp, msg)
}

// awaitSignalUntil is awaitSignal with its give-up channel supplied rather
// than derived. It exists so the pre-check above can be tested: a caller
// can hand it a give-up channel that is ALREADY ready, which is the only
// state in which the two select cases race, and which
// failAfterTestDeadline will not produce on any run that is not already
// out of time.
func awaitSignalUntil[T any](t *testing.T, ch <-chan T, giveUp <-chan time.Time, msg string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	default:
	}
	select {
	case v := <-ch:
		return v
	case <-giveUp:
		t.Fatalf("%s (waited to the test binary's own deadline)", msg)
		var zero T
		return zero
	}
}

// TestAwaitSignalPrefersAnAlreadyDeliveredValue pins awaitSignal's
// "an already satisfied wait never fails" property.
//
// Go picks uniformly at random among ready select cases. Without the
// non-blocking pre-check, a channel that has already delivered loses that
// coin flip about half the time whenever the give-up channel is ready too,
// and the helper reports a deadline failure for a signal that did in fact
// arrive.
//
// The give-up channel is supplied ready, through awaitSignalUntil, because
// that is the only state in which the two cases race —
// failAfterTestDeadline produces it only for a wait starting within
// deadlineReportMargin of the binary's whole -timeout. 200 iterations put
// the chance of a missed regression below 2^-200.
func TestAwaitSignalPrefersAnAlreadyDeliveredValue(t *testing.T) {
	ready := make(chan time.Time)
	close(ready)
	for i := 0; i < 200; i++ {
		ch := make(chan int, 1)
		ch <- 42
		if got := awaitSignalUntil(t, (<-chan int)(ch), ready, "signal never arrived"); got != 42 {
			t.Fatalf("iteration %d: awaitSignalUntil = %d, want 42", i, got)
		}
	}
}
