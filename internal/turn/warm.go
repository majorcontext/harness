package turn

import "context"

// Warmer is an optional Backend capability. Warm prepares the transport for
// req, the first model call of a session, and makes no call that is recorded.
type Warmer interface {
	Warm(ctx context.Context, req Request) error
}

// WarmGate is an optional capability of a Warmer. CanWarm has no side effect.
// A Warmer with no WarmGate always warms.
type WarmGate interface {
	CanWarm(model string) bool
}

// Warm warms b with req described as the first model call, with tools and
// src. A model that cannot warm costs no tool discovery and no hook.
func Warm(ctx context.Context, b Backend, req Request, tools []Tool, src Source) error {
	w, ok := b.(Warmer)
	if !ok {
		return nil
	}
	if g, ok := b.(WarmGate); ok && !g.CanWarm(req.Model) {
		return nil
	}
	describe(ctx, &req, tools, src, b.Capabilities(req.Model).OwnsLoop)
	return w.Warm(ctx, req)
}
