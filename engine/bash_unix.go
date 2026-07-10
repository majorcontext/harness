//go:build unix

package engine

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// bashGroupKillWindow bounds killProcessGroup's retry loop (see there for
// why a single kill(-pgid, SIGKILL) isn't always enough). Tests override
// this var to keep wall-clock cost small.
var bashGroupKillWindow = 200 * time.Millisecond

// killProcessGroup SIGKILLs every process in pgid, retrying for a short,
// bounded window until the group is confirmed empty (kill(-pgid, ...)
// reports ESRCH) instead of sending one shot and hoping.
//
// One shot is not enough: kill(-pgid, sig) signals only the processes that
// are members of the group at the instant of that syscall. If `sh` is, at
// that exact instant, itself inside the fork() for a backgrounded command
// (`sleep 60 &`, the grandchild this whole fix is about), the kernel may
// finish that fork before delivering the pending SIGKILL to `sh` — and the
// brand-new grandchild, not yet a group member when kill() ran, never gets
// signaled at all. It's then a live orphan once `sh` dies, indistinguishable
// from the very hang this fix closes. Retrying for a short window catches
// that straggler on the next pass; it does not affect the common case
// (group already gone → immediate ESRCH, one syscall).
func killProcessGroup(pgid int) {
	deadline := time.Now().Add(bashGroupKillWindow)
	for {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && errors.Is(err, syscall.ESRCH) {
			return // no process in the group remains
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// configureProcessGroup runs sh in its own process group so a grandchild
// that backgrounds itself (`sleep 60 &`, or — the real production trigger —
// a dev server an agent legitimately starts to test against) can be killed
// as a unit. Without this, CommandContext's default Cancel only reaches the
// direct sh child, orphaning any grandchild that inherited the output
// pipe's write end and permanently wedging Wait().
//
// cmd.Cancel runs once the command context is done — BashTimeout elapsing,
// or the caller aborting the turn. SIGKILL the whole process group
// (negative pid), not just the direct child, so a backgrounded grandchild
// dies with it instead of being orphaned holding the pipe open.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		killProcessGroup(cmd.Process.Pid)
		return os.ErrProcessDone
	}
}
