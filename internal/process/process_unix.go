//go:build unix

package process

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// killGroupWindow bounds killProcess's retry loop on unix, mirroring
// engine/bash_unix.go's bashGroupKillWindow (see its doc comment for why a
// single kill(-pgid, SIGKILL) is not always enough — a straggler
// grandchild forked just as the signal is delivered can miss it). Var so
// tests can shrink it.
var killGroupWindow = 200 * time.Millisecond

// configureProcessGroup runs the process in its own process group
// (Setpgid) so killProcess below can SIGKILL the whole tree as a unit —
// the same reasoning as engine/bash_unix.go's configureProcessGroup: a
// managed dev server that backgrounds a grandchild must not leave it
// orphaned when stopped.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcess SIGKILLs pid's whole process group, retrying for a short
// bounded window until the group is confirmed empty (ESRCH) — see
// engine/bash_unix.go's killProcessGroup for the exact race this guards
// against.
func killProcess(_ *exec.Cmd, pid int) {
	if pid <= 0 {
		return
	}
	deadline := time.Now().Add(killGroupWindow)
	for {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
