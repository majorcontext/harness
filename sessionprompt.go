package harness

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/protocol"
)

// promptReader returns the function that reads the system prompt of session id as a new session would: the default
// segments or the blocks of the override, the skills, and the body of the agent profile. profile is the profile that
// the first read uses, when the caller has read it.
func (r *Runtime) promptReader(id string, c protocol.SessionCreated, profile *prompt.Profile) func(context.Context) prompt.Info {
	var first atomic.Pointer[prompt.Profile]
	first.Store(profile)
	return func(ctx context.Context) prompt.Info {
		read := r.prompt(r.listSkills(ctx, id))
		if c.SystemBlocks != nil {
			read.Segments, read.Instructions = slices.Clone(c.SystemBlocks), nil
			if read.SkillList != "" {
				read.Segments = append(read.Segments, read.SkillList)
			}
		}
		if c.Agent != "" {
			if t := strings.Trim(r.profile(c.Agent, first.Swap(nil)).Prompt, "\n"); t != "" {
				read.Segments = append(slices.Clone(read.Segments), t)
			}
		}
		return read
	}
}

// checkBreakpoint fails a session that is created whose cache selection names no block of its system prompt.
func (r *Runtime) checkBreakpoint(ctx context.Context, id string, c protocol.SessionCreated, created bool, read func(context.Context) prompt.Info, own session.Ownership) error {
	i := c.SystemCacheBreakpoint
	if i == nil || !created {
		return nil
	}
	n := len(c.SystemBlocks)
	if c.SystemBlocks == nil {
		n = len(read(ctx).Segments)
	}
	if *i < 0 || *i >= n {
		session.Discard(own, id, r.log)
		return fmt.Errorf("%w: system_cache_breakpoint must name a system block", ErrInvalidRequest)
	}
	return nil
}

func copySystem(req protocol.CreateSession, c *protocol.SessionCreated) {
	c.SystemBlocks = slices.Clone(req.SystemBlocks)
	if req.SystemCacheBreakpoint != nil {
		i := *req.SystemCacheBreakpoint
		c.SystemCacheBreakpoint = &i
	}
}
