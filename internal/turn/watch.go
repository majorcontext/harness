package turn

import (
	"context"
	"errors"
	"time"

	"github.com/majorcontext/harness/protocol"
)

// watch calls b once. With a positive idle, a call that reports nothing to
// s for idle ends with errStalled.
func watch(ctx context.Context, b Backend, req Request, s Sink, idle time.Duration) error {
	var timer *time.Timer
	if idle > 0 {
		var cancel context.CancelCauseFunc
		ctx, cancel = context.WithCancelCause(ctx)
		defer cancel(nil)
		timer = time.AfterFunc(idle, func() { cancel(errStalled) })
		defer timer.Stop()
	}
	if target, ok := s.(*sink); ok && target.early != nil {
		target.normalizeCtx = ctx
		defer func() { target.normalizeCtx = nil }()
	}
	if timer != nil {
		s = watched{s, func() { timer.Reset(idle) }}
	}
	err := b.Run(ctx, req, s)
	if errors.Is(context.Cause(ctx), errStalled) {
		return errStalled
	}
	return err
}

// watched is a Sink that calls alive on each delta, item, and Alive.
type watched struct {
	Sink
	alive func()
}

func (w watched) Item(m protocol.LogMessage) error {
	w.alive()
	return w.Sink.Item(m)
}

func (w watched) Delta(itemID string, d Delta) {
	w.alive()
	w.Sink.Delta(itemID, d)
}

func (w watched) Alive() { w.alive() }

func (w watched) Call(c protocol.ToolCall) {
	w.alive()
	if cs, ok := w.Sink.(CallSink); ok {
		cs.Call(c)
	}
}
