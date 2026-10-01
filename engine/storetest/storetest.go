// Package storetest is the conformance suite for engine.SessionStore.
package storetest

import (
	"bytes"
	"errors"
	"io/fs"
	"sync"
	"testing"

	"github.com/majorcontext/harness/engine"
)

func rec(s string) []byte { return []byte(s) }

func mustAppend(t *testing.T, st engine.SessionStore, id string, at int, records ...[]byte) {
	t.Helper()
	if err := st.Append(id, at, records...); err != nil {
		t.Fatalf("Append(%q, %d): %v", id, at, err)
	}
}

func assertRecords(t *testing.T, st engine.SessionStore, id string, want ...string) {
	t.Helper()
	got, err := st.Load(id)
	if err != nil {
		t.Fatalf("Load(%q): %v", id, err)
	}
	if len(got) != len(want) {
		t.Fatalf("Load(%q) = %q, want %q", id, got, want)
	}
	for i := range want {
		if !bytes.Equal(got[i], []byte(want[i])) {
			t.Fatalf("Load(%q)[%d] = %q, want %q", id, i, got[i], want[i])
		}
	}
}

// Run checks that the stores newStore returns keep the SessionStore
// invariants.
func Run(t *testing.T, newStore func(t *testing.T) engine.SessionStore) {
	t.Run("AppendLoadOrder", func(t *testing.T) {
		st := newStore(t)
		mustAppend(t, st, "ses_a", 0, rec(`{"n":1}`))
		mustAppend(t, st, "ses_a", 1, rec(`{"n":2}`), rec(`{"n":3}`))
		assertRecords(t, st, "ses_a", `{"n":1}`, `{"n":2}`, `{"n":3}`)
	})

	t.Run("AppendWrongPositionConflicts", func(t *testing.T) {
		st := newStore(t)
		mustAppend(t, st, "ses_a", 0, rec(`{"n":1}`), rec(`{"n":2}`))
		for _, at := range []int{1, 3} {
			if err := st.Append("ses_a", at, rec(`{"n":9}`)); !errors.Is(err, engine.ErrAppendConflict) {
				t.Errorf("Append at %d = %v, want ErrAppendConflict", at, err)
			}
		}
		assertRecords(t, st, "ses_a", `{"n":1}`, `{"n":2}`)
	})

	t.Run("AppendFirstRecordAtZero", func(t *testing.T) {
		st := newStore(t)
		if err := st.Append("ses_new", 1, rec(`{"n":1}`)); !errors.Is(err, engine.ErrAppendConflict) {
			t.Fatalf("Append at 1 on an unknown id = %v, want ErrAppendConflict", err)
		}
		if _, err := st.Load("ses_new"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Load after a conflicting append = %v, want fs.ErrNotExist", err)
		}
		mustAppend(t, st, "ses_new", 0, rec(`{"n":1}`))
		assertRecords(t, st, "ses_new", `{"n":1}`)
	})

	t.Run("ConcurrentAppendOneWins", func(t *testing.T) {
		st := newStore(t)
		mustAppend(t, st, "ses_a", 0, rec(`{"n":1}`), rec(`{"n":2}`))
		cands := []string{`{"w":"a"}`, `{"w":"b"}`}
		errs := make([]error, len(cands))
		var wg sync.WaitGroup
		for i, c := range cands {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = st.Append("ses_a", 2, rec(c))
			}()
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
			case !errors.Is(err, engine.ErrAppendConflict):
				t.Fatalf("append %d = %v, want nil or ErrAppendConflict", i, err)
			}
		}
		if winner < 0 {
			t.Fatalf("both appends conflicted: %v", errs)
		}
		assertRecords(t, st, "ses_a", `{"n":1}`, `{"n":2}`, cands[winner])
	})

	t.Run("LoadUnknown", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.Load("ses_x"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Load = %v, want fs.ErrNotExist", err)
		}
		if n, err := st.Len("ses_x"); n != 0 || err != nil {
			t.Errorf("Len = %d, %v, want 0, nil", n, err)
		}
		if _, err := st.Header("ses_x"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Header = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("Header", func(t *testing.T) {
		st := newStore(t)
		mustAppend(t, st, "ses_a", 0, rec(`{"n":1}`), rec(`{"n":2}`))
		got, err := st.Header("ses_a")
		if err != nil || string(got) != `{"n":1}` {
			t.Errorf("Header = %q, %v, want {\"n\":1}", got, err)
		}
		if n, err := st.Len("ses_a"); n != 2 || err != nil {
			t.Errorf("Len = %d, %v, want 2, nil", n, err)
		}
	})

	t.Run("BlobRoundTrip", func(t *testing.T) {
		st := newStore(t)
		if err := st.PutBlob("ses_a", "cc", []byte("first")); err != nil {
			t.Fatal(err)
		}
		if err := st.PutBlob("ses_a", "cc", []byte("second")); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetBlob("ses_a", "cc")
		if err != nil || string(got) != "second" {
			t.Errorf("GetBlob = %q, %v, want second", got, err)
		}
		if _, err := st.GetBlob("ses_a", "absent"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("GetBlob(absent) = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("ReleaseKeepsData", func(t *testing.T) {
		st := newStore(t)
		mustAppend(t, st, "ses_a", 0, rec(`{"n":1}`))
		st.Release("ses_a")
		mustAppend(t, st, "ses_a", 1, rec(`{"n":2}`))
		assertRecords(t, st, "ses_a", `{"n":1}`, `{"n":2}`)
	})

	t.Run("ListReturnsIds", func(t *testing.T) {
		st := newStore(t)
		mustAppend(t, st, "ses_a", 0, rec(`{"n":1}`))
		mustAppend(t, st, "ses_b", 0, rec(`{"n":1}`))
		ids, err := st.List()
		if err != nil {
			t.Fatal(err)
		}
		have := map[string]bool{}
		for _, id := range ids {
			have[id] = true
		}
		if !have["ses_a"] || !have["ses_b"] {
			t.Errorf("List = %q, want ses_a and ses_b", ids)
		}
	})

	t.Run("SyncAfterAppend", func(t *testing.T) {
		st := newStore(t)
		mustAppend(t, st, "ses_a", 0, rec(`{"n":1}`))
		if err := st.Sync("ses_a"); err != nil {
			t.Errorf("Sync = %v, want nil", err)
		}
	})
}
