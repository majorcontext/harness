package harness

import (
	"context"
	"fmt"
)

type entry struct {
	ready chan struct{}
	// create is set when the load creates the session.
	create bool
	s      *Session
	err    error
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
			e = &entry{ready: make(chan struct{}), create: l.created != nil}
			r.sessions[id] = e
			r.group.Add(1)
			r.mu.Unlock()
			e.s, e.err = r.start(ctx, id, e, l)
			if e.err != nil {
				r.forget(id, e)
			}
			close(e.ready)
			if e.err == nil {
				r.run(e.s, l.created != nil)
			}
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
			if e.err != nil {
				continue
			}
			return nil, fmt.Errorf("%w: %s", ErrSessionExists, id)
		}
		if e.err != nil {
			return nil, e.err
		}
		if !e.s.a.View().Stopped {
			return e.s, nil
		}
		select {
		case <-e.s.a.Done():
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r.forget(id, e)
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

func (r *Runtime) forget(id string, e *entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[id] == e {
		delete(r.sessions, id)
	}
}
