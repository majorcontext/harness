//go:build !unix

package builtin

import "os/exec"

// processGroup does nothing: cmd.WaitDelay still bounds a held pipe.
func processGroup(*exec.Cmd) {}
