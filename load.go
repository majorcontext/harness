package harness

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/majorcontext/harness/internal/storecheck"
)

type entry struct {
	ready   chan struct{}
	started chan struct{}
	// mcp holds the names of the session MCP servers.
	mcp []string
	// create is set when the load creates the session.
	create bool
	s      *Session
	err    error
}

func (r *Runtime) openWithAccounts(ctx context.Context, id string, accounts map[string]string) (*Session, error) {
	if err := storecheck.Name(id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return r.load(ctx, id, launch{accounts: &accounts})
}

func (r *Runtime) load(ctx context.Context, id string, l launch) (*Session, error) {
	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, ErrDraining
		}
		e := r.sessions[id]
		if e == nil {
			if name, clash := r.mcpClash(l.mcp); clash {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w: session MCP server %q has the name of a runtime server", ErrInvalidRequest, name)
			}
			e = &entry{ready: make(chan struct{}), started: make(chan struct{}), create: l.created != nil, mcp: slices.Sorted(maps.Keys(l.mcp))}
			r.sessions[id] = e
			r.group.Add(1)
			r.mu.Unlock()
			e.s, e.err = r.start(ctx, id, e, l)
			if e.err != nil {
				r.forget(id, e, nil)
			}
			close(e.ready)
			if e.err == nil {
				r.run(e.s, l.created != nil)
			}
			close(e.started)
			r.group.Done()
			return e.s, e.err
		}
		r.mu.Unlock()
		if l.created != nil && e.create {
			return nil, fmt.Errorf("%w: %s", ErrSessionExists, id)
		}
		// A Create waits for an Open that is in flight: an Open that races
		// the spawn of a session finds no log, and must not fail the spawn.
		select {
		case <-e.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if l.created != nil {
			if errors.Is(e.err, ErrSessionNotFound) {
				continue
			}
			if e.err != nil {
				return nil, e.err
			}
			return nil, fmt.Errorf("%w: %s", ErrSessionExists, id)
		}
		if e.err != nil {
			return nil, e.err
		}
		if !e.s.a.View().Stopped {
			if l.accounts != nil {
				select {
				case <-e.started:
				default:
					return nil, fmt.Errorf("account selections cannot be applied while the child is opening; retry task send after it loads")
				}
				if err := e.s.a.UpdateAccountsIfIdle(ctx, *l.accounts); err != nil {
					return nil, err
				}
			}
			return e.s, nil
		}
		select {
		case <-e.s.a.Done():
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r.forget(id, e, nil)
	}
}

// run runs session s after load publishes it, so a tool of its first run
// finds it. An opened session then settles or opens its unsettled children.
func (r *Runtime) run(s *Session, create bool) {
	s.a.Run()
	if create {
		close(s.recovered)
		return
	}
	r.group.Go(func() {
		r.tree.Recover(s.a)
		close(s.recovered)
	})
}

func (r *Runtime) forget(id string, e *entry, then func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[id] == e {
		delete(r.sessions, id)
	}
	if then != nil {
		then()
	}
}
