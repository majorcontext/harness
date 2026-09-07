//go:build unix

package server

import (
	"errors"
	"testing"
)

// Two open file descriptions in one process contend on flock exactly as two
// processes do, so this needs no subprocess.
func TestLockSessionDirRejectsSecondHolder(t *testing.T) {
	dir := t.TempDir()

	first, err := LockSessionDir(dir)
	if err != nil {
		t.Fatalf("first LockSessionDir: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	second, err := LockSessionDir(dir)
	if err == nil {
		_ = second.Close()
		t.Fatal("second LockSessionDir succeeded, want ErrSessionDirLocked")
	}
	if !errors.Is(err, ErrSessionDirLocked) {
		t.Fatalf("second LockSessionDir err = %v, want ErrSessionDirLocked", err)
	}
}

func TestLockSessionDirReleasesOnClose(t *testing.T) {
	dir := t.TempDir()

	first, err := LockSessionDir(dir)
	if err != nil {
		t.Fatalf("first LockSessionDir: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := LockSessionDir(dir)
	if err != nil {
		t.Fatalf("LockSessionDir after Close: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
}
