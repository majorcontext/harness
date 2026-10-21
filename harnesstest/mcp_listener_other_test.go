//go:build !linux

package harnesstest

import "testing"

func checkMCPReleased(t *testing.T, addr string) {
	t.Helper()
	checkMCPRefused(t, addr)
}
