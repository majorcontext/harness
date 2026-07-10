//go:build !unix

package engine

import "os/exec"

// configureProcessGroup is a no-op on every non-unix GOOS (windows, plan9,
// js/wasm, wasip1): there is no unix process-group SIGKILL there, so a
// backgrounded grandchild survives cancellation — but cmd.WaitDelay (see
// bashTool) still bounds Wait(), so a pipe-holding child costs at most
// bashWaitDelay instead of wedging the turn forever. The bash tool is
// unix-only at runtime anyway (`sh -c`); this stub exists so the engine
// package keeps cross-compiling for ALL GOOS per AGENTS.md, not just the
// big two.
func configureProcessGroup(_ *exec.Cmd) {}
