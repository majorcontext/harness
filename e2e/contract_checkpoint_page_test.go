package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/store"
)

var pageReadFailure = errors.New("page read failure")

type pageFailureStore struct{ *checkpointStore }

func (s pageFailureStore) ReadSeqs(ctx context.Context, _ string, _ []uint64) ([]harness.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, pageReadFailure
}

type checkpointPageFallback struct{ as string }

func TestContractCheckpointPageFallback(t *testing.T) {
	runScenarios(t, []scenario{{
		name:    "checkpoint_page_corrupt_source_replays_and_read_errors_propagate",
		model:   []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: "answer"}}},
		actions: []action{create{as: "a"}, submit{as: "a", text: "hello"}, waitIdle{as: "a"}, restart{}, checkpointPageFallback{as: "a"}},
	}})
}

func (a checkpointPageFallback) run(t *testing.T, r *run) {
	t.Helper()
	ctx := t.Context()
	base := plainStore{store.NewDiskStore(r.drv.storeDir())}
	id := r.id(t, a.as)
	want, err := harness.OpenView(ctx, base, id)
	if err != nil {
		t.Fatal(err)
	}
	page, err := want.Messages(ctx, 0, 1)
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("replay page: %+v %v", page, err)
	}
	initial := &checkpointStore{Store: base}
	if _, err := harness.OpenView(ctx, initial, id); err != nil {
		t.Fatal(err)
	}
	seq, data, err := initial.LoadCheckpoint(ctx, id)
	if err != nil || seq != want.Snapshot().HeadSeq {
		t.Fatalf("head checkpoint: %d %v", seq, err)
	}
	packed, body := unpackCheckpoint(t, data)
	var source uint64
	for event, err := range harness.ReadEvents(ctx, base, id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == "item.completed" && event.Time.Equal(page.Messages[0].CreatedAt) {
			source = event.Seq
		}
	}
	changed := corruptPageSource(t, body, source, page.Messages[0].CreatedAt)
	tracking := &replayTrackingStore{Store: base}
	corrupt := &checkpointStore{Store: tracking, seq: seq, data: packCheckpoint(t, packed, changed)}
	view, err := harness.OpenView(ctx, corrupt, id)
	if err != nil || tracking.fromStart {
		t.Fatalf("restore corrupt source: %v, replay=%v", err, tracking.fromStart)
	}
	got, err := view.Messages(ctx, 0, 1)
	if err != nil || !reflect.DeepEqual(got, page) || !tracking.fromStart {
		t.Fatalf("corrupt page: %+v, %v, replay=%v; want %+v", got, err, tracking.fromStart, page)
	}
	checkPageErrors(t, base, id, seq, data)
}

func corruptPageSource(t *testing.T, body []byte, seq uint64, at time.Time) []byte {
	t.Helper()
	stamp, err := at.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	pattern := binary.AppendUvarint(nil, seq)
	pattern = binary.AppendVarint(pattern, 1)
	prefix := len(pattern)
	pattern = binary.AppendUvarint(pattern, seq)
	pattern = binary.AppendVarint(pattern, int64(len(stamp)))
	pattern = append(pattern, stamp...)
	if seq <= 1 || seq >= 128 || bytes.Count(body, pattern) != 1 {
		t.Fatalf("message source pattern not unique for seq %d", seq)
	}
	changed := bytes.Clone(body)
	changed[bytes.Index(body, pattern)+prefix] = 1
	return changed
}

func checkPageErrors(t *testing.T, base harness.Store, id string, seq uint64, data []byte) {
	t.Helper()
	tracking := &replayTrackingStore{Store: base}
	st := pageFailureStore{&checkpointStore{Store: tracking, seq: seq, data: data}}
	v, err := harness.OpenView(t.Context(), st, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Messages(t.Context(), 0, 1); !errors.Is(err, pageReadFailure) || tracking.fromStart {
		t.Fatalf("page store failure = %v, replay=%v", err, tracking.fromStart)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := v.Messages(ctx, 0, 1); !errors.Is(err, context.Canceled) || tracking.fromStart {
		t.Fatalf("canceled page = %v, replay=%v", err, tracking.fromStart)
	}
}
