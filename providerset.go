package harness

import (
	"context"
	"errors"
	"fmt"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/backend"
	"github.com/majorcontext/harness/protocol"
)

// SetProviders makes providers the live set of configured provider entries. Config.Providers is the set at the start of the
// runtime, and the set does not persist. An entry whose config and credential are the same keeps its client, its websocket
// pool, and its catalog; an entry that is new or changed gets a new backend; an entry that providers leaves out takes no new
// call and closes when its calls in flight end. The next model call reads the new set. It returns each provider of the live
// set in name order. An entry that Validate rejects fails with ErrInvalidRequest and changes nothing.
func (r *Runtime) SetProviders(ctx context.Context, providers map[string]config.Provider) ([]protocol.ProviderStatus, error) {
	if err := r.hold(); err != nil {
		return nil, err
	}
	defer r.group.Done()
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	status, err := r.models.Set(providers)
	switch {
	case errors.Is(err, backend.ErrClosed):
		return nil, ErrDraining
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return status, nil
}

// ReconnectProvider moves the credential revision of the provider name: the idle pooled websockets of the entry close at once,
// a busy one closes when its call ends, and the next connection authenticates again. The client and the catalog stay. A name that
// the live set lacks, or a name with no backend, fails with ErrInvalidRequest.
func (r *Runtime) ReconnectProvider(ctx context.Context, name string) error {
	if err := r.hold(); err != nil {
		return err
	}
	defer r.group.Done()
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	switch err := r.models.Reconnect(name); {
	case errors.Is(err, backend.ErrClosed):
		return ErrDraining
	case err != nil:
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return nil
}
