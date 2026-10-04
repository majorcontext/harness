//go:build unix

package harness

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive lock of f, which its close releases.
func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return err
		}
	}
}
