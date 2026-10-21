//go:build linux

package harnesstest

import (
	"errors"
	"net"
	"syscall"
	"testing"
)

func checkMCPReleased(t *testing.T, addr string) {
	t.Helper()
	checkMCPRefused(t, addr)
	ln, err := net.Listen("tcp4", addr)
	if err == nil {
		_ = ln.Close()
		t.Fatal("closed MCP fixture port can be reused by another server")
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("rebind error = %v, want address already in use", err)
	}
}
