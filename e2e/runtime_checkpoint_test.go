package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

var checkpointRows = map[string]runtimeRow{
	"stopped_view_checkpoint_preserves_reads":       sameAsServe(),
	"stopped_view_checkpoint_preserves_attachments": sameAsServe(),
}

type checkpointStore struct {
	harness.Store
	mu       sync.Mutex
	seq      uint64
	data     []byte
	seqReads int
}

func (s *checkpointStore) LoadCheckpoint(_ context.Context, _ string) (uint64, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq, append([]byte(nil), s.data...), nil
}

func (s *checkpointStore) SaveCheckpoint(_ context.Context, _ string, seq uint64, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq > s.seq {
		s.seq, s.data = seq, append([]byte(nil), data...)
	}
	return nil
}

func (s *checkpointStore) ReadSeqs(ctx context.Context, session string, seqs []uint64) ([]harness.Record, error) {
	s.mu.Lock()
	s.seqReads++
	s.mu.Unlock()
	seqs = append([]uint64(nil), seqs...)
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	var out []harness.Record
	for i, seq := range seqs {
		if seq == 0 || i > 0 && seq == seqs[i-1] {
			continue
		}
		records, err := s.Store.Read(ctx, session, seq-1, 1)
		if err != nil {
			return nil, err
		}
		if len(records) != 0 && records[0].Seq == seq {
			out = append(out, records[0])
		}
	}
	return out, nil
}

type plainStore struct{ harness.Store }

type prefixStore struct {
	harness.Store
	head uint64
}

func (s prefixStore) Head(context.Context, string) (uint64, error) { return s.head, nil }

func (s prefixStore) Read(ctx context.Context, id string, after uint64, limit int) ([]harness.Record, error) {
	if after >= s.head || limit <= 0 {
		return nil, nil
	}
	return s.Store.Read(ctx, id, after, min(limit, int(s.head-after)))
}

type checkpointsWithoutSeqs struct {
	harness.Store
	checkpoint *checkpointStore
}

func (s checkpointsWithoutSeqs) LoadCheckpoint(ctx context.Context, id string) (uint64, []byte, error) {
	return s.checkpoint.LoadCheckpoint(ctx, id)
}

func (s checkpointsWithoutSeqs) SaveCheckpoint(ctx context.Context, id string, seq uint64, data []byte) error {
	return s.checkpoint.SaveCheckpoint(ctx, id, seq, data)
}

type checkpointView struct{ as string }

func TestContractStoppedViewCheckpoint(t *testing.T) {
	runScenarios(t, []scenario{{
		name: "stopped_view_checkpoint_preserves_reads",
		model: []harnesstest.Step{
			{Name: "one", Match: harnesstest.LastUserText("one"), Reply: harnesstest.Reply{Text: "first"}},
			{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: "second"}},
		},
		actions: []action{create{as: "a"}, submit{as: "a", text: "one"}, waitIdle{as: "a"}, submit{as: "a", text: "two"}, waitIdle{as: "a"}, restart{}, checkpointView{as: "a"}},
	}, {
		name:    "stopped_view_checkpoint_preserves_attachments",
		model:   []harnesstest.Step{{Name: "one", Reply: harnesstest.Reply{Text: "seen"}}},
		actions: []action{create{as: "a"}, submitAttachments{as: "a", text: "look"}, waitIdle{as: "a"}, restart{}, checkpointView{as: "a"}},
	}})
}

func (a checkpointView) run(t *testing.T, r *run) {
	t.Helper()
	ctx := t.Context()
	checkMemCheckpointOrder(t)
	base := plainStore{store.NewDiskStore(r.drv.(*runtimeDriver).store)}
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
	if err != nil {
		t.Fatal(err)
	}
	if seq == 0 || len(data) == 0 {
		t.Errorf("OpenView did not save a checkpoint for the stopped session: seq=%d bytes=%d", seq, len(data))
		return
	}
	got, err := harness.OpenView(ctx, st, id)
	if err != nil {
		t.Fatal(err)
	}
	checkViewParity(t, got, want)
	if err := st.SaveCheckpoint(ctx, id, seq-1, []byte("older")); err != nil {
		t.Fatal(err)
	}
	if after, bytes, err := st.LoadCheckpoint(ctx, id); err != nil || after != seq || !reflect.DeepEqual(bytes, data) {
		t.Fatalf("lower-seq save replaced checkpoint: seq=%d bytes=%q err=%v", after, bytes, err)
	}
	st.mu.Lock()
	st.data = []byte("corrupt checkpoint")
	st.mu.Unlock()
	fallback, err := harness.OpenView(ctx, st, id)
	if err != nil {
		t.Fatalf("OpenView with corrupt checkpoint: %v", err)
	}
	checkViewParity(t, fallback, want)
	checkCheckpointVersions(t, st, id, seq, data, want)
	checkCheckpointPrefixes(t, base, id, want)
}

