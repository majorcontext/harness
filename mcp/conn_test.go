package mcp

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"
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
	c.wmu.Lock()
	t.Cleanup(c.wmu.Unlock)
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
