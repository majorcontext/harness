package harness

import (
	"context"

	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/protocol"
)

// ErrStaleEpoch reports a SyncBatch whose epoch is older than the receiver's.
var ErrStaleEpoch = session.ErrStaleEpoch

// ErrSyncRejected reports a SyncBatch that the receiver refuses for a reason
// that a resend cannot change: a bad request, a batch over the size bound, or
// a refused token.
var ErrSyncRejected = session.ErrRejected

// Sync replicates each session's records elsewhere, in seq order.
type Sync interface {
	// Deliver returns the receiver's head on success and on a seq mismatch;
	// the sender resends from Head+1. ErrStaleEpoch, ErrConflict, or
	// ErrSyncRejected stops the session and releases its Ownership. Any other error resends the batch.
	Deliver(ctx context.Context, b protocol.SyncBatch) (protocol.SyncAck, error)
}
