package harness

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/internal/selftool"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// bound returns ctx joined with the start of Close and holds one unit of
// the work that Close waits for. The caller calls done when the work ends.
func (r *Runtime) bound(ctx context.Context) (context.Context, func(), error) {
	if err := r.hold(); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.closing, cancel)
	return ctx, func() { stop(); cancel(); r.group.Done() }, nil
}

// mcpCall serves client/mcp.call. Every configured server is open to it;
// the allowed-tools filter of a session does not apply.
func (r *Runtime) mcpCall(ctx context.Context, server, tool string, args json.RawMessage) (protocol.ToolResult, error) {
	ctx, done, err := r.bound(ctx)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	defer done()
	return r.mcp.Call(ctx, server, tool, args)
}

// generate serves client/generate: one model call with no tools and no session.
func (r *Runtime) generate(ctx context.Context, model string, req turn.Request) (modelref.ModelRef, string, protocol.Usage, error) {
	ref, err := modelref.ParseModelRef(r.resolve(model))
	if err != nil {
		return ref, "", protocol.Usage{}, fmt.Errorf("invalid model %q: %w", model, err)
	}
	if err := (selftool.Catalog{Aliases: r.aliases, Providers: r.models.Providers}).CheckChild(ref.String()); err != nil {
		return ref, "", protocol.Usage{}, err
	}
	ctx, done, err := r.bound(ctx)
	if err != nil {
		return ref, "", protocol.Usage{}, err
	}
	defer done()
	req.Model = ref.String()
	if req.MaxTokens == 0 {
		req.MaxTokens = r.maxTokens
	}
	text, usage, err := turn.Ask(ctx, r.models, req, r.limits)
	return ref, text, usage, err
}
