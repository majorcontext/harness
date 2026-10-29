//go:build !unix

package workspace

import "os/exec"

// A platform with no process groups kills the process of the command only.
func ownProcessGroup(*exec.Cmd) {}

func killProcessGroup(*exec.Cmd) error { return nil }