func checkMemCheckpointOrder(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	st := store.NewMemStore()
	if err := st.SaveCheckpoint(ctx, "s1", 10, []byte("newer")); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCheckpoint(ctx, "s1", 4, []byte("older")); err != nil {
		t.Fatal(err)
	}
	seq, data, err := st.LoadCheckpoint(ctx, "s1")
	if err != nil || seq != 10 || string(data) != "newer" {
		t.Fatalf("MemStore checkpoint after lower-seq save = %d, %q, %v; want 10, newer, nil", seq, data, err)
	}
}

func checkCheckpointVersions(t *testing.T, st *checkpointStore, id string, seq uint64, data []byte, want *harness.View) {
	t.Helper()
	var encoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &encoded); err != nil || encoded["version"] == nil {
		t.Fatalf("checkpoint has no readable version: %v", err)
	}
	encoded["version"] = json.RawMessage("999999")
	unknown, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.seq, st.data = seq, unknown
	st.mu.Unlock()
	v, err := harness.OpenView(t.Context(), st, id)
	if err != nil {
		t.Fatalf("OpenView with unsupported checkpoint version: %v", err)
	}
	checkViewParity(t, v, want)
}

func checkCheckpointPrefixes(t *testing.T, base harness.Store, id string, want *harness.View) {
	t.Helper()
	ctx := t.Context()
	head := want.Snapshot().HeadSeq
	for seq := uint64(1); seq <= head; seq++ {
		prefix := &checkpointStore{Store: prefixStore{Store: base, head: seq}}
		if _, err := harness.OpenView(ctx, prefix, id); err != nil {
			t.Fatalf("OpenView prefix %d: %v", seq, err)
		}
		saved, data, err := prefix.LoadCheckpoint(ctx, id)
		if err != nil || saved != seq || len(data) == 0 {
			t.Fatalf("prefix %d checkpoint = %d, %d bytes, %v", seq, saved, len(data), err)
		}
		for _, fetch := range []bool{false, true} {
			cp := &checkpointStore{Store: base, seq: saved, data: data}
			var st harness.Store = cp
			if !fetch {
				st = checkpointsWithoutSeqs{Store: base, checkpoint: cp}
			}
			v, err := harness.OpenView(ctx, st, id)
			if err != nil {
				t.Fatalf("OpenView checkpoint %d, ReadSeqs=%v: %v", seq, fetch, err)
			}
			checkViewParity(t, v, want)
			if fetch {
				cp.mu.Lock()
				reads := cp.seqReads
				cp.mu.Unlock()
				if reads == 0 {
					t.Fatalf("message pages did not use ReadSeqs with checkpoint at seq %d", seq)
				}
			}
		}
	}
}

func checkViewParity(t *testing.T, got, want *harness.View) {
	t.Helper()
	ctx := t.Context()
	if !reflect.DeepEqual(got.Snapshot(), want.Snapshot()) || got.Resumable() != want.Resumable() {
		t.Fatalf("checkpoint view snapshot or resumability differs from full replay")
	}
	gotInputs, gotErr := got.Inputs(ctx)
	wantInputs, wantErr := want.Inputs(ctx)
	if !reflect.DeepEqual(gotInputs, wantInputs) || !reflect.DeepEqual(gotErr, wantErr) {
		t.Fatalf("checkpoint inputs = %v, %v; replay = %v, %v", gotInputs, gotErr, wantInputs, wantErr)
	}
	head := want.Snapshot().HeadSeq
	page, err := want.Messages(ctx, 0, protocol.MaxMessageLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range page.Messages {
		for _, part := range message.Parts {
			if part.Key != "" {
				checkBlobParity(t, got, want, part.Key)
			}
		}
	}
	if _, err := got.Blob(ctx, "missing-blob"); !errors.Is(err, harness.ErrBlobNotFound) {
		t.Fatalf("missing blob: %v, want ErrBlobNotFound", err)
	}
	for before := uint64(0); before <= head+1; before++ {
		for _, limit := range []int{1, 2, protocol.MaxMessageLimit} {
			gp, ge := got.Messages(ctx, before, limit)
			wp, we := want.Messages(ctx, before, limit)
			if !reflect.DeepEqual(gp, wp) || !reflect.DeepEqual(ge, we) {
				t.Fatalf("messages before %d limit %d: checkpoint = %+v, %v; replay = %+v, %v", before, limit, gp, ge, wp, we)
			}
		}
		if ge, we := collectEvents(t, got.Events(ctx, before)), collectEvents(t, want.Events(ctx, before)); !reflect.DeepEqual(ge, we) {
			t.Fatalf("events after %d differ between checkpoint and replay", before)
		}
	}
}

func checkBlobParity(t *testing.T, got, want *harness.View, key string) {
	t.Helper()
	gb, ge := got.Blob(t.Context(), key)
	wb, we := want.Blob(t.Context(), key)
	if ge != nil || we != nil {
		t.Fatalf("Blob(%q): checkpoint=%v replay=%v", key, ge, we)
	}
	defer gb.Body.Close()
	defer wb.Body.Close()
	gd, ge := io.ReadAll(gb.Body)
	wd, we := io.ReadAll(wb.Body)
	if ge != nil || we != nil || gb.MediaType != wb.MediaType || gb.Size != wb.Size || !reflect.DeepEqual(gd, wd) {
		t.Fatalf("Blob(%q) differs between checkpoint and replay", key)
	}
}
