//go:build unix

package harness

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

const (
	lockPollMin = time.Millisecond
	lockPollMax = 20 * time.Millisecond
)

// lockFile takes a lock of f, shared or exclusive, which its close releases.
// It polls a nonblocking flock, so a wait for another holder ends with ctx.
func lockFile(ctx context.Context, f *os.File, shared bool) error {
	how := syscall.LOCK_EX
	if shared {
		how = syscall.LOCK_SH
	}
	wait := lockPollMin
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
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
		wait = min(2*wait, lockPollMax)
	}
}
