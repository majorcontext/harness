package session

import (
	"context"
	"iter"

	"github.com/majorcontext/harness/protocol"
)

// Subscription owns a lossless live queue until Close or its context ends.
type Subscription struct {
	a      *Actor
	frames *liveSubscriber
	ctx    context.Context
	cancel context.CancelFunc
}

// Subscribe registers an unbounded live queue before the caller admits an input.
func (a *Actor) Subscribe(ctx context.Context) *Subscription {
	ctx, cancel := context.WithCancel(ctx)
	frames, _ := a.live.subscribe(true)
	s := &Subscription{a: a, frames: frames, ctx: ctx, cancel: cancel}
	context.AfterFunc(ctx, func() { a.live.unsubscribe(frames) })
	return s
}

// Close unregisters the subscription and releases its queued frames.
func (s *Subscription) Close() {
	s.cancel()
	s.a.live.unsubscribe(s.frames)
}

// Events yields queued and future frames in order with their durable records.
func (s *Subscription) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(ctx, s.Close)()
		defer context.AfterFunc(s.ctx, cancel)()
		if s.ctx.Err() != nil {
			cancel()
		}
		defer s.Close()
		s.a.events(ctx, after, s.frames, nil)(yield)
	}
}
