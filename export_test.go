package harness

import "github.com/majorcontext/harness/internal/turn"

// NewWithBackend returns a Runtime that runs every turn on b.
func NewWithBackend(opts Options, b turn.Backend) (*Runtime, error) {
	opts.backend = b
	return New(opts)
}
