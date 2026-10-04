package harness

import (
	"context"
	"testing"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/internal/turn"
)

// NewWithBackend returns a Runtime that runs every turn on b.
func NewWithBackend(opts Options, b turn.Backend) (*Runtime, error) {
	opts.backend = b
	return New(opts)
}

// PanicIn makes the operation of the control command op panic until t ends.
func PanicIn(t testing.TB, o command.Op) {
	prior := ops[o]
	ops[o] = op{prior.method, prior.path, func(context.Context, *Session, map[string]any) (any, error) { panic("test panic") }}
	t.Cleanup(func() { ops[o] = prior })
}
