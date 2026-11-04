package e2e

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

var checkpointRows = map[string]runtimeRow{
	"oversized_encoded_checkpoint_falls_back_to_replay":                sameAsServe(),
	"checkpoint_page_corrupt_source_replays_and_read_errors_propagate": sameAsServe(),
	"checkpoint_invalid_head_and_oversized_collection_fall_back":       sameAsServe(),
	"stopped_view_checkpoint_preserves_reads":                          sameAsServe(),
	"stopped_view_checkpoint_preserves_attachments":                    sameAsServe(),
	"checkpoint_compaction_and_command_history":                        sameAsServe(),
	"checkpoint_question_answer_history":                               sameAsServe(),
	"checkpoint_question_dismiss_history":                              sameAsServe(),
	"checkpoint_promoted_inputs_history":                               sameAsServe(),
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
	for i := 0; i < len(seqs); {
		if seqs[i] == 0 || i > 0 && seqs[i] == seqs[i-1] {
			i++
			continue
		}
		end := i + 1
		for end < len(seqs) && seqs[end] == seqs[end-1]+1 {
			end++
		}
		records, err := s.Store.Read(ctx, session, seqs[i]-1, end-i)
		if err != nil {
			return nil, err
		}
		out = append(out, records...)
		i = end
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

type replayTrackingStore struct {
	harness.Store
	fromStart bool
	fromTail  bool
}

func (s *replayTrackingStore) Read(ctx context.Context, id string, after uint64, limit int) ([]harness.Record, error) {
	if after == 0 {
		s.fromStart = true
	} else if after == 1 && !s.fromStart {
		s.fromTail = true
	}
	return s.Store.Read(ctx, id, after, limit)
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
	want, wantClock, err := openParityView(ctx, base, id)
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
	withoutSeqs := checkpointsWithoutSeqs{Store: base, checkpoint: st}
	withoutSeqView, err := harness.OpenView(ctx, withoutSeqs, id)
	if err != nil {
		t.Fatal(err)
	}
	checkViewParity(t, withoutSeqView, want)
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
	first := &checkpointStore{Store: prefixStore{Store: base, head: 1}}
	if _, err := harness.OpenView(ctx, first, id); err != nil {
		t.Fatal(err)
	}
	_, firstData, err := first.LoadCheckpoint(ctx, id)
	if err != nil || len(firstData) == 0 {
		t.Fatalf("first checkpoint: %d bytes, %v", len(firstData), err)
	}
	checkCorruptCheckpointState(t, base, id, firstData, want)
	checkCheckpointPrefixes(t, base, id, want, wantClock)
}

func checkMemCheckpointOrder(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	st := store.NewMemStore()
	if seq, data, err := st.LoadCheckpoint(ctx, "s1"); err != nil || seq != 0 || data != nil {
		t.Fatalf("missing checkpoint = %d, %q, %v", seq, data, err)
	}
	data := []byte("newer")
	if err := st.SaveCheckpoint(ctx, "s1", 10, data); err != nil {
		t.Fatal(err)
	}
	data[0] = 'X'
	for _, seq := range []uint64{4, 10} {
		if err := st.SaveCheckpoint(ctx, "s1", seq, []byte("older")); err != nil {
			t.Fatal(err)
		}
	}
	seq, loaded, err := st.LoadCheckpoint(ctx, "s1")
	if err != nil || seq != 10 || string(loaded) != "newer" {
		t.Fatalf("MemStore checkpoint after lower/equal save = %d, %q, %v; want 10, newer, nil", seq, loaded, err)
	}
	loaded[0] = 'X'
	if _, loaded, err := st.LoadCheckpoint(ctx, "s1"); err != nil || string(loaded) != "newer" {
		t.Fatalf("checkpoint changed after caller mutated loaded bytes: %q, %v", loaded, err)
	}
	checkMemReadSeqs(t, st)
	checkMemConcurrentCheckpoints(t, st)
}

func checkMemReadSeqs(t *testing.T, st *store.MemStore) {
	t.Helper()
	ctx := t.Context()
	if err := st.Append(ctx, "log", 0, []byte("one"), []byte("two"), []byte("three")); err != nil {
		t.Fatal(err)
	}
	got, err := st.ReadSeqs(ctx, "log", []uint64{3, 1, 3, 0, 9})
	want := []harness.Record{{Seq: 1, Data: []byte("one")}, {Seq: 3, Data: []byte("three")}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadSeqs = %v, %v; want %v", got, err, want)
	}
	got[0].Data[0] = 'X'
	again, err := st.ReadSeqs(ctx, "log", []uint64{1, 3})
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("ReadSeqs returned aliased bytes: %v, %v", again, err)
	}
}

func checkMemConcurrentCheckpoints(t *testing.T, st *store.MemStore) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for seq := uint64(1); seq <= 20; seq++ {
		wg.Go(func() { errs <- st.SaveCheckpoint(t.Context(), "concurrent", seq, []byte{byte(seq)}) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seq, data, err := st.LoadCheckpoint(t.Context(), "concurrent")
	if err != nil || seq != 20 || !reflect.DeepEqual(data, []byte{20}) {
		t.Fatalf("concurrent checkpoint = %d, %v, %v", seq, data, err)
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

func checkScenarioCheckpointParity(t *testing.T, dir string) {
	t.Helper()
	base := plainStore{store.NewDiskStore(dir)}
	ctx := t.Context()
	for after := ""; ; {
		ids, err := base.Sessions(ctx, after, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			checkScenarioSessionParity(t, base, id)
		}
		if len(ids) < 100 {
			return
		}
		after = ids[len(ids)-1]
	}
}

type snapshotLogStore struct {
	harness.Store
	blobs harness.Store
}

func (s snapshotLogStore) GetBlob(ctx context.Context, id, key string) (io.ReadCloser, error) {
	return s.blobs.GetBlob(ctx, id, key)
}

func snapshotScenarioLog(t *testing.T, source harness.Store, id string, head uint64) harness.Store {
	t.Helper()
	ctx := t.Context()
	memory := store.NewMemStore()
	for after := uint64(0); after < head; {
		records, err := source.Read(ctx, id, after, int(min(head-after, 512)))
		if err != nil || len(records) == 0 {
			t.Fatalf("snapshot log after %d: %d records, %v", after, len(records), err)
		}
		data := make([][]byte, len(records))
		for i, record := range records {
			if record.Seq != after+uint64(i)+1 {
				t.Fatalf("snapshot log: sequence %d after %d", record.Seq, after)
			}
			data[i] = record.Data
		}
		if err := memory.Append(ctx, id, after, data...); err != nil {
			t.Fatal(err)
		}
		after = records[len(records)-1].Seq
	}
	return plainStore{snapshotLogStore{Store: memory, blobs: source}}
}

func checkScenarioSessionParity(t *testing.T, base harness.Store, id string) {
	t.Helper()
	head, err := base.Head(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	fixed := prefixStore{Store: snapshotScenarioLog(t, base, id, head), head: head}
	want, wantClock, err := openParityView(t.Context(), fixed, id)
	if err != nil {
		t.Fatalf("full replay of %s: %v", id, err)
	}
	checkCheckpointPrefixes(t, fixed, id, want, wantClock)
}

func checkCheckpointPrefixes(t *testing.T, base harness.Store, id string, want *harness.View, wantClock viewClock) {
	t.Helper()
	ctx := t.Context()
	head := want.Snapshot().HeadSeq
	captures := syntheticCaptureBySeq(t, base, id, head)
	headPages := cacheViewReads(t, want, captures[head])
	for seq := uint64(1); seq <= head; seq++ {
		prefixBase := prefixStore{Store: base, head: seq}
		wantPrefix, prefixClock, err := openParityView(ctx, prefixBase, id)
		if err != nil {
			t.Fatalf("full replay of prefix %d: %v", seq, err)
		}
		prefix := &checkpointStore{Store: prefixBase}
		first, firstClock, err := openParityView(ctx, prefix, id)
		if err != nil {
			t.Fatalf("OpenView prefix %d: %v", seq, err)
		}
		prefixPages := cacheViewReads(t, wantPrefix, captures[seq])
		checkCachedViewParity(t, first, wantPrefix, prefixPages, firstClock, prefixClock)
		saved, data, err := prefix.LoadCheckpoint(ctx, id)
		if err != nil || saved != seq || len(data) == 0 {
			t.Fatalf("prefix %d checkpoint = %d, %d bytes, %v", seq, saved, len(data), err)
		}
		restored, restoredClock, err := openParityView(ctx, prefix, id)
		if err != nil {
			t.Fatalf("restore prefix %d: %v", seq, err)
		}
		checkCachedViewParity(t, restored, wantPrefix, prefixPages, restoredClock, prefixClock)
		cp := &checkpointStore{Store: base, seq: saved, data: data}
		v, gotClock, err := openParityView(ctx, cp, id)
		if err != nil {
			t.Fatalf("OpenView checkpoint %d: %v", seq, err)
		}
		checkCachedViewParity(t, v, want, headPages, gotClock, wantClock)
		cp.mu.Lock()
		reads := cp.seqReads
		cp.mu.Unlock()
		if headPages.pages[0].page.Total > 0 && reads == 0 {
			t.Fatalf("message pages did not use ReadSeqs with checkpoint at seq %d", seq)
		}
	}
}

type expectedPage struct {
	before uint64
	limit  int
	page   protocol.MessagePage
}

type expectedViewReads struct {
	pages            []expectedPage
	syntheticCapture int64
}

func cacheViewReads(t *testing.T, view *harness.View, capture ...int64) expectedViewReads {
	t.Helper()
	ctx := t.Context()
	var out expectedViewReads
	if len(capture) > 0 {
		out.syntheticCapture = capture[0]
	} else if view.Snapshot().SubscriptionUsage != nil {
		out.syntheticCapture = syntheticCaptureTime(t, view)
	}
	for _, limit := range []int{1, 2, protocol.MaxMessageLimit} {
		for before := uint64(0); ; {
			page, err := view.Messages(ctx, before, limit)
			if err != nil {
				t.Fatal(err)
			}
			out.pages = append(out.pages, expectedPage{before: before, limit: limit, page: page})
			if !page.HasMore {
				break
			}
			if page.FirstSeq == 0 || page.FirstSeq == before {
				t.Fatalf("message page did not advance before %d", before)
			}
			before = page.FirstSeq
		}
	}
	return out
}

type viewClock struct{ before, after int64 }

func openParityView(ctx context.Context, st harness.Store, id string) (*harness.View, viewClock, error) {
	before := time.Now().Unix()
	view, err := harness.OpenView(ctx, st, id)
	return view, viewClock{before: before, after: time.Now().Unix()}, err
}

func syntheticCaptureBySeq(t *testing.T, st harness.Store, id string, head uint64) []int64 {
	t.Helper()
	out := make([]int64, head+1)
	var costAt int64
	var recorded bool
	for event, err := range harness.ReadEvents(t.Context(), st, id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Seq > head {
			break
		}
		if event.Kind == "context.measured" {
			var measured protocol.ContextMeasured
			if err := json.Unmarshal(event.Data, &measured); err != nil {
				t.Fatal(err)
			}
			recorded = recorded || measured.SubscriptionUsage != nil
			if measured.CostUSD != nil {
				costAt = event.Time.Unix()
			}
		}
		if !recorded {
			out[event.Seq] = costAt
		}
	}
	return out
}

func syntheticCaptureTime(t *testing.T, view *harness.View) int64 {
	t.Helper()
	var costAt int64
	for event, err := range view.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind != "context.measured" {
			continue
		}
		var measured protocol.ContextMeasured
		if err := json.Unmarshal(event.Data, &measured); err != nil {
			t.Fatal(err)
		}
		if measured.SubscriptionUsage != nil {
			return 0
		}
		if measured.CostUSD != nil {
			costAt = event.Time.Unix()
		}
	}
	return costAt
}

func normalizeSyntheticCapture(t *testing.T, got, want *protocol.Session, costAt int64, clocks []viewClock) {
	t.Helper()
	if costAt == 0 || got.SubscriptionUsage == nil || want.SubscriptionUsage == nil {
		return
	}
	if len(clocks) != 2 {
		clocks = []viewClock{{before: costAt, after: time.Now().Unix()}, {before: costAt, after: time.Now().Unix()}}
	}
	for i, captured := range []int64{got.SubscriptionUsage.CapturedAt, want.SubscriptionUsage.CapturedAt} {
		if captured < clocks[i].before || captured > clocks[i].after {
			t.Fatalf("synthetic captured_at %d is outside view creation bounds [%d, %d]", captured, clocks[i].before, clocks[i].after)
		}
	}
	got.SubscriptionUsage.CapturedAt = 0
	want.SubscriptionUsage.CapturedAt = 0
}

func checkViewParity(t *testing.T, got, want *harness.View) {
	t.Helper()
	checkCachedViewParity(t, got, want, cacheViewReads(t, want))
}

func checkCachedViewParity(t *testing.T, got, want *harness.View, expected expectedViewReads, clocks ...viewClock) {
	t.Helper()
	ctx := t.Context()
	gotSnapshot, wantSnapshot := got.Snapshot(), want.Snapshot()
	normalizeSyntheticCapture(t, &gotSnapshot, &wantSnapshot, expected.syntheticCapture, clocks)
	if !reflect.DeepEqual(gotSnapshot, wantSnapshot) || got.Resumable() != want.Resumable() {
		gotJSON, _ := json.Marshal(got.Snapshot())
		wantJSON, _ := json.Marshal(want.Snapshot())
		t.Fatalf("checkpoint snapshot = %s, resumable=%v; replay = %s, resumable=%v", gotJSON, got.Resumable(), wantJSON, want.Resumable())
	}
	gotInputs, gotErr := got.Inputs(ctx)
	wantInputs, wantErr := want.Inputs(ctx)
	if !reflect.DeepEqual(gotInputs, wantInputs) || !reflect.DeepEqual(gotErr, wantErr) {
		t.Fatalf("checkpoint inputs = %v, %v; replay = %v, %v", gotInputs, gotErr, wantInputs, wantErr)
	}
	checkCachedPages(t, got, want, expected)
}

func checkCachedPages(t *testing.T, got, want *harness.View, expected expectedViewReads) {
	t.Helper()
	ctx := t.Context()
	for _, entry := range expected.pages {
		page, err := got.Messages(ctx, entry.before, entry.limit)
		if err != nil || !reflect.DeepEqual(page, entry.page) {
			t.Fatalf("messages before %d limit %d: checkpoint = %+v, %v; replay = %+v", entry.before, entry.limit, page, err, entry.page)
		}
		if entry.limit == protocol.MaxMessageLimit {
			for _, message := range entry.page.Messages {
				for _, part := range message.Parts {
					if part.Key != "" {
						checkBlobParity(t, got, want, part.Key)
					}
				}
			}
		}
	}
	if _, err := got.Blob(ctx, "missing-blob"); !errors.Is(err, harness.ErrBlobNotFound) {
		t.Fatalf("missing blob: %v, want ErrBlobNotFound", err)
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

func checkCorruptCheckpointState(t *testing.T, base harness.Store, id string, data []byte, want *harness.View) {
	t.Helper()
	if want.Snapshot().HeadSeq <= 1 {
		t.Fatal("corrupt checkpoint fixture needs a nonempty tail")
	}
	packed, body := unpackCheckpoint(t, data)
	pos := checkpointInputsOffset(t, body)
	for _, offset := range []int{0, pos} {
		changed := bytes.Clone(body)
		if offset == 0 {
			changed[offset] = 0
		} else {
			changed[offset] = 1
		}
		checkCorruptReplay(t, base, id, packCheckpoint(t, packed, changed), want, false)
	}
	var turnID string
	for event, err := range harness.ReadEvents(t.Context(), base, id, 1) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == "turn.started" {
			var started struct {
				TurnID string `json:"turn_id"`
			}
			if err := json.Unmarshal(event.Data, &started); err != nil {
				t.Fatal(err)
			}
			turnID = started.TurnID
			break
		}
	}
	if turnID == "" {
		t.Fatal("corrupt checkpoint fixture needs a turn in the tail")
	}
	changed := corruptUsedTurn(t, body, pos, turnID)
	checkCorruptReplay(t, base, id, packCheckpoint(t, packed, changed), want, true)
}

func unpackCheckpoint(t *testing.T, data []byte) ([]byte, []byte) {
	t.Helper()
	var wrapper struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatal(err)
	}
	packed, err := base64.StdEncoding.DecodeString(wrapper.Data)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(packed[4:]))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	return packed, body
}

type checkpointCursor struct {
	t    *testing.T
	body []byte
	pos  int
}

func (c *checkpointCursor) integer() int64 {
	c.t.Helper()
	v, n := binary.Varint(c.body[c.pos:])
	if n <= 0 {
		c.t.Fatal("invalid checkpoint fixture")
	}
	c.pos += n
	return v
}

func (c *checkpointCursor) text() {
	c.t.Helper()
	n := c.integer()
	if n < 0 || n > int64(len(c.body)-c.pos) {
		c.t.Fatal("invalid checkpoint string")
	}
	c.pos += int(n)
}

func checkpointInputsOffset(t *testing.T, body []byte) int {
	t.Helper()
	c := checkpointCursor{t: t, body: body, pos: 1}
	for range 10 {
		c.text()
	}
	if c.integer() != -1 || c.integer() != -1 {
		t.Fatal("unexpected checkpoint fixture")
	}
	c.integer()
	c.text()
	c.text()
	if body[c.pos] != 0 {
		t.Fatal("expected empty inputs map")
	}
	return c.pos
}

func corruptTurnID(t *testing.T, body []byte, inputs int) []byte {
	t.Helper()
	c := checkpointCursor{t: t, body: body, pos: inputs + 1}
	_, n := binary.Uvarint(body[c.pos:])
	if n <= 0 {
		t.Fatal("invalid checkpoint sequence")
	}
	c.pos += n
	c.text()
	c.text()
	if c.integer() != -1 || c.integer() != -1 {
		t.Fatal("expected empty progress lists")
	}
	if body[c.pos] != 0 || body[c.pos+1] != 0 {
		t.Fatal("expected empty progress pointers")
	}
	c.pos += 2
	if body[c.pos] != 0 {
		t.Fatal("expected no current turn")
	}
	changed := append(bytes.Clone(body[:c.pos]), 2, 'x')
	return append(changed, body[c.pos+1:]...)
}

func corruptUsedTurn(t *testing.T, body []byte, inputs int, turnID string) []byte {
	t.Helper()
	c := checkpointCursor{t: t, body: body, pos: inputs + 1}
	_, n := binary.Uvarint(body[c.pos:])
	if n <= 0 {
		t.Fatal("invalid checkpoint sequence")
	}
	c.pos += n
	c.text()
	c.text()
	if c.integer() != -1 || c.integer() != -1 {
		t.Fatal("expected empty progress lists")
	}
	if body[c.pos] != 0 || body[c.pos+1] != 0 {
		t.Fatal("expected empty progress pointers")
	}
	c.pos += 2
	c.text()
	if c.integer() != -1 {
		t.Fatal("expected empty turn input list")
	}
	if body[c.pos] != 0 {
		t.Fatal("expected unsuspended turn")
	}
	c.pos++
	c.integer()
	if body[c.pos] != 0 {
		t.Fatal("expected empty used-turn map")
	}
	changed := append(bytes.Clone(body[:c.pos]), 2)
	changed = binary.AppendVarint(changed, int64(len(turnID)))
	changed = append(changed, turnID...)
	changed = append(changed, 1)
	return append(changed, body[c.pos+1:]...)
}

func packCheckpoint(t *testing.T, packed, changed []byte) []byte {
	t.Helper()
	if len(changed) <= sha256.Size {
		t.Fatal("checkpoint has no state checksum")
	}
	sum := sha256.Sum256(changed[:len(changed)-sha256.Size])
	copy(changed[len(changed)-sha256.Size:], sum[:])
	var compressed bytes.Buffer
	compressed.Write(packed[:4])
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(changed); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(`{"version":` + strconv.Itoa(int(packed[3])) + `,"data":"` + base64.StdEncoding.EncodeToString(compressed.Bytes()) + `"}`)
}

func checkCorruptReplay(t *testing.T, base harness.Store, id string, data []byte, want *harness.View, tailFirst bool) {
	t.Helper()
	tracking := &replayTrackingStore{Store: base}
	st := &checkpointStore{Store: tracking, seq: 1, data: data}
	got, err := harness.OpenView(t.Context(), st, id)
	if err != nil {
		t.Fatalf("OpenView with invalid checkpoint state: %v", err)
	}
	if !tracking.fromStart || tailFirst && !tracking.fromTail {
		t.Fatalf("invalid checkpoint replay: from start=%v, tail first=%v", tracking.fromStart, tracking.fromTail)
	}
	checkViewParity(t, got, want)
}
