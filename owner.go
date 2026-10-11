package harness

import (
	"context"
	"sync"
)

// Owner grants sessions. Calls can be concurrent.
type Owner interface {
	// Acquire returns at once. For a held session it fails with an error that
	// matches ErrSessionHeld.
	Acquire(ctx context.Context, session string) (Ownership, error)
}

// Ownership is the grant to run one session. The runtime calls Release once.
type Ownership interface {
	// Epoch orders the grants of a session. owner.acquired and every SyncBatch carry it.
	Epoch() uint64
	// Lost closes when the grant ends without Release. A nil channel means never lost.
	Lost() <-chan struct{}
	// Release ends the grant. Its failure reaches the caller of Session.Release and Runtime.Close.
	Release(ctx context.Context) error
}

// NewLocalOwner returns an Owner that grants every session to this process at
// epoch, one grant at a time. A held session fails with ErrSessionHeld.
func NewLocalOwner(epoch uint64) Owner {
	return &localOwner{epoch: epoch, held: map[string]bool{}}
}

type localOwner struct {
	epoch uint64
	mu    sync.Mutex
	held  map[string]bool
}

func (o *localOwner) Acquire(ctx context.Context, session string) (Ownership, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.held[session] {
		return nil, ErrSessionHeld
	}
	o.held[session] = true
	return &localGrant{owner: o, session: session}, nil
}

type localGrant struct {
	owner   *localOwner
	session string
	once    sync.Once
}

func (g *localGrant) Epoch() uint64 { return g.owner.epoch }

func (g *localGrant) Lost() <-chan struct{} { return nil }

func (g *localGrant) Release(context.Context) error {
	g.once.Do(func() {
		g.owner.mu.Lock()
		defer g.owner.mu.Unlock()
		delete(g.owner.held, g.session)
	})
	return nil
}
