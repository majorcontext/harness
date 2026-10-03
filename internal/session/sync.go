package session

import (
	"context"
	"errors"
	"time"

	"github.com/majorcontext/harness/protocol"
)

// ErrStaleEpoch reports a SyncBatch whose epoch is older than the receiver's.
var ErrStaleEpoch = errors.New("harness: stale epoch")

// Sync replicates the records of a session elsewhere, in seq order.
type Sync interface {
	Deliver(ctx context.Context, b protocol.SyncBatch) (protocol.SyncAck, error)
}

const (
	minBackoff = 250 * time.Millisecond
	maxBackoff = 30 * time.Second
)

// replicate delivers each durable record to Sync in seq order. A failed
// delivery resends the same batch, so a lost ack meets a duplicate. It
// returns nil after the last record of a stopped actor is acknowledged, and
// ErrNotOwned when the ownership ends first. ErrStaleEpoch stops the actor.
func (a *Actor) replicate() error {
	var b *protocol.SyncBatch
	next, wait := uint64(1), minBackoff
	for !a.revoked() {
		var err error
		if b == nil {
			v := a.View()
			if next > v.Session.HeadSeq {
				if v.Stopped {
					return nil
				}
				await(a, v.changed)
				continue
			}
			b, err = a.batch(next, v.Session.HeadSeq)
		}
		var ack protocol.SyncAck
		if err == nil {
			ack, err = a.cfg.Sync.Deliver(a.cfg.Base, *b)
		}
		if errors.Is(err, ErrStaleEpoch) {
			close(a.stale)
			return ErrNotOwned
		}
		if err != nil {
			t := time.NewTimer(wait)
			await(a, t.C)
			t.Stop()
			wait = min(wait*2, maxBackoff)
			continue
		}
		a.synced.Store(ack.Head)
		b, next, wait = nil, ack.Head+1, minBackoff
	}
	return ErrNotOwned
}

func (a *Actor) batch(from, head uint64) (*protocol.SyncBatch, error) {
	recs, err := a.cfg.Log.Read(a.cfg.Base, from-1, int(min(head-from+1, page)))
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, errors.New("harness: log ends before the published head")
	}
	b := &protocol.SyncBatch{Epoch: a.cfg.Ownership.Epoch(), Session: a.cfg.ID, FromSeq: from}
	for _, r := range recs {
		b.Records = append(b.Records, r.Data)
	}
	return b, nil
}

// await waits for ch or for the ownership to end.
func await[T any](a *Actor, ch <-chan T) {
	select {
	case <-ch:
	case <-a.cfg.Ownership.Lost():
	case <-a.cfg.Base.Done():
	case <-a.stale:
	}
}

// Synced returns the receiver head of the last acknowledged SyncBatch.
func (a *Actor) Synced() uint64 { return a.synced.Load() }
