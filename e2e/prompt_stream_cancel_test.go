package e2e

import (
	"context"
	"errors"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

func holdCanceledPromptStream(t *testing.T, r *run, stream *harness.Stream) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	held, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		var ended error
		for e, err := range stream.Events(ctx) {
			if err != nil {
				ended = err
				break
			}
			if e.Kind == protocol.KindItemDelta {
				close(held)
				<-resume
			}
		}
		done <- ended
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("stream ended before its text callback: %v", err)
	}
	cancel()
	r.fake.Release("answer")
	if _, err := stream.Wait(t.Context()); err != nil {
		close(resume)
		t.Fatal(err)
	}
	close(resume)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("held iterator cancellation = %v", err)
	}
}
