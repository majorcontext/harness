package harness

import (
	"cmp"
	"context"
	"fmt"

	"github.com/majorcontext/harness/internal/storecheck"
	"github.com/majorcontext/harness/protocol"
)

// Create creates a session and runs it. An empty model is Config.Model, or
// config.DefaultModel. A SessionOption applies to this session only.
func (r *Runtime) Create(ctx context.Context, req protocol.CreateSession, opts ...SessionOption) (*Session, error) {
	if req.Model == "" {
		req.Model = r.resolve("")
	}
	if err := checkEffort(req.Effort); err != nil {
		return nil, err
	}
	if req.MaxSteps < 0 {
		return nil, fmt.Errorf("%w: max_steps must be >= 0", ErrInvalidRequest)
	}
	for _, block := range req.SystemBlocks {
		if block == "" {
			return nil, fmt.Errorf("%w: system_blocks must not contain an empty block", ErrInvalidRequest)
		}
	}
	so, err := r.sessionOptions(opts)
	if err != nil {
		return nil, err
	}
	if so.allowedTools != nil || so.maxSteps != nil {
		return nil, fmt.Errorf("%w: WithAllowedTools and WithMaxSteps are only valid for Open", ErrInvalidRequest)
	}
	if err := r.startPlugins(ctx, false); err != nil {
		return nil, err
	}
	id := cmp.Or(req.ID, "ses_"+newSuffix())
	if err := storecheck.Name(id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	created := protocol.SessionCreated{Model: req.Model, Origin: req.Origin, Settings: protocol.Settings{Effort: req.Effort, ServiceTier: req.ServiceTier}, AllowedTools: req.AllowedTools, MaxSteps: req.MaxSteps}
	copySystem(req, &created)
	return r.create(ctx, id, launch{created: &created, mcp: so.mcp})
}
