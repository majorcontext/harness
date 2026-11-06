package harness

import (
	"context"
	"fmt"
	"slices"

	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/selftool"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/protocol"
)

func (r *Runtime) sessionPrompt(ctx context.Context, id string, c protocol.SessionCreated, profile *prompt.Profile, own session.Ownership) (*selftool.Prompt, error) {
	read := r.prompt(r.listSkills(ctx, id))
	if c.SystemBlocks != nil {
		read.Segments, read.Instructions = slices.Clone(c.SystemBlocks), nil
		if read.SkillList != "" {
			read.Segments = append(read.Segments, read.SkillList)
		}
	}
	sp := selftool.NewPrompt(read, c.Agent, r.profile(c.Agent, profile))
	n := len(sp.System())
	if c.SystemBlocks != nil {
		n = len(c.SystemBlocks)
	}
	if i := c.SystemCacheBreakpoint; i != nil && (*i < 0 || *i >= n) {
		session.Discard(own, id, r.log)
		return nil, fmt.Errorf("%w: system_cache_breakpoint must name a system block", ErrInvalidRequest)
	}
	return sp, nil
}

func copySystem(req protocol.CreateSession, c *protocol.SessionCreated) {
	c.SystemBlocks = slices.Clone(req.SystemBlocks)
	if req.SystemCacheBreakpoint != nil {
		i := *req.SystemCacheBreakpoint
		c.SystemCacheBreakpoint = &i
	}
}
