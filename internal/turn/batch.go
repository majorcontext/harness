package turn

import (
	"context"
	"sync"

	"github.com/majorcontext/harness/protocol"
)

// MaxParallel bounds the tool calls of one model call that run at once.
const MaxParallel = 8

// Alone is a Tool that runs with no other call of its model call in flight:
// each earlier call ends before it starts, and it ends before a later call
// starts. A tool that changes what the next call sees, such as the model,
// the goal, or the MCP servers, is Alone. A forwarded call is Alone only when its tool is.
// A tool whose spec is not ReadOnly runs the same way.
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

// flight is one call of a batch, from the moment it is submitted.
type flight struct {
	call    protocol.ToolCall
	out     outcome
	done    chan struct{}
	claimed bool
}

// batch runs the tool calls of one model call. A call starts when it is
// submitted, which is when the model stream delivers it, or at the end of the
// step for a call that the stream did not deliver. Calls are ordered by
// submission: a ReadOnly call waits only for the last call that is not
// ReadOnly or Alone, and runs beside other ReadOnly calls, at most MaxParallel
// at once. Any other call waits for every earlier call and holds the batch
// alone until it ends.
type batch struct {
	ctx, step context.Context
	stop      context.CancelCauseFunc
	t         runner
	slots     chan struct{}
	wg        sync.WaitGroup

	mu      sync.Mutex
	flights map[string]*flight
	// held is set once a call must wait for the end of the step: no later call starts early.
	held bool
	// excl ends with the last exclusive call; reads are the read-only calls since.
	excl  <-chan struct{}
	reads []<-chan struct{}
	tail  map[string]chan struct{}
}

func newBatch(ctx, step context.Context, t runner) *batch {
	wctx, stop := context.WithCancelCause(ctx)
	return &batch{ctx: wctx, step: step, stop: stop, t: t, slots: make(chan struct{}, MaxParallel),
		flights: map[string]*flight{}, tail: map[string]chan struct{}{}}
}

// readOnly reports whether c may run beside other read-only calls.
func (t runner) readOnly(c protocol.ToolCall) (ro bool) {
	defer func() {
		if recover() != nil {
			ro = false
		}
	}()
	if t.alone(c) {
		return false
	}
	tools, c := t.target(c)
	tool := findIn(tools, c.Name)
	return tool != nil && tool.Spec().ReadOnly
}

// Late is a Tool whose call starts only when its model call has ended, because
// its result reads the state that the end of the model call changes, such as the usage.
type Late interface {
	Late()
}

// late reports whether c waits for the end of the model call.
func (t runner) late(c protocol.ToolCall) (late bool) {
	defer func() {
		if recover() != nil {
			late = true
		}
	}()
	tools, c := t.target(c)
	_, late = findIn(tools, c.Name).(Late)
	return late
}

// Call submits c, which the model stream delivered complete. It starts c at
// once when its turn comes under the rules of batch. A call to the question
// tool waits for the end of the step and takes no turn. A call to a Late tool,
// or with no ID to match its item, also waits, and so does every call after it,
// so that calls keep their arrival order.
func (b *batch) Call(c protocol.ToolCall) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.held || c.Name == AskTool && b.t.find(AskTool) != nil {
		return
	}
	if c.ID == "" || b.t.late(c) {
		b.held = true
		return
	}
	if _, ok := b.flights[c.ID]; !ok {
		b.submit(c)
	}
}

// submit starts c. The caller holds b.mu.
func (b *batch) submit(c protocol.ToolCall) *flight {
	f := &flight{call: c, done: make(chan struct{})}
	if c.ID != "" {
		b.flights[c.ID] = f
	}
	var wait []<-chan struct{}
	if b.excl != nil {
		wait = append(wait, b.excl)
	}
	ro := b.t.readOnly(c)
	if ro {
		b.reads = append(b.reads, f.done)
	} else {
		wait = append(wait, b.reads...)
		b.excl, b.reads = f.done, nil
	}
	var prev <-chan struct{}
	var next chan struct{}
	if k := b.t.key(c); k != "" {
		prev, next = b.tail[k], make(chan struct{})
		b.tail[k] = next
	}
	b.wg.Go(func() {
		if next != nil {
			defer close(next)
		}
		defer close(f.done)
		if prev != nil {
			<-prev
		}
		for _, w := range wait {
			<-w
		}
		if ro {
			b.slots <- struct{}{}
			defer func() { <-b.slots }()
		}
		b.exec(f)
	})
	return f
}

// exec runs f unless the turn ended, and turns a panic into an error
// result, so that every call has one result.
func (b *batch) exec(f *flight) {
	if b.step.Err() != nil || b.ctx.Err() != nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			f.out = outcome{protocol.ToolResult{Text: toolPanicked, IsError: true}, true}
		}
	}()
	f.out = outcome{b.t.run(b.ctx, f.call), true}
}

// started reports whether any call has started or is waiting to start.
func (b *batch) started() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.flights) > 0
}

// abort cancels every call and waits for them to end. It discards their results.
// A nil cause or a nil batch does nothing.
func (b *batch) abort(cause error) {
	if b == nil || cause == nil {
		return
	}
	b.stop(cause)
	b.wg.Wait()
}

// runBatch runs calls, at most MaxParallel read-only calls at once, and gives the result of
// each call that ran to emit in call order as soon as it and every earlier
// call have ended. A call that the stream already
// started is not started again. When ctx ends, every running
// call is canceled and none starts; the result of each call that ran is still
// recorded in call order, and the cause is returned. When only step ends, running calls finish, no new call starts, and
// the cause of step is returned unless every call ran. It returns after
// every call it started has ended.
func (b *batch) run(ctx context.Context, calls []protocol.ToolCall, emit func(i int, r protocol.ToolResult) error) error {
	defer b.stop(nil)
	fs := make([]*flight, len(calls))
	b.mu.Lock()
	for i, c := range calls {
		f := b.flights[c.ID]
		if f == nil || f.claimed {
			f = b.submit(c)
		}
		f.claimed = true
		fs[i] = f
	}
	b.mu.Unlock()
	err := b.collect(ctx, fs, emit)
	if err != nil {
		b.stop(err)
	}
	b.wg.Wait()
	if err != nil {
		return err
	}
	for _, f := range fs {
		if !f.out.ran {
			return context.Cause(b.step)
		}
	}
	return nil
}

func (b *batch) collect(ctx context.Context, fs []*flight, emit func(i int, r protocol.ToolResult) error) error {
	for i, f := range fs {
		<-f.done
		if !f.out.ran {
			continue
		}
		if err := emit(i, f.out.res); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return nil
}
