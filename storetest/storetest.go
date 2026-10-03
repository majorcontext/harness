// Package storetest is the conformance suite for harness.Store.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"slices"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
)

var ctx = context.Background()

func rec(s string) []byte { return []byte(s) }

func mustAppend(t *testing.T, st harness.Store, id string, at uint64, records ...string) {
	t.Helper()
	bs := make([][]byte, len(records))
	for i, r := range records {
		bs[i] = rec(r)
	}
	if err := st.Append(ctx, id, at, bs...); err != nil {
		t.Fatalf("Append(%q, %d): %v", id, at, err)
	}
}

func readAll(t *testing.T, st harness.Store, id string, after uint64, limit int) []string {
	t.Helper()
	got, err := st.Read(ctx, id, after, limit)
	if err != nil {
		t.Fatalf("Read(%q, %d, %d): %v", id, after, limit, err)
	}
	out := make([]string, len(got))
	for i, r := range got {
		if want := after + uint64(i) + 1; r.Seq != want {
			t.Fatalf("Read(%q, %d, %d)[%d].Seq = %d, want %d", id, after, limit, i, r.Seq, want)
		}
		out[i] = string(r.Data)
	}
	return out
}

func wantRecords(t *testing.T, st harness.Store, id string, want ...string) {
	t.Helper()
	if got := readAll(t, st, id, 0, 100); !slices.Equal(got, want) {
		t.Fatalf("records of %q = %q, want %q", id, got, want)
	}
	head, err := st.Head(ctx, id)
	if err != nil || head != uint64(len(want)) {
		t.Fatalf("Head(%q) = %d, %v, want %d", id, head, err, len(want))
	}
}

func readBlob(t *testing.T, st harness.Store, id, key string) string {
	t.Helper()
	rc, err := st.GetBlob(ctx, id, key)
	if err != nil {
		t.Fatalf("GetBlob(%q, %q): %v", id, key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type store = harness.Store

// Run checks that the stores newStore returns keep the harness.Store
// invariants.
func Run(t *testing.T, newStore func(t *testing.T) harness.Store) {
	for _, c := range []struct {
		name string
		fn   func(t *testing.T, st store)
	}{
		{"AppendReadOrder", testAppendReadOrder},
		{"AppendWrongSeqConflicts", testAppendWrongSeqConflicts},
		{"FirstAppendAtZero", testFirstAppendAtZero},
		{"ConcurrentAppendOneWins", testConcurrentAppendOneWins},
		{"InvalidRecordRejected", testInvalidRecordRejected},
		{"ReadPaging", testReadPaging},
		{"UnknownSessionIsEmpty", testUnknownSessionIsEmpty},
		{"SessionsSortedAndPaged", testSessionsSortedAndPaged},
		{"BlobRoundTrip", testBlobRoundTrip},
		{"StoredRecordsAreCopies", testStoredRecordsAreCopies},
	} {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newStore(t)) })
	}
}

func testAppendReadOrder(t *testing.T, st store) {
	mustAppend(t, st, "a", 0, `{"n":1}`)
	mustAppend(t, st, "a", 1, `{"n":2}`, `{"n":3}`)
	wantRecords(t, st, "a", `{"n":1}`, `{"n":2}`, `{"n":3}`)
}

func testAppendWrongSeqConflicts(t *testing.T, st store) {
	mustAppend(t, st, "a", 0, "1", "2")
	for _, at := range []uint64{1, 3} {
		if err := st.Append(ctx, "a", at, rec("9")); !errors.Is(err, harness.ErrConflict) {
			t.Errorf("Append at %d = %v, want ErrConflict", at, err)
		}
	}
	wantRecords(t, st, "a", "1", "2")
}

func testFirstAppendAtZero(t *testing.T, st store) {
	if err := st.Append(ctx, "new", 1, rec("1")); !errors.Is(err, harness.ErrConflict) {
		t.Fatalf("Append at 1 on an unknown session = %v, want ErrConflict", err)
	}
	if head, err := st.Head(ctx, "new"); head != 0 || err != nil {
		t.Fatalf("Head after a conflicting append = %d, %v, want 0, nil", head, err)
	}
	mustAppend(t, st, "new", 0, "1")
	wantRecords(t, st, "new", "1")
}

