package harness

import (
	"context"
	"fmt"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/backend"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// ErrModelUnavailable reports a model that no configured provider serves.
var ErrModelUnavailable = backend.ErrUnavailable

// checkModel reports why model cannot start a session that allows the
// names. A backend that owns the loop runs only its built-in tools, the
// built-in tools of the WorkDir, and the tools that no model owns.
func (r *Runtime) checkModel(model string, names []string, session bool) error {
	ref, err := modelref.ParseModelRef(model)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	be, err := r.models.Check(ref)
	if err != nil {
		return err
	}
	if err := r.checkShadowed(names); err != nil {
		return err
	}
	caps := be.Capabilities(model)
	if !caps.OwnsLoop {
		return nil
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
	if err == nil {
		err = r.checkShadowed(c.AllowedTools)
	}
	return c, err
}

// lineageOf returns the creation record of session id and its place in the
// session tree. On an error it releases own.
func (r *Runtime) lineageOf(ctx context.Context, own session.Ownership, id string, l launch) (c protocol.SessionCreated, root string, depth int, err error) {
	if l.created != nil {
		c = *l.created
	} else if c, err = r.createdForOpen(ctx, id); err != nil {
		session.Discard(own, id, r.log)
		return c, "", 0, err
	}
	if root, depth, err = r.tree.Lineage(ctx, id, c.ParentID); err != nil {
		session.Discard(own, id, r.log)
	}
	return c, root, depth, err
}

// modelRef resolves the optional model key ref of c through its aliases. An empty key gives no model.
func modelRef(c config.Config, ref string) string {
	if ref == "" {
		return ""
	}
	return c.ResolveModel(ref)
}

// checkOptions adds tools to the runtime and refuses a tool name that is empty,
// repeated, or reserved, a bad plugin, and a title_model that runs its own agent loop, as generate does.
func (r *Runtime) checkOptions(tools []turn.Tool, opts Options) error {
	for _, t := range tools {
		if name := t.Spec().Name; name == "" || r.known("", name) && (!r.shadowable(name) || r.owns(name)) {
			return fmt.Errorf("harness: tool name %q is empty, repeated, or reserved", name)
		}
		r.tools = append(r.tools, t)
	}
	if err := r.checkPlugins(opts.Config.Plugins, opts.Plugins); err != nil {
		return err
	}
	if r.titleModel != "" && r.models.Capabilities(r.titleModel).OwnsLoop {
		return fmt.Errorf("harness: title_model %s runs its own agent loop; use a model API provider", r.titleModel)
	}
	return nil
}
