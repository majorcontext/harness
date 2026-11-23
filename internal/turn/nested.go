package turn

import (
	"context"
	"slices"

	"github.com/majorcontext/harness/protocol"
)

// Nested lets a tool run calls of the other tools of its model call. A nested call applies the allowed tools
// of the model call and runs between its hooks. It runs no retention step.
type Nested interface {
	// Specs lists the tools that a nested call can name.
	Specs() []protocol.ToolSpec
	// Spec returns the spec of the tool name, also a tool that only the resolver of the sources finds.
	Spec(ctx context.Context, name string) (protocol.ToolSpec, bool)
	// Group opens an ordering domain for the calls of the call parent.
	Group(ctx context.Context, parent string) Group
}

// Group orders the calls submitted to it as the batch of a model call does: Alone and Keyed hold among them.
type Group interface {
	// Submit starts c and returns the func that waits for its result. The order of the calls of Submit is
	// the order that the rules of the batch use.
	Submit(c protocol.ToolCall) (wait func() protocol.ToolResult)
	// Close cancels the calls that did not end and waits for them.
	Close()
}

// Nesting is a Tool that runs nested calls. The turn calls Nest when it binds the tools of a model call, and
// the model call runs the tool that Nest returns.
type Nesting interface {
	Nest(n Nested) Tool
}

// NestedRecorder is a Turn that records each nested call that ends.
type NestedRecorder interface {
	NestedCall(rec protocol.NestedCallRecorded) error
}

type nested struct {
	t  runner
	to Turn
}

// bindNested returns tools with each Nesting tool bound to a Nested of t. It sets t.runnable to the bound list.
func bindNested(t *runner, to Turn) {
	if !slices.ContainsFunc(t.runnable, func(x Tool) bool { _, ok := x.(Nesting); return ok }) {
		return
	}
	n := &nested{to: to}
	bound := slices.Clone(t.runnable)
	t.nests = map[string]bool{}
	for i, x := range bound {
		if b, ok := x.(Nesting); ok {
			t.nests[x.Spec().Name] = true
			bound[i] = b.Nest(n)
		}
	}
	t.runnable = bound
	n.t = *t
}

func (n *nested) Specs() []protocol.ToolSpec {
	var out []protocol.ToolSpec
	for _, x := range n.t.runnable {
		if s, ok := safeSpec(x); ok {
			out = append(out, s)
		}
	}
	return out
}

func safeSpec(x Tool) (s protocol.ToolSpec, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return x.Spec(), true
}

func (n *nested) Spec(ctx context.Context, name string) (protocol.ToolSpec, bool) {
	if x := findIn(n.t.runnable, name); x != nil {
		return safeSpec(x)
	}
	if n.t.resolver != nil {
		if x := n.t.resolver.ResolveTool(ctx, name); x != nil {
			return safeSpec(x)
		}
	}
	return protocol.ToolSpec{}, false
}

func (n *nested) Group(ctx context.Context, parent string) Group {
	return &group{n: n, parent: parent, b: newBatch(ctx, ctx, n.t)}
}

type group struct {
	n      *nested
	parent string
	b      *batch
}

func (g *group) Submit(c protocol.ToolCall) func() protocol.ToolResult {
	g.b.mu.Lock()
	f := g.b.submit(c)
	g.b.mu.Unlock()
	return func() protocol.ToolResult {
		<-f.done
		if !f.out.ran {
			return protocol.ToolResult{Text: context.Cause(g.b.ctx).Error(), IsError: true}
		}
		g.record(c, f.out.res)
		return f.out.res
	}
}

func (g *group) Close() {
	g.b.stop(context.Canceled)
	g.b.wg.Wait()
}

func (g *group) record(c protocol.ToolCall, r protocol.ToolResult) {
	if rec, ok := g.n.to.(NestedRecorder); ok {
		_ = rec.NestedCall(protocol.NestedCallRecorded{CallID: g.parent, Name: c.Name, Arguments: c.Arguments, IsError: r.IsError, Text: r.Text})
	}
}
