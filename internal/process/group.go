package process

import (
	"os/exec"
	"time"
)

// KillOnCancel makes cmd run in its own process group, so that the end of its context kills the whole group,
// and gives the pipes of cmd grace to close after that.
func KillOnCancel(cmd *exec.Cmd, grace time.Duration) {
	configureProcessGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			killProcess(cmd, cmd.Process.Pid)
		}
		return nil
	}
	cmd.WaitDelay = grace
}
