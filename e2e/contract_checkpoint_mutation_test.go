package e2e

import (
	"bytes"
	"context"
	"testing"

	"github.com/majorcontext/harness"
)

type invalidTailStore struct {
	*replayTrackingStore
	failed bool
}

func (s *invalidTailStore) Read(ctx context.Context, id string, after uint64, limit int) ([]harness.Record, error) {
	if after == 1 && !s.failed {
		s.failed = true
		s.fromTail = true
		return nil, harness.ErrUnreplayable
	}
	return s.replayTrackingStore.Read(ctx, id, after, limit)
}

func checkCorruptCheckpointState(t *testing.T, base harness.Store, id string, data []byte, want *harness.View) {
	t.Helper()
	changed := bytes.Clone(data)
	changed[len(changed)/2] ^= 1
	checkCorruptReplay(t, base, id, changed, want, false)
	tracking := &invalidTailStore{replayTrackingStore: &replayTrackingStore{Store: base}}
	st := &checkpointStore{Store: tracking, seq: 1, data: data}
	got, err := harness.OpenView(t.Context(), st, id)
	if err != nil || !tracking.fromStart || !tracking.fromTail {
		t.Fatalf("invalid checkpoint tail: %v, tail=%v replay=%v", err, tracking.fromTail, tracking.fromStart)
	}
	checkViewParity(t, got, want)
}
