package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

// ErrStaleEpoch reports a SyncBatch whose epoch is older than the receiver's.
var ErrStaleEpoch = errors.New("harness: stale epoch")

// ErrRejected reports a SyncBatch that the receiver refuses for a reason that
// a resend cannot change, such as a malformed or oversized batch.
var ErrRejected = errors.New("harness: the Sync receiver rejected the batch")

// Sync replicates the records of a session elsewhere, in seq order. An
// error that matches ErrStaleEpoch, ErrConflict, or ErrRejected rejects the
// batch for good: a resend cannot change the answer. Any other error is retried.
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
// ErrNotOwned when the ownership ends first. A rejection stops the actor:
// ErrStaleEpoch returns ErrNotOwned; ErrConflict and ErrRejected return the
// rejection.
func (a *Actor) replicate() error {
	ctx, cancel := context.WithCancel(a.cfg.Base)
	defer cancel()
	go func() {
		select {
		case <-a.cfg.Ownership.Lost():
			cancel()
		case <-ctx.Done():
		}
	}()
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
			ack, err = a.cfg.Sync.Deliver(ctx, *b)
		}
		if errors.Is(err, ErrStaleEpoch) {
			close(a.rejected)
			return ErrNotOwned
		}
		if final(err) {
			close(a.rejected)
			slog.Error("harness: sync stopped for a session", "session", a.cfg.ID, "from_seq", b.FromSeq, "err", err)
			return fmt.Errorf("harness: the Sync receiver rejected seq %d of session %s: %w", b.FromSeq, a.cfg.ID, err)
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

// final reports whether err rejects a batch for good, other than a stale epoch.
func final(err error) bool {
	return errors.Is(err, ErrConflict) || errors.Is(err, ErrRejected)
}

func (a *Actor) batch(from, head uint64) (*protocol.SyncBatch, error) {
	return batch(a.cfg.Base, a.cfg.Store, a.cfg.Ownership.Epoch(), a.cfg.ID, from, head)
}

// maxBatchBytes bounds the JSON body of a SyncBatch. A receiver answers 413
// above it, so a sender never builds a larger batch from more than one record.
const maxBatchBytes = 32 << 20

// batch reads the records from..head of session id, at most one page and
// under maxBatchBytes, with the blobs that they name. It always returns the
// first record, so one record that is over the bound is the receiver's to
// refuse.
func batch(ctx context.Context, st Storage, epoch uint64, id string, from, head uint64) (*protocol.SyncBatch, error) {
	recs, err := st.Read(ctx, from-1, int(min(head-from+1, page)))
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, errors.New("harness: log ends before the published head")
	}
	b := &protocol.SyncBatch{Epoch: epoch, Session: id, FromSeq: from}
	size := jsonOverhead + len(id)*6
	for _, r := range recs {
		blobs, err := recordBlobs(ctx, st, r)
		if err != nil {
			return nil, err
		}
		cost := base64Len(len(r.Data))
		for k, v := range blobs {
			cost += len(k) + base64Len(len(v)) + jsonOverhead
		}
		if len(b.Records) > 0 && size+cost > maxBatchBytes {
			break
		}
		size += cost
		b.Records = append(b.Records, r.Data)
		for k, v := range blobs {
			if b.Blobs == nil {
				b.Blobs = map[string][]byte{}
			}
			b.Blobs[k] = v
		}
	}
	return b, nil
}

// jsonOverhead covers the quotes, colon, and comma that one JSON entry adds,
// with room for the envelope of the batch.
const jsonOverhead = 64

func base64Len(n int) int { return (n+2)/3*4 + jsonOverhead }

// CatchUp delivers the whole log of a stored session to sy, in pages from seq
// 1; each acknowledgement moves the next page to the head that sy reports. It
// appends nothing. It returns nil once sy holds every
// record, ErrStaleEpoch, ErrConflict, or ErrRejected when sy rejects a batch for good, and
// ctx.Err() when ctx ends first. Any other delivery error is resent with
// backoff.
func CatchUp(ctx context.Context, st Storage, sy Sync, epoch uint64, id string) error {
	head, err := st.Head(ctx)
	if err != nil {
		return err
	}
	next, wait := uint64(1), minBackoff
	for next <= head {
		b, err := batch(ctx, st, epoch, id, next, head)
		if err != nil {
			return err
		}
		ack, err := sy.Deliver(ctx, *b)
		switch {
		case errors.Is(err, ErrStaleEpoch), final(err):
			return err
		case err != nil:
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			}
			wait = min(wait*2, maxBackoff)
			continue
		}
		next, wait = ack.Head+1, minBackoff
	}
	return nil
}

// recordBlobs reads each blob that r points to. A state chunk never changes. A
// later save under the key of a one-blob state overwrites that blob, so the
// batch carries its newest content.
func recordBlobs(ctx context.Context, st Blobs, r eventlog.Record) (map[string][]byte, error) {
	env, err := eventlog.Decode(r.Data)
	if err != nil {
		return nil, err
	}
	var keys []string
	switch e := env.Event.(type) {
	case eventlog.BackendState:
		if key := cmp.Or(e.Chunk, e.BlobKey); key != "" {
			keys = []string{key}
		}
	case eventlog.ToolResultRetained:
		keys = []string{e.BlobKey}
	case eventlog.InputAdmitted:
		for _, p := range e.Parts {
			if p.Type == eventlog.PartBlob {
				keys = append(keys, p.BlobKey)
			}
		}
	}
	var blobs map[string][]byte
	for _, key := range keys {
		data, err := readBlob(ctx, st, key)
		if err != nil {
			return nil, err
		}
		if blobs == nil {
			blobs = map[string][]byte{}
		}
		blobs[key] = data
	}
	return blobs, nil
}

// await waits for ch or for the ownership to end.
func await[T any](a *Actor, ch <-chan T) {
	select {
	case <-ch:
	case <-a.cfg.Ownership.Lost():
	case <-a.cfg.Base.Done():
	case <-a.rejected:
	}
}

// Synced returns the receiver head of the last acknowledged SyncBatch.
func (a *Actor) Synced() uint64 { return a.synced.Load() }

// Rejected reports whether Sync rejected a batch for good, so that it lacks
// records of the session.
func (a *Actor) Rejected() bool {
	select {
	case <-a.rejected:
		return true
	default:
		return false
	}
}