func testConcurrentAppendOneWins(t *testing.T, st store) {
	mustAppend(t, st, "a", 0, "1", "2")
	cands := []string{"a", "b"}
	errs := make([]error, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Go(func() { errs[i] = st.Append(ctx, "a", 2, rec(c)) })
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner >= 0 {
				t.Fatalf("both appends succeeded")
			}
			winner = i
		case !errors.Is(err, harness.ErrConflict):
			t.Fatalf("append %d = %v, want nil or ErrConflict", i, err)
		}
	}
	if winner < 0 {
		t.Fatalf("both appends conflicted: %v", errs)
	}
	wantRecords(t, st, "a", "1", "2", cands[winner])
}

func testInvalidRecordRejected(t *testing.T, st store) {
	for name, bad := range map[string]string{"empty": "", "newline": "a\nb"} {
		t.Run(name, func(t *testing.T) {
			err := st.Append(ctx, "a", 0, rec("ok"), rec(bad))
			if err == nil || errors.Is(err, harness.ErrConflict) {
				t.Fatalf("Append = %v, want a non-conflict error", err)
			}
			wantRecords(t, st, "a")
		})
	}
}

func testReadPaging(t *testing.T, st store) {
	mustAppend(t, st, "a", 0, "1", "2", "3", "4", "5")
	for _, tc := range []struct {
		after uint64
		limit int
		want  []string
	}{
		{0, 2, []string{"1", "2"}},
		{2, 2, []string{"3", "4"}},
		{4, 10, []string{"5"}},
		{5, 10, nil},
		{99, 10, nil},
	} {
		if got := readAll(t, st, "a", tc.after, tc.limit); !slices.Equal(got, tc.want) {
			t.Errorf("Read(after %d, limit %d) = %q, want %q", tc.after, tc.limit, got, tc.want)
		}
	}
}

func testUnknownSessionIsEmpty(t *testing.T, st store) {
	if head, err := st.Head(ctx, "x"); head != 0 || err != nil {
		t.Errorf("Head = %d, %v, want 0, nil", head, err)
	}
	if got := readAll(t, st, "x", 0, 10); len(got) != 0 {
		t.Errorf("Read = %q, want none", got)
	}
}

func testSessionsSortedAndPaged(t *testing.T, st store) {
	for _, id := range []string{"c", "a", "d", "b"} {
		mustAppend(t, st, id, 0, "1")
	}
	for _, tc := range []struct {
		after string
		limit int
		want  []string
	}{
		{"", 10, []string{"a", "b", "c", "d"}},
		{"", 2, []string{"a", "b"}},
		{"b", 2, []string{"c", "d"}},
		{"d", 2, nil},
	} {
		got, err := st.Sessions(ctx, tc.after, tc.limit)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("Sessions(%q, %d) = %q, %v, want %q", tc.after, tc.limit, got, err, tc.want)
		}
	}
}

func testBlobRoundTrip(t *testing.T, st store) {
	for _, v := range []string{"first", "second"} {
		if err := st.PutBlob(ctx, "a", "k", bytes.NewReader(rec(v))); err != nil {
			t.Fatal(err)
		}
	}
	if got := readBlob(t, st, "a", "k"); got != "second" {
		t.Errorf("GetBlob = %q, want second", got)
	}
	if _, err := st.GetBlob(ctx, "a", "absent"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("GetBlob(absent) = %v, want fs.ErrNotExist", err)
	}
}

func testStoredRecordsAreCopies(t *testing.T, st store) {
	in := rec("abc")
	mustAppend(t, st, "a", 0, "x")
	if err := st.Append(ctx, "a", 1, in); err != nil {
		t.Fatal(err)
	}
	in[0] = 'Z'
	got, err := st.Read(ctx, "a", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	got[1].Data[0] = 'Y'
	wantRecords(t, st, "a", "x", "abc")
}
