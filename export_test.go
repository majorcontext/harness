package harness

import (
	"cmp"
	"context"
	"testing"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/internal/backend"
	"github.com/majorcontext/harness/internal/turn"
)

// NewWithBackend returns a Runtime whose provider fake runs every turn on b.
// An empty Config.Model is fake/model.
func NewWithBackend(opts Options, b turn.Backend) (*Runtime, error) {
	return NewWithBackends(opts, map[string]turn.Backend{"fake": b})
}

// NewWithBackends is NewWithBackend with one backend per provider name.
func NewWithBackends(opts Options, backends map[string]turn.Backend) (*Runtime, error) {
	opts.Config.Model = cmp.Or(opts.Config.Model, "fake/model")
	r, err := New(opts)
	if err != nil {
		return nil, err
	}
	r.models.Close()
	r.models = backend.NewRouter(backends, false)
	return r, nil
}

// PanicIn makes the operation of the control command op panic until t ends.
func PanicIn(t testing.TB, o command.Op) {
	prior := ops[o]
	ops[o] = func(context.Context, *Session, map[string]any) (any, error) { panic("test panic") }
	t.Cleanup(func() { ops[o] = prior })
}

// SpawnChild runs the admission and the child.spawned append of a spawn of
// child by the session id, which the runtime runs.
func (r *Runtime) SpawnChild(ctx context.Context, id, child, agent string) error {
	_, err := r.tree.SpawnChild(ctx, node(r.running(id)), child, agent)
	return err
}
