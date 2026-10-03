//go:build !unix

package external

import (
	"os"
	"os/exec"
)

func ownGroup(*exec.Cmd) {}

func interruptGroup(p *os.Process) { _ = p.Kill() }

func killGroup(p *os.Process) { _ = p.Kill() }
