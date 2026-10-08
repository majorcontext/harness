package turn

import (
	"context"
	"sync"

	"github.com/majorcontext/harness/protocol"
)

// maxParallel bounds the tool calls of one model call that run at once.
const maxParallel = 8

// Alone is a Tool that runs with no other call of its model call in flight:
// each earlier call ends before it starts, and it ends before a later call
// starts. A tool that changes what the next call sees, such as the model,
// the goal, or the loaded MCP tools, is Alone.
type Alone interface {
	Alone()
}

// Keyed is a Tool that names the resource a call touches. Calls of one model
// call with the same non-empty key run one at a time in call order, beside
// calls with another key.
type Keyed interface {
	Key(c protocol.ToolCall) string
}

const toolPanicked = "tool call failed: the tool panicked"

// outcome is the result of one call of a batch. ran is false for a call that
// never started because the turn ended first.
type outcome struct {
	res protocol.ToolResult
	ran bool
}

// batch runs the tool calls of one model call.
type batch struct {
	ctx, step context.Context
	run       func(context.Context, protocol.ToolCall) protocol.ToolResult
	join      func(context.Context, protocol.ToolCall, protocol.ToolResult) protocol.ToolResult
	alone     func(name string) bool
	key       func(c protocol.ToolCall) string
	calls     []protocol.ToolCall
	outs      []outcome
	done      []chan struct{}
}

// runBatch runs calls, at most maxParallel at once, and gives the result of
// each call that ran to emit in call order as soon as it and every earlier
// call have ended, after the Join of the hooks. When ctx ends, every running
// call is canceled and none starts; the result of each call that ran is still
// recorded in call order, and the cause is returned. When only step ends, running calls finish, no new call starts, and
// the cause of step is returned unless every call ran. It returns after
// every call it started has ended.
func runBatch(ctx, step context.Context, t runner, calls []protocol.ToolCall, emit func(i int, r protocol.ToolResult) error) error {
	wctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	b := &batch{ctx: wctx, step: step, run: t.run, join: t.join, alone: t.alone, key: t.key, calls: calls,
		outs: make([]outcome, len(calls)), done: make([]chan struct{}, len(calls))}
	for i := range b.done {
		b.done[i] = make(chan struct{})
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		b.schedule()
	}()
	err := b.collect(ctx, emit)
	if err != nil {
		stop(err)
	}
	<-finished
	if err != nil {
		return err
	}
	for _, o := range b.outs {
		if !o.ran {
			return context.Cause(step)
		}
	}
	return nil
}

func (b *batch) collect(ctx context.Context, emit func(i int, r protocol.ToolResult) error) error {
	for i := range b.calls {
		<-b.done[i]
		if !b.outs[i].ran {
			continue
		}
		if err := emit(i, b.join(ctx, b.calls[i], b.outs[i].res)); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return nil
}

// schedule starts every call and closes the done channel of each. A call
// that is Alone waits for the calls before it. A call with a key waits for
// the call before it with the same key, then for a free slot, so a call that
// cannot start holds no slot.
func (b *batch) schedule() {
	var wg sync.WaitGroup
	slots := make(chan struct{}, min(maxParallel, len(b.calls)))
	tail := map[string]chan struct{}{}
	for i, c := range b.calls {
		if b.alone(c.Name) {
			wg.Wait()
			b.exec(i)
			continue
		}
		var prev <-chan struct{}
		var next chan struct{}
		if k := b.key(c); k != "" {
			prev, next = tail[k], make(chan struct{})
			tail[k] = next
		}
		wg.Go(func() {
			if next != nil {
				defer close(next)
			}
			if prev != nil {
				<-prev
			}
			slots <- struct{}{}
			defer func() { <-slots }()
			b.exec(i)
		})
	}
	wg.Wait()
}

// exec runs call i unless the turn ended, and turns a panic into an error
// result, so that every call has one result.
func (b *batch) exec(i int) {
	defer close(b.done[i])
	if b.step.Err() != nil || b.ctx.Err() != nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			b.outs[i] = outcome{protocol.ToolResult{Text: toolPanicked, IsError: true}, true}
		}
	}()
	b.outs[i] = outcome{b.run(b.ctx, b.calls[i]), true}
}
