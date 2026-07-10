//go:build windows

package engine

import "os/exec"

// configureProcessGroup is a no-op on windows: there is no process-group
// SIGKILL here, so a backgrounded grandchild survives cancellation — but
// cmd.WaitDelay (see bashTool) still bounds Wait(), so a pipe-holding child
// costs at most bashWaitDelay instead of wedging the turn forever. The bash
// tool is unix-only at runtime anyway (`sh -c`); this stub exists so the
// engine package keeps cross-compiling per AGENTS.md.
func configureProcessGroup(_ *exec.Cmd) {}
