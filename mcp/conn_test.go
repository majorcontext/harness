package mcp

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// TestConnCancelledNotifyDoesNotBlockOnStuckWrite reproduces the case where
// the connection's write mutex is already held by some other, stuck write
// (e.g. a peer that stopped reading) at the moment a call's context is
// cancelled. The best-effort notifications/cancelled cleanup must never
// make the cancelled call itself block longer than the operation it was
// meant to cancel: call() must return promptly with ctx.Err(), not wait on
// wmu.
func TestConnCancelledNotifyDoesNotBlockOnStuckWrite(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	t.Cleanup(func() { _ = clientSide.Close(); _ = serverSide.Close() })

	gotRequest := make(chan struct{})
	stopReading := make(chan struct{})
	t.Cleanup(func() { close(stopReading) })
	go func() {
		r := bufio.NewReader(serverSide)
		if _, err := r.ReadBytes('\n'); err == nil {
			close(gotRequest)
		}
		// Never respond, and stop reading further so nothing else drains
		// the pipe; the call is left pending until ctx fires.
		<-stopReading
	}()

	c := newConn(clientSide, nil)
	go func() { _ = c.run() }()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- c.call(ctx, "tools/call", nil, nil)
	}()

	select {
	case <-gotRequest:
	case <-time.After(2 * time.Second):
		t.Fatal("server never observed the initial request")
	}

	// Simulate another write already stuck holding wmu (e.g. a concurrent
	// notify blocked on a peer that stopped reading), then cancel: the
	// pending call's cleanup notify must not be able to acquire wmu, so
	// call() must return without waiting for it.
	c.wmu <- struct{}{}
	t.Cleanup(func() { <-c.wmu })
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call() blocked on cancellation cleanup behind a stuck write instead of returning immediately")
	}
}

// TestConnSubsequentCallTimesOutBehindAbandonedNotify reproduces the case
// where the cancelled-notify goroutine's write itself gets stuck on a
// wedged peer (one that accepted the request but then stops reading
// entirely, never returning a response). The notify goroutine's deadline
// context (cancelledNotifyTimeout) must be enforced at the write layer —
// acquiring wmu and performing the actual write — so it can't hold wmu
// forever. A second, unrelated call made while the notify is stuck must
// still time out on its OWN deadline rather than blocking behind the
// abandoned notify's cancelledNotifyTimeout (1s).
//
// Run inside a synctest bubble: every blocking point involved (net.Pipe
// reads/writes, ctx.Done() channels, and — once fixed — wmu acquisition)
// is channel-based and therefore durably blocked, so fake time advances
// deterministically. Against the current (buggy) code, wmu is a
// sync.Mutex: Mutex.Lock is explicitly *not* durably blocking per
// testing/synctest's docs, so the second call's write genuinely wedges in
// real wall-clock time — this test must be run with a bounded -timeout to
// observe that failure (a clean assertion failure is not possible for a
// real, permanent deadlock).
func TestConnSubsequentCallTimesOutBehindAbandonedNotify(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientSide, serverSide := net.Pipe()
		t.Cleanup(func() { _ = clientSide.Close(); _ = serverSide.Close() })

		stopReading := make(chan struct{})
		t.Cleanup(func() { close(stopReading) })
		go func() {
			r := bufio.NewReader(serverSide)
			// Read exactly the first call's request line (so its write
			// succeeds), then go wedged: never read anything again,
			// simulating a peer that accepted one message and hung.
			_, _ = r.ReadBytes('\n')
			<-stopReading
		}()

		c := newConn(clientSide, nil)
		go func() { _ = c.run() }()

		// First call: its own short deadline fires, triggering the
		// best-effort notifications/cancelled cleanup goroutine. That
		// goroutine's write will itself wedge behind the now-silent peer.
		ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel1()
		done1 := make(chan error, 1)
		go func() { done1 <- c.call(ctx1, "tools/call", nil, nil) }()

		err1 := <-done1
		if !errors.Is(err1, context.DeadlineExceeded) {
			t.Fatalf("first call err = %v, want context.DeadlineExceeded", err1)
		}

		// Let the spawned cancelled-notify goroutine run until it parks
		// on its write to the wedged peer (holding wmu, in the unfixed
		// code, for the notify's own cancelledNotifyTimeout).
		synctest.Wait()

		// Second, independent call with its own short deadline. It must
		// time out on ITS OWN deadline, not the notify's 1s
		// cancelledNotifyTimeout.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel2()
		start := time.Now()
		done2 := make(chan error, 1)
		go func() { done2 <- c.call(ctx2, "tools/call2", nil, nil) }()

		err2 := <-done2
		elapsed := time.Since(start)

		if !errors.Is(err2, context.DeadlineExceeded) {
			t.Fatalf("second call err = %v, want context.DeadlineExceeded", err2)
		}
		if elapsed >= cancelledNotifyTimeout {
			t.Fatalf("second call took %v (>= cancelledNotifyTimeout %v): it waited behind the abandoned notify instead of timing out on its own deadline", elapsed, cancelledNotifyTimeout)
		}
	})
}
