package harness

import (
	"cmp"
	"context"
	"testing"

	"github.com/majorcontext/harness/internal/backend"
	"github.com/majorcontext/harness/internal/command"
	"github.com/majorcontext/harness/internal/turn"
)

// NewWithBackend returns a Runtime whose provider fake runs every turn on b.
// An empty Config.Model is fake/model.
func NewWithBackend(opts Options, b turn.Backend) (*Runtime, error) {
	opts.Config.Model = cmp.Or(opts.Config.Model, "fake/model")
	r, err := New(opts)
	if err != nil {
		return nil, err
	}
	r.models.Close()
	r.models = backend.NewRouter(map[string]turn.Backend{"fake": b}, false)
	return r, nil
}

// PanicIn makes the operation of the control command op panic until t ends.
func PanicIn(t testing.TB, o command.Op) {
	prior := ops[o]
	ops[o] = func(context.Context, *Session, map[string]any) (any, error) { panic("test panic") }
	t.Cleanup(func() { ops[o] = prior })
}
