//go:build unix

package builtin

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// killWindow bounds the kill retries of a process group.
const killWindow = 200 * time.Millisecond

// killGroup kills pgid until no member is left. One kill can miss a child
// that sh forks at the same instant, so it retries for a short window.
func killGroup(pgid int) {
	deadline := time.Now().Add(killWindow)
	for {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) || time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func processGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		killGroup(cmd.Process.Pid)
		return os.ErrProcessDone
	}
}
