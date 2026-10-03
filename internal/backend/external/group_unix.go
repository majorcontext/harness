//go:build unix

package external

import (
	"os"
	"os/exec"
	"syscall"
)

func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func interruptGroup(p *os.Process) { _ = syscall.Kill(-p.Pid, syscall.SIGINT) }

func killGroup(p *os.Process) { _ = syscall.Kill(-p.Pid, syscall.SIGKILL) }
