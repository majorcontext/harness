package harness_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

func recs(data ...string) [][]byte {
	out := make([][]byte, len(data))
	for i, d := range data {
		out[i] = []byte(d)
	}
	return out
}

func rawLog(t *testing.T, st harness.Store) []string {
	t.Helper()
	got, err := st.Read(bg, "s1", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range got {
		out = append(out, string(r.Data))
	}
	return out
}

func TestApplySync(t *testing.T) {
	for _, tc := range []struct {
		name    string
		b       protocol.SyncBatch
		want    uint64
		wantErr error
		log     []string
	}{
		{"a batch at head+1 is appended with its blobs",
			protocol.SyncBatch{Epoch: 2, FromSeq: 3, Records: recs("r3", "r4"), Blobs: map[string][]byte{"k": []byte("v")}},
			4, nil, []string{"r1", "r2", "r3", "r4"}},
		{"a newer epoch is accepted", protocol.SyncBatch{Epoch: 3, FromSeq: 3, Records: recs("r3")},
			3, nil, []string{"r1", "r2", "r3"}},
		{"a retry with the same bytes is a duplicate", protocol.SyncBatch{Epoch: 2, FromSeq: 1, Records: recs("r1", "r2")},
			2, nil, []string{"r1", "r2"}},
		{"a retry with other bytes is rejected", protocol.SyncBatch{Epoch: 2, FromSeq: 2, Records: recs("x2")},
			0, harness.ErrConflict, []string{"r1", "r2"}},
		{"a gap is a seq mismatch", protocol.SyncBatch{Epoch: 2, FromSeq: 4, Records: recs("r4")},
			2, nil, []string{"r1", "r2"}},
		{"an overlap is a seq mismatch", protocol.SyncBatch{Epoch: 2, FromSeq: 2, Records: recs("r2", "r3")},
			2, nil, []string{"r1", "r2"}},
		{"a batch that carries the reserved epoch key is rejected",
			protocol.SyncBatch{Epoch: 3, FromSeq: 3, Records: recs("r3"), Blobs: map[string][]byte{"sync-epoch": []byte("1")}},
			0, harness.ErrInvalidRequest, []string{"r1", "r2"}},
		{"an older epoch is stale", protocol.SyncBatch{Epoch: 1, FromSeq: 3, Records: recs("r3")},
			0, harness.ErrStaleEpoch, []string{"r1", "r2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := harness.NewMemStore()
			seed := protocol.SyncBatch{Epoch: 2, Session: "s1", FromSeq: 1, Records: recs("r1", "r2")}
			if ack, err := harness.ApplySync(bg, st, seed); err != nil || ack.Head != 2 {
				t.Fatalf("seed ApplySync = %+v, %v", ack, err)
			}
			tc.b.Session = "s1"
			ack, err := harness.ApplySync(bg, st, tc.b)
			if !errors.Is(err, tc.wantErr) || (err == nil) != (tc.wantErr == nil) {
				t.Fatalf("ApplySync error = %v, want %v", err, tc.wantErr)
			}
			if err == nil && ack.Head != tc.want {
				t.Fatalf("ApplySync = %+v, want head %d", ack, tc.want)
			}
			if got := rawLog(t, st); !slices.Equal(got, tc.log) {
				t.Fatalf("receiver log = %q, want %q", got, tc.log)
			}
			for k, v := range tc.b.Blobs {
				if err != nil {
					break
				}
				rc, err := st.GetBlob(bg, "s1", k)
				if err != nil {
					t.Fatal(err)
				}
				got, _ := io.ReadAll(rc)
				if string(got) != string(v) {
					t.Fatalf("blob %s = %q, want %q", k, got, v)
				}
			}
		})
	}
}

// replica is a Sync that applies each batch to st with ApplySync. The next
// fault applies to one delivery: "lose" applies the batch and fails, "drop"
// acknowledges the batch without applying it, and "hold" waits for hold.
type replica struct {
	st     *harness.MemStore
	hold   chan struct{}
	mu     sync.Mutex
	faults []string
	got    []string
}

func (r *replica) Deliver(ctx context.Context, b protocol.SyncBatch) (protocol.SyncAck, error) {
	r.mu.Lock()
	var fault string
	if len(r.faults) > 0 {
		fault, r.faults = r.faults[0], r.faults[1:]
	}
	r.mu.Unlock()
	if fault == "hold" {
		select {
		case <-r.hold:
		case <-ctx.Done():
			return protocol.SyncAck{}, ctx.Err()
		}
	}
	ack, err := protocol.SyncAck{Head: b.FromSeq + uint64(len(b.Records)) - 1}, error(nil)
	if fault != "drop" {
		ack, err = harness.ApplySync(ctx, r.st, b)
	}
	if fault == "lose" && err == nil {
		err = errors.New("lost")
	}
	note := fmt.Sprintf("%d+%d ack %d", b.FromSeq, len(b.Records), ack.Head)
	if err != nil {
		note = fmt.Sprintf("%d+%d %v", b.FromSeq, len(b.Records), err)
	}
	r.mu.Lock()
	r.got = append(r.got, note)
	r.mu.Unlock()
	return ack, err
}

func (r *replica) notes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

func syncRuntime(t *testing.T, st harness.Store, rep *replica, f *fake) *harness.Runtime {
	t.Helper()
	r, err := harness.NewWithBackend(harness.Options{Store: st, Sync: rep}, f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSyncReplicatesTheLog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		faults []string
		want   []string
	}{
		{"records arrive in seq order", nil, []string{"1+2 ack 2", "3+2 ack 4", "5+1 ack 5"}},
		{"a lost ack is retried and acknowledged as a duplicate", []string{"lose"},
			[]string{"1+2 lost", "1+2 ack 2", "3+3 ack 5"}},
		{"a receiver that missed records heals from its head", []string{"drop"},
			[]string{"1+2 ack 2", "3+2 ack 0", "1+4 ack 4", "5+1 ack 5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st, f, rep := harness.NewMemStore(), newFake(), &replica{st: harness.NewMemStore(), faults: tc.faults}
				r := syncRuntime(t, st, rep, f)
				s := create(t, r)
				synctest.Wait()
				submit(t, s, text("a", "hi"))
				(<-f.runs).end()
				closeRuntime(t, r)
				if got := rep.notes(); !slices.Equal(got, tc.want) {
					t.Fatalf("deliveries = %q, want %q", got, tc.want)
				}
				if got, want := rawLog(t, rep.st), rawLog(t, st); !slices.Equal(got, want) {
					t.Fatalf("receiver log = %q, want %q", got, want)
				}
				if v := s.View(); v.SyncedSeq != 5 || v.HeadSeq != 5 {
					t.Fatalf("View = %+v, want HeadSeq and SyncedSeq 5", v)
				}
			})
		})
	}
}

