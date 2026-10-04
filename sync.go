package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"

	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/protocol"
)

// ErrStaleEpoch reports a SyncBatch whose epoch is older than the receiver's.
var ErrStaleEpoch = session.ErrStaleEpoch

// Sync replicates each session's records elsewhere, in seq order.
type Sync interface {
	// Deliver returns the receiver's head on success and on a seq mismatch;
	// the sender resends from Head+1. ErrStaleEpoch or ErrConflict stops the
	// session and releases its Ownership. Any other error resends the batch.
	Deliver(ctx context.Context, b protocol.SyncBatch) (protocol.SyncAck, error)
}

const epochBlob = "sync-epoch"

// ApplySync appends b to st by the receiver rules of a Sync. It rejects an
// older epoch with ErrStaleEpoch. It appends a batch at head+1. A batch at or
// below head is a retry: the same bytes return the head, and other bytes fail
// with ErrConflict. Any other batch returns the head, so the sender resends
// from head+1. The caller serializes calls for one session: the epoch check
// and the append are separate Store operations.
func ApplySync(ctx context.Context, st Store, b protocol.SyncBatch) (protocol.SyncAck, error) {
	if b.FromSeq == 0 {
		return protocol.SyncAck{}, fmt.Errorf("%w: from_seq is 0", ErrInvalidRequest)
	}
	if _, ok := b.Blobs[epochBlob]; ok {
		return protocol.SyncAck{}, fmt.Errorf("%w: blob key %q is reserved", ErrInvalidRequest, epochBlob)
	}
	if err := fenceEpoch(ctx, st, b.Session, b.Epoch); err != nil {
		return protocol.SyncAck{}, err
	}
	head, err := st.Head(ctx, b.Session)
	if err != nil {
		return protocol.SyncAck{}, err
	}
	last := b.FromSeq + uint64(len(b.Records)) - 1
	switch {
	case len(b.Records) > 0 && b.FromSeq == head+1:
		for k, v := range b.Blobs {
			if err := st.PutBlob(ctx, b.Session, k, bytes.NewReader(v)); err != nil {
				return protocol.SyncAck{}, err
			}
		}
		if err := st.Append(ctx, b.Session, head, b.Records...); err != nil {
			return protocol.SyncAck{}, err
		}
		return protocol.SyncAck{Head: last}, nil
	case last <= head:
		return protocol.SyncAck{Head: head}, sameRecords(ctx, st, b)
	}
	return protocol.SyncAck{Head: head}, nil
}

// fenceEpoch fails with ErrStaleEpoch below the stored epoch and stores a newer one.
func fenceEpoch(ctx context.Context, st Store, id string, epoch uint64) error {
	var stored uint64
	rc, err := st.GetBlob(ctx, id, epochBlob)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
		if stored, err = strconv.ParseUint(string(data), 10, 64); err != nil {
			return fmt.Errorf("harness: session %s: sync epoch: %w", id, err)
		}
	}
	switch {
	case epoch < stored:
		return fmt.Errorf("%w: session %s: epoch %d is older than %d", ErrStaleEpoch, id, epoch, stored)
	case epoch > stored:
		return st.PutBlob(ctx, id, epochBlob, bytes.NewReader(strconv.AppendUint(nil, epoch, 10)))
	}
	return nil
}

func sameRecords(ctx context.Context, st Store, b protocol.SyncBatch) error {
	for i := 0; i < len(b.Records); {
		got, err := st.Read(ctx, b.Session, b.FromSeq-1+uint64(i), len(b.Records)-i)
		if err != nil {
			return err
		}
		if len(got) == 0 {
			return fmt.Errorf("harness: session %s: log ends before seq %d", b.Session, b.FromSeq+uint64(i))
		}
		for _, r := range got {
			if !bytes.Equal(r.Data, b.Records[i]) {
				return fmt.Errorf("%w: session %s: seq %d differs from the retry", ErrConflict, b.Session, r.Seq)
			}
			i++
		}
	}
	return nil
}
