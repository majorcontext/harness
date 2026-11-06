package harness

import (
	"context"
	"fmt"
	"iter"
	"sync/atomic"

	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/protocol"
)

// Stream owns the lossless events of one input and must be closed when not consumed.
type Stream struct {
	*Turn
	sub  *session.Subscription
	used atomic.Bool
}

// PromptStream registers a lossless queue before admitting a new queued input.
// It rejects steer delivery, control commands, and repeated IDs; the caller must close the stream.
// The context bounds the input's lifetime, but Close only releases the event queue.
func (s *Session) PromptStream(ctx context.Context, in protocol.Input) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.Delivery != "" && in.Delivery != protocol.DeliveryQueue {
		return nil, fmt.Errorf("%w: PromptStream requires queue delivery", ErrInvalidRequest)
	}
	in.Delivery = protocol.DeliveryQueue
	sub := s.a.Subscribe(ctx)
	adm, err := s.admitInput(ctx, in, func(adm protocol.Admitted) { s.Turn(adm).bind(ctx) })
	if err != nil {
		sub.Close()
		return nil, err
	}
	if adm.Repeat {
		sub.Close()
		return nil, fmt.Errorf("%w: PromptStream requires a new input ID", ErrInvalidRequest)
	}
	return &Stream{Turn: s.Turn(adm), sub: sub}, nil
}

func (t *Turn) bind(ctx context.Context) {
	r := t.s.r
	if ctx.Done() == nil || r.hold() != nil {
		return
	}
	go func() {
		defer r.group.Done()
		wait, cancel := context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(r.closing, cancel)()
		_, _ = t.Wait(wait)
		if ctx.Err() != nil {
			_ = t.Cancel(r.closing)
		}
	}()
}

// Close releases queued events without canceling the turn and is safe for concurrent use.
func (s *Stream) Close() { s.sub.Close() }

// Events yields one lossless turn stream and closes it on completion, cancellation, or break.
func (s *Stream) Events(ctx context.Context) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		if !s.used.CompareAndSwap(false, true) {
			yield(protocol.Event{}, fmt.Errorf("%w: stream events already consumed", ErrInvalidRequest))
			return
		}
		defer s.Close()
		events := s.sub.Events(ctx, s.adm.Seq-1)
		if _, err := s.followEvents(events, func(e protocol.Event) bool { return yield(e, nil) }, true); err != nil {
			yield(protocol.Event{}, err)
		}
	}
}
