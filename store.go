// Package harness is the public Go API of the harness runtime.
package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
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

// ErrConflict reports an Append whose expectedSeq is not the session head.
var ErrConflict = errors.New("harness: append conflict")

func checkName(kind, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("harness: invalid %s %q", kind, name)
	}
	return nil
}

func checkRecords(records [][]byte) error {
	for _, r := range records {
		if len(r) == 0 || bytes.IndexByte(r, '\n') >= 0 {
			return errors.New("harness: record is empty or contains a newline")
		}
	}
	return nil
}
