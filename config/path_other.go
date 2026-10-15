//go:build !js

package config

import "os/exec"

// onPath reports whether the program resolves, through PATH when it has no separator. It starts no process.
func onPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}
