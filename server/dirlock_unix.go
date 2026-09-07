//go:build unix

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const lockFileName = ".harness-serve.lock"

// DirLock is an exclusive advisory hold on one session directory.
//
// The kernel releases a flock when its file descriptor closes, including on
// any process death, so this needs no TTL and leaves nothing to clean up.
// It is advisory and kernel-scoped: it fences processes on one node, which
// is the whole hazard only because RWO pins the volume to one node.
type DirLock struct{ f *os.File }

// LockSessionDir takes the exclusive lock on dir, creating dir if needed.
// It returns ErrSessionDirLocked when another holder exists.
func LockSessionDir(dir string) (*DirLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open session lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s", ErrSessionDirLocked, dir)
	}
	return &DirLock{f: f}, nil
}

// Close releases the lock. Safe on a nil receiver and safe to call twice.
func (l *DirLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	return f.Close()
}
