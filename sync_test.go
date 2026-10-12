package harness_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/httpsync"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
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

// replica is a Sync that applies each batch to st with ApplySync. The next
// fault applies to one delivery: "lose" applies the batch and fails, "drop"
// acknowledges the batch without applying it, and "hold" waits for hold.
type replica struct {
	st     *store.MemStore
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
		ack, err = httpsync.ApplySync(ctx, r.st, b)
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
				st, f, rep := store.NewMemStore(), newFake(), &replica{st: store.NewMemStore(), faults: tc.faults}
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
				if v := s.Snapshot(); v.SyncedSeq != 5 || v.HeadSeq != 5 {
					t.Fatalf("View = %+v, want HeadSeq and SyncedSeq 5", v)
				}
			})
		})
	}
}

func TestAReplicaResumesTheBackendState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, f1, rep := store.NewMemStore(), newFake(), &replica{st: store.NewMemStore()}
		f1.save = "v1"
		r1 := syncRuntime(t, st, rep, f1)
		submit(t, create(t, r1), text("a", "hi"))
		(<-f1.runs).end()
		closeRuntime(t, r1)
		f2 := newFake()
		f2.save = "v2"
		r2 := runtime(t, rep.st, f2)
		submit(t, open(t, r2), text("b", "again"))
		run := <-f2.runs
		run.end()
		closeRuntime(t, r2)
		if run.state != "v1" {
			t.Fatalf("state on the replica = %q, want %q", run.state, "v1")
		}
	})
}

func TestStaleEpochStopsTheSessionWithoutAnAppend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rep := &replica{st: store.NewMemStore()}
		if _, err := httpsync.ApplySync(bg, rep.st, protocol.SyncBatch{Epoch: 2, Session: "s1", FromSeq: 1}); err != nil {
			t.Fatal(err)
		}
		st, f := store.NewMemStore(), newFake()
		r := syncRuntime(t, st, rep, f)
		s := create(t, r)
		synctest.Wait()
		if _, err := s.Prompt(bg, text("a", "hi")); !errors.Is(err, harness.ErrSessionNotOwned) {
			t.Fatalf("Prompt after a stale-epoch rejection = %v, want ErrSessionNotOwned", err)
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
				st, f := store.NewMemStore(), newFake()
				rep := &replica{st: store.NewMemStore(), hold: make(chan struct{}), faults: []string{"", "hold"}}
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
		rep := &replica{st: store.NewMemStore(), hold: make(chan struct{}), faults: []string{"hold"}}
		r, err := harness.NewWithBackend(harness.Options{Store: store.NewMemStore(), Owner: k, Sync: rep}, newFake())
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

func TestADivergedReceiverStopsTheSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rep := &replica{st: store.NewMemStore()}
		other := protocol.SyncBatch{Epoch: 1, Session: "s1", FromSeq: 1, Records: recs("x1", "x2", "x3")}
		if _, err := httpsync.ApplySync(bg, rep.st, other); err != nil {
			t.Fatal(err)
		}
		st, f := store.NewMemStore(), newFake()
		r := syncRuntime(t, st, rep, f)
		s := create(t, r)
		synctest.Wait()
		if _, err := s.Prompt(bg, text("a", "hi")); !errors.Is(err, harness.ErrSessionNotOwned) {
			t.Fatalf("Prompt after a conflict rejection = %v, want ErrSessionNotOwned", err)
		}
		ctx, cancel := context.WithTimeout(bg, time.Minute)
		defer cancel()
		if err := s.Release(ctx); !errors.Is(err, harness.ErrConflict) {
			t.Fatalf("Release after a conflict rejection = %v, want ErrConflict", err)
		}
		noRun(t, f)
		wantLog(t, st, 0, "session.created", "owner.acquired 1")
		if got := rep.notes(); len(got) != 1 {
			t.Fatalf("deliveries = %q, want one rejected delivery", got)
		}
		closeRuntime(t, r)
	})
}
