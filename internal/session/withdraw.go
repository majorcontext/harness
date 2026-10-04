package session

import (
	"context"

	"github.com/majorcontext/harness/internal/eventlog"
)

// Withdraw appends input.withdrawn for input inputID when it is still queued.
// An input that was promoted, was withdrawn, or never existed appends nothing
// and returns nil.
func (a *Actor) Withdraw(ctx context.Context, inputID string) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		for _, in := range a.state.Queue() {
			if in.InputID == inputID {
				reply(struct{}{}, a.append(eventlog.InputWithdrawn{InputID: inputID}))
				return
			}
		}
		reply(struct{}{}, nil)
	})
	return err
}
