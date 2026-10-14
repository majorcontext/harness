package harness

import (
	"context"
	"fmt"

	"github.com/majorcontext/harness/internal/backend"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/protocol"
)

// ErrModelUnavailable reports a model that no configured provider serves.
var ErrModelUnavailable = backend.ErrUnavailable

// checkModel reports why model cannot start a session that allows the
// names. A backend that owns the loop runs only its built-in tools and the
// tools that no model owns.
func (r *Runtime) checkModel(model string, names []string, session bool) error {
	ref, err := modelref.ParseModelRef(model)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	be, err := r.models.Check(ref)
	if err != nil {
		return err
	}
	caps := be.Capabilities(model)
	if !caps.OwnsLoop {
		return r.checkShadowed(names)
	}
	for _, n := range caps.Tools {
		if r.owns(n) {
			return fmt.Errorf("%w: tool %q has the name of a built-in tool of %s", ErrInvalidRequest, n, model)
		}
	}
	for _, n := range names {
		if !r.knownIn(model, n, session) {
			return fmt.Errorf("%w: %s has no tool %q", ErrInvalidRequest, model, n)
		}
	}
	return nil
}

// changeModel reports why a session that allows the names cannot move to
// model to. A backend that owns its context reads the history of another
// provider through the history tool, so any two models may follow each other.
func (r *Runtime) changeModel(session bool) func(_, to string, names []string) error {
	return func(_, to string, names []string) error { return r.checkModel(to, names, session) }
}

// checkShadowed fails when the session allows every tool and an embedder tool
// has the name of a built-in tool of the WorkDir.
func (r *Runtime) checkShadowed(names []string) error {
	if names != nil {
		return nil
	}
	if taken := r.shadowed(); len(taken) > 0 {
		return fmt.Errorf("%w: tool %q has the name of a built-in tool, which the session allows; list the allowed tools to replace it", ErrInvalidRequest, taken[0])
	}
	return nil
}

// createdForOpen reads the session.created record of session id and checks
// its tool names as Create does.
func (r *Runtime) createdForOpen(ctx context.Context, id string) (protocol.SessionCreated, error) {
	c, err := r.created(ctx, id)
	if err == nil && !r.models.Capabilities(c.Model).OwnsLoop {
		err = r.checkShadowed(c.AllowedTools)
	}
	return c, err
}
