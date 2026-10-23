// Package harness is the public Go API of the harness runtime.
package harness

import (
	"context"
	"io"

	"github.com/majorcontext/harness/internal/session"
)

// Store holds each session as an append-only log of opaque records.
// A record is non-empty and has no newline.
type Store interface {
	// Append writes records at expectedSeq+1.. or fails with ErrConflict. It is durable on return.
	Append(ctx context.Context, session string, expectedSeq uint64, records ...[]byte) error
	// Read returns up to limit records after afterSeq. A limit of 0 or less returns none.
	Read(ctx context.Context, session string, afterSeq uint64, limit int) ([]Record, error)
	// Head returns 0 for an unknown or empty session.
	Head(ctx context.Context, session string) (uint64, error)
	// Sessions returns up to limit non-empty session IDs above after, sorted.
	Sessions(ctx context.Context, after string, limit int) ([]string, error)
	PutBlob(ctx context.Context, session, key string, r io.Reader) error
	// GetBlob fails with an error matching fs.ErrNotExist for a missing key.
	GetBlob(ctx context.Context, session, key string) (io.ReadCloser, error)
}

// Record is one stored record. Seq starts at 1 and has no gaps.
type Record struct {
	Seq  uint64
	Data []byte
}

// ErrConflict reports an Append whose expectedSeq is not the session head,
// or a SyncBatch whose records differ from the records of the receiver.
var ErrConflict = session.ErrConflict

// firstRecord reads the first record of a session of st.
func firstRecord(st Store) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, id string) ([]byte, error) {
		recs, err := st.Read(ctx, id, 0, 1)
		if err != nil || len(recs) == 0 {
			return nil, err
		}
		return recs[0].Data, nil
	}
}
