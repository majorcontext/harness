package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/majorcontext/harness/internal/session"
)

const catchUpPage = 100

// catchGrant is the grant that CatchUp holds for one session. Open of that
// session cancels it and waits for done.
type catchGrant struct {
	cancel    context.CancelFunc
	done      chan struct{}
	preempted atomic.Bool
}

// CatchUp replicates every stored session through Sync and opens none: it
// appends no record, so an idle session gets no owner.acquired. A session
// that this runtime runs replicates itself and is skipped. Each session goes
// under the epoch of a grant that CatchUp takes and releases. Open of a
// session that CatchUp holds ends the catch-up of that session, and the
// opened session replicates itself; if that Open fails, CatchUp replicates the
// session. Two calls at once run one after the other. With no Sync it does nothing. A stale
// epoch ends it at once with ErrStaleEpoch, and Close ends it with
// ErrDraining. A session that fails for another reason, such as a log that
// cannot be read, is skipped; the other sessions finish, and CatchUp then
// returns every failure with its session ID.
func (r *Runtime) CatchUp(ctx context.Context) error {
	if r.sync == nil {
		return nil
	}
	if err := r.hold(); err != nil {
		return err
	}
	defer r.group.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(r.base, cancel)()
	defer context.AfterFunc(r.closing, cancel)()
	select {
	case r.catchSlot <- struct{}{}:
		defer func() { <-r.catchSlot }()
	case <-ctx.Done():
		return r.catchUpEnd(ctx, ctx.Err())
	}
	var failed []error
	for after := ""; ; {
		ids, err := r.store.Sessions(ctx, after, catchUpPage)
		if err != nil {
			return r.catchUpEnd(ctx, err)
		}
		for _, id := range ids {
			err := r.replicate(ctx, id)
			switch {
			case errors.Is(err, ErrStaleEpoch):
				return err
			case ctx.Err() != nil:
				return r.catchUpEnd(ctx, err)
			case err != nil:
				failed = append(failed, err)
			}
		}
		if len(ids) < catchUpPage {
			return errors.Join(failed...)
		}
		after = ids[len(ids)-1]
	}
}

func (r *Runtime) catchUpEnd(ctx context.Context, err error) error {
	if r.base.Err() != nil || r.closing.Err() != nil {
		return ErrDraining
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// replicate catches up session id. A session that this runtime runs
// replicates itself once its Open has succeeded; an Open that fails, or that
// ends the grant of catchUp and then fails, leaves the session to catchUp.
func (r *Runtime) replicate(ctx context.Context, id string) error {
	for {
		r.mu.Lock()
		e := r.sessions[id]
		r.mu.Unlock()
		if e != nil {
			select {
			case <-e.ready:
			case <-ctx.Done():
				return ctx.Err()
			}
			if e.err == nil {
				return nil
			}
		}
		again, err := r.catchUp(ctx, id)
		if !again {
			return err
		}
	}
}

// catchUp replicates session id under a grant of its own. again is true when
// a running or opening session took the session, and replicate must look
// again.
func (r *Runtime) catchUp(ctx context.Context, id string) (again bool, err error) {
	sctx, cancel := context.WithCancel(ctx)
	g := &catchGrant{cancel: cancel, done: make(chan struct{})}
	r.mu.Lock()
	if _, running := r.sessions[id]; running {
		r.mu.Unlock()
		cancel()
		return true, nil
	}
	if r.closed {
		r.mu.Unlock()
		cancel()
		return false, nil
	}
	r.catching[id] = g
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.catching, id)
		r.mu.Unlock()
		cancel()
		close(g.done)
	}()
	own, err := r.owner.Acquire(sctx, id)
	switch {
	case errors.Is(err, errHeld):
		return false, nil
	case err != nil && g.preempted.Load() && ctx.Err() == nil:
		return true, nil
	case err != nil:
		return false, fmt.Errorf("session %s: %w", id, err)
	}
	defer own.Release()
	err = session.CatchUp(sctx, storeLog{r.store, id}, r.sync, own.Epoch(), id)
	switch {
	case err == nil:
		return false, nil
	case g.preempted.Load() && ctx.Err() == nil:
		return true, nil
	case errors.Is(err, ErrStaleEpoch), ctx.Err() != nil:
		return false, err
	}
	err = fmt.Errorf("session %s: %w", id, err)
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrSyncRejected) {
		slog.Error("harness: catch-up stopped for a session", "session", id, "err", err)
	}
	return false, err
}

// yieldCatchUp ends the catch-up grant of session id, if any, and waits until
// it is released.
func (r *Runtime) yieldCatchUp(ctx context.Context, id string) error {
	r.mu.Lock()
	g := r.catching[id]
	r.mu.Unlock()
	if g == nil {
		return nil
	}
	g.preempted.Store(true)
	g.cancel()
	select {
	case <-g.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
