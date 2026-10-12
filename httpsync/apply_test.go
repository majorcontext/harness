package httpsync_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/httpsync"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

var bg = context.Background()

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
			st := store.NewMemStore()
			seed := protocol.SyncBatch{Epoch: 2, Session: "s1", FromSeq: 1, Records: recs("r1", "r2")}
			if ack, err := httpsync.ApplySync(bg, st, seed); err != nil || ack.Head != 2 {
				t.Fatalf("seed ApplySync = %+v, %v", ack, err)
			}
			tc.b.Session = "s1"
			ack, err := httpsync.ApplySync(bg, st, tc.b)
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
