//go:build unix

package engine_test

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/majorcontext/harness/engine"
)

// TestWaitSnapshotsJoinsAnInFlightWriteFromOutsideThePackage names the
// failure the package-internal snapshot tests cannot: a snapshot write
// OUTLIVES the call that scheduled it, and before WaitSnapshots existed no
// caller outside engine/ could join it. Input: a session whose snapshot
// write is blocked mid-flight. State: the caller has returned and is about
// to delete the session directory, exactly as t.TempDir() cleanup does.
// Wrong output without the join: the writer creates its file under a
// directory the caller has already started to remove, and the removal fails
// with "directory not empty" (cmd/harness/delegation_passthrough_test.go
// hit this; server/ hit the same shape in #302).
//
// This file is package engine_test on purpose. The unexported
// waitSnapshots is unreachable from here, so the test compiles only against
// the exported seam — the gap itself is red-verified by the compiler.
//
// Forcing the ordering, with no sleep and no deadline: the snapshot writer
// opens its temp file with O_WRONLY (engine/snapshot.go's
// writeSessionSnapshot), so a FIFO at that path blocks it inside the open
// syscall until this test opens the read end. The write is therefore
// PROVABLY still in flight at the moment WaitSnapshots is called — the
// assertion below the barrier proves it, so this test cannot pass because
// the write happened to finish first.
func TestWaitSnapshotsJoinsAnInFlightWriteFromOutsideThePackage(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "sessions")

	s := engine.NewSession(engine.Config{
		SessionDir: sessionDir,
		// Any positive cadence is enough: it enables snapshot WRITING at
		// all, and ReleaseFiles' on-idle trigger below is what actually
		// schedules this test's write.
		SnapshotEveryRecords: 1,
		// The writer fsyncs its temp file unless the session runs in
		// volume-sync mode, and fsync on a FIFO is EINVAL — which would
		// send the write down its error path instead of the rename this
		// test watches for. The barrier, not the durability mode, is what
		// this test is about.
		SessionSync: engine.SessionSyncVolume,
	})
	// Persist writes the header records, so the session has journal state
	// to checkpoint; ReleaseFiles is the on-idle (eviction) trigger.
	if err := s.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	tmpPath := filepath.Join(sessionDir, s.ID+".snap.tmp")
	snapPath := filepath.Join(sessionDir, s.ID+".snap")
	if err := syscall.Mkfifo(tmpPath, 0o644); err != nil {
		t.Fatalf("mkfifo %s: %v", tmpPath, err)
	}

	s.ReleaseFiles()

	// The barrier holds: the writer is parked in open(2) on the FIFO, so
	// the snapshot cannot have landed. A failure here means the write path
	// no longer opens this temp path, and the rest of this test would be
	// proving nothing.
	if _, err := os.Stat(snapPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat %s before the barrier was released = %v, want ErrNotExist (the write was not in flight, so this test cannot prove a join)", snapPath, err)
	}

	drained := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(tmpPath, os.O_RDONLY, 0)
		if err != nil {
			drained <- err
			return
		}
		_, err = io.Copy(io.Discard, f)
		drained <- errors.Join(err, f.Close())
	}()

	s.WaitSnapshots()

	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("stat %s after WaitSnapshots = %v, want the snapshot on disk: WaitSnapshots returned while the write was still running", snapPath, err)
	}
	if err := <-drained; err != nil {
		t.Fatalf("draining the barrier FIFO: %v", err)
	}

	// The property the join exists for, stated the way t.TempDir() states
	// it: the directory comes out and stays out.
	if err := os.RemoveAll(sessionDir); err != nil {
		t.Fatalf("RemoveAll(%s): %v", sessionDir, err)
	}
	if _, err := os.Stat(sessionDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat %s after removal = %v, want ErrNotExist: a snapshot write recreated the session directory", sessionDir, err)
	}
}