func TestStaleEpochStopsTheSessionWithoutAnAppend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rep := &replica{st: harness.NewMemStore()}
		if _, err := harness.ApplySync(bg, rep.st, protocol.SyncBatch{Epoch: 2, Session: "s1", FromSeq: 1}); err != nil {
			t.Fatal(err)
		}
		st, f := harness.NewMemStore(), newFake()
		r := syncRuntime(t, st, rep, f)
		s := create(t, r)
		synctest.Wait()
		if _, err := s.Submit(bg, text("a", "hi")); !errors.Is(err, harness.ErrSessionNotOwned) {
			t.Fatalf("Submit after a stale-epoch rejection = %v, want ErrSessionNotOwned", err)
		}
		noRun(t, f)
		wantLog(t, st, 0, "session.created", "owner.acquired 1")
		if got := rawLog(t, rep.st); len(got) != 0 {
			t.Fatalf("receiver log = %q, want empty", got)
		}
		closeRuntime(t, r)
	})
}

func TestHandoffWaitsForTheFinalAck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handoff func(*harness.Runtime, *harness.Session) error
	}{
		{"Release", func(_ *harness.Runtime, s *harness.Session) error { return s.Release(bg) }},
		{"Close", func(r *harness.Runtime, _ *harness.Session) error { return r.Close(bg) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st, f := harness.NewMemStore(), newFake()
				rep := &replica{st: harness.NewMemStore(), hold: make(chan struct{}), faults: []string{"", "hold"}}
				r := syncRuntime(t, st, rep, f)
				s := create(t, r)
				synctest.Wait()
				submit(t, s, text("a", "hi"))
				<-f.runs
				done := make(chan error, 1)
				go func() { done <- tc.handoff(r, s) }()
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("%s returned %v before the final ack", tc.name, err)
				default:
				}
				close(rep.hold)
				if err := <-done; err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
				wantLog(t, rep.st, 4, "turn.suspended handoff")
				if got, want := rawLog(t, rep.st), rawLog(t, st); !slices.Equal(got, want) {
					t.Fatalf("receiver log = %q, want %q", got, want)
				}
				closeRuntime(t, r)
			})
		})
	}
}

func TestOwnershipLossEndsABlockedDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := killable{lost: make(chan struct{})}
		rep := &replica{st: harness.NewMemStore(), hold: make(chan struct{}), faults: []string{"hold"}}
		r, err := harness.NewWithBackend(harness.Options{Store: harness.NewMemStore(), Owner: k, Sync: rep}, newFake())
		if err != nil {
			t.Fatal(err)
		}
		create(t, r)
		synctest.Wait()
		close(k.lost)
		synctest.Wait()
		closeRuntime(t, r)
	})
}
