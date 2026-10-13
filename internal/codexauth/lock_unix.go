//go:build unix

package codexauth

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// lockFile takes an exclusive flock of f, which its close releases. It polls a
// nonblocking flock, so a wait for another holder ends with ctx.
func lockFile(ctx context.Context, f *os.File) error {
	wait := time.Millisecond
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, syscall.EWOULDBLOCK):
			return err
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		wait = min(2*wait, 50*time.Millisecond)
	}
}

// otherUsersCanRead reports whether the mode lets the group or other users read the file.
func otherUsersCanRead(mode os.FileMode) bool { return mode.Perm()&0o077 != 0 }
