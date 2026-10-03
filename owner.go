package harness

import (
	"context"
	"errors"
	"sync"
)

// Owner grants the right to run a session.
type Owner interface {
	Acquire(ctx context.Context, session string) (Ownership, error)
}

// Ownership is the grant to run one session.
type Ownership interface {
	// Epoch orders the grants of a session. owner.acquired records it.
	Epoch() uint64
	// Lost closes when the grant ends without Release.
	Lost() <-chan struct{}
	Release()
}

// ErrBusy reports an Acquire of a session that a grant already holds.
var ErrBusy = errors.New("harness: session is busy")

// localOwner grants every session to this process, one grant at a time.
type localOwner struct {
	mu   sync.Mutex
	held map[string]bool
}

func newLocalOwner() *localOwner { return &localOwner{held: map[string]bool{}} }

func (o *localOwner) Acquire(ctx context.Context, session string) (Ownership, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.held[session] {
		return nil, ErrBusy
	}
	o.held[session] = true
	return &localGrant{owner: o, session: session}, nil
}

type localGrant struct {
	owner   *localOwner
	session string
	once    sync.Once
}

func (g *localGrant) Epoch() uint64 { return 1 }

func (g *localGrant) Lost() <-chan struct{} { return nil }

func (g *localGrant) Release() {
	g.once.Do(func() {
		g.owner.mu.Lock()
		defer g.owner.mu.Unlock()
		delete(g.owner.held, g.session)
	})
}
