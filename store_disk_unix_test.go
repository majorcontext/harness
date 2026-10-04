//go:build unix

package harness_test

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/storetest"
)

func TestDiskStoreInstances(t *testing.T) {
	storetest.RunInstances(t, func(t *testing.T) func() harness.Store {
		root := t.TempDir()
		return func() harness.Store { return harness.NewDiskStore(root) }
	})
}

// lockedLog opens log as a writer of another process does: it holds the
// exclusive lock until the file closes.
func lockedLog(t *testing.T, log string) *os.File {
	t.Helper()
	f, err := os.OpenFile(log, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return f
}

type headResult struct {
	head uint64
	err  error
}

func TestDiskStoreHeadWaitsForAWriterInFlight(t *testing.T) {
	root := t.TempDir()
	log := writeLog(t, root, "s", "1\n")
	synctest.Test(t, func(t *testing.T) {
		w := lockedLog(t, log)
		if _, err := w.WriteString("2\n3\n"); err != nil {
			t.Fatal(err)
		}
		st := harness.NewDiskStore(root)
		got := make(chan headResult, 1)
		go func() {
			head, err := st.Head(context.Background(), "s")
			got <- headResult{head, err}
		}()
		synctest.Wait()
		select {
		case r := <-got:
			t.Fatalf("Head = %+v while a writer holds the log, want it to wait", r)
		default:
		}
		if err := w.Truncate(2); err != nil {
			t.Fatal(err)
		}
		if _, err := w.WriteString("234\n"); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if r := <-got; r.head != 2 || r.err != nil {
			t.Fatalf("Head = %+v after the rollback and a new append of the same length, want 2 records", r)
		}
	})
}

func TestDiskStoreAppendEndsWithItsContextWhileAnotherHoldsTheLock(t *testing.T) {
	root := t.TempDir()
	log := writeLog(t, root, "s", "1\n")
	synctest.Test(t, func(t *testing.T) {
		w := lockedLog(t, log)
		ctx, cancel := context.WithCancel(context.Background())
		got := make(chan error, 1)
		go func() { got <- harness.NewDiskStore(root).Append(ctx, "s", 1, []byte("2")) }()
		synctest.Wait()
		select {
		case err := <-got:
			t.Fatalf("Append = %v while another holds the lock, want it to wait", err)
		default:
		}
		cancel()
		synctest.Wait()
		if err := <-got; !errors.Is(err, context.Canceled) {
			t.Fatalf("Append after cancel = %v, want context.Canceled", err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(log); string(b) != "1\n" {
			t.Fatalf("log = %q, want the canceled append to write nothing", b)
		}
	})
}
