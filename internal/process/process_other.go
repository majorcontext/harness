//go:build !unix

package process

import "os/exec"

// configureProcessGroup is a no-op on non-unix GOOS: there is no unix
// process-group SIGKILL there (see killProcess below). This stub exists
// so package process keeps cross-compiling for every GOOS per AGENTS.md,
// not just the big two.
func configureProcessGroup(_ *exec.Cmd) {}

// killProcess plainly kills the direct child process; there is no process
// group to reach on this platform.
func killProcess(cmd *exec.Cmd, _ int) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
