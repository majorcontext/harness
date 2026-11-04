package e2e

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/store"
)

type malformedHeadCheckpoint struct{ as string }

func TestContractMalformedHeadCheckpoint(t *testing.T) {
	runScenarios(t, []scenario{{
		name:    "checkpoint_invalid_head_and_oversized_collection_fall_back",
		actions: []action{create{as: "a"}, restart{}, malformedHeadCheckpoint{as: "a"}},
	}})
}

func (a malformedHeadCheckpoint) run(t *testing.T, r *run) {
	t.Helper()
	ctx := t.Context()
	base := plainStore{store.NewDiskStore(r.drv.storeDir())}
	id := r.id(t, a.as)
	want, err := harness.OpenView(ctx, base, id)
	if err != nil {
		t.Fatal(err)
	}
	st := &checkpointStore{Store: base}
	if _, err := harness.OpenView(ctx, st, id); err != nil {
		t.Fatal(err)
	}
	seq, data, err := st.LoadCheckpoint(ctx, id)
	if err != nil || seq != want.Snapshot().HeadSeq || len(data) == 0 {
		t.Fatalf("head checkpoint = %d, %d bytes, %v", seq, len(data), err)
	}
	packed, body := unpackCheckpoint(t, data)
	pos := checkpointInputsOffset(t, body)
	turn := corruptTurnID(t, body, pos)
	count := append(bytes.Clone(body[:pos]), binary.AppendVarint(nil, 1<<20)...)
	count = append(count, body[pos+1:]...)
	for name, changed := range map[string][]byte{"idle with current turn": turn, "oversized input count": count} {
		tracking := &replayTrackingStore{Store: base}
		corrupt := &checkpointStore{Store: tracking, seq: seq, data: packCheckpoint(t, packed, changed)}
		got, err := harness.OpenView(ctx, corrupt, id)
		if err != nil {
			t.Fatalf("%s: OpenView: %v", name, err)
		}
		if !tracking.fromStart {
			t.Fatalf("%s: did not fall back to replay", name)
		}
		checkViewParity(t, got, want)
	}
}
