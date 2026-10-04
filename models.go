package harness

import (
	"fmt"

	"github.com/majorcontext/harness/internal/backend"
	"github.com/majorcontext/harness/message"
)

// ErrModelUnavailable reports a model that no configured provider serves.
var ErrModelUnavailable = backend.ErrUnavailable

// checkModel reports why model cannot start a session that allows the
// names. A backend that owns the loop runs only its built-in tools and the
// tools that no model owns.
func (r *Runtime) checkModel(model string, names []string) error {
	ref, err := message.ParseModelRef(model)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	be, err := r.models.Check(ref)
	if err != nil {
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
		if !r.known(model, n) {
			return fmt.Errorf("%w: %s has no tool %q", ErrInvalidRequest, model, n)
		}
	}
	return nil
}

// changeModel reports why a session that allows the names cannot move to
// model to. A backend that owns its context reads the history of another
// provider through the history tool, so any two models may follow each other.
func (r *Runtime) changeModel(_, to string, names []string) error {
	return r.checkModel(to, names)
}
