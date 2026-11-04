package e2e

import (
	"bytes"
	"context"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/store"
)

type oversizedCheckpointStore struct {
	harness.Store
	seq  uint64
	data []byte
}

func (s oversizedCheckpointStore) LoadCheckpoint(context.Context, string) (uint64, []byte, error) {
	return s.seq, s.data, nil
}

func (s oversizedCheckpointStore) SaveCheckpoint(context.Context, string, uint64, []byte) error {
	return nil
}

type oversizedCheckpoint struct{ as string }

func TestContractOversizedCheckpoint(t *testing.T) {
	runScenarios(t, []scenario{{
		name:    "oversized_encoded_checkpoint_falls_back_to_replay",
		actions: []action{create{as: "a"}, restart{}, oversizedCheckpoint{as: "a"}},
	}})
}

func (a oversizedCheckpoint) run(t *testing.T, r *run) {
	t.Helper()
	ctx := t.Context()
	base := plainStore{store.NewDiskStore(r.drv.storeDir())}
	id := r.id(t, a.as)
	want, err := harness.OpenView(ctx, base, id)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := &checkpointStore{Store: base}
	if _, err := harness.OpenView(ctx, checkpoint, id); err != nil {
		t.Fatal(err)
	}
	seq, data, err := checkpoint.LoadCheckpoint(ctx, id)
	if testHasVCS() && (err != nil || seq != want.Snapshot().HeadSeq || len(data) == 0) {
		t.Fatalf("checkpoint = %d, %d bytes, %v", seq, len(data), err)
	}
	wrapper := bytes.Repeat([]byte{'A'}, ((128<<20)*4/3)+2049)
	tracking := &replayTrackingStore{Store: base}
	st := oversizedCheckpointStore{Store: tracking, seq: seq, data: wrapper}
	got, err := harness.OpenView(ctx, st, id)
	if err != nil || !tracking.fromStart {
		t.Fatalf("oversized checkpoint: %v, replay=%v", err, tracking.fromStart)
	}
	checkViewParity(t, got, want)
}
