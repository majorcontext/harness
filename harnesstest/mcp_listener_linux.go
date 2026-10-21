//go:build linux

package harnesstest

import (
	"context"
	"net"
	"strconv"
	"syscall"
)

func listenMCP() (net.Listener, func(), error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_TCP)
	if err != nil {
		return nil, nil, err
	}
	release := func() { _ = syscall.Close(fd) }
	fail := func(err error) (net.Listener, func(), error) {
		release()
		return nil, nil, err
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0); err != nil {
		return fail(err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, mcpReusePortOption, 1); err != nil {
		return fail(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		return fail(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		return fail(err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(sa.(*syscall.SockaddrInet4).Port))
	lc := net.ListenConfig{Control: mcpReusePort}
	ln, err := lc.Listen(context.Background(), "tcp4", addr)
	if err != nil {
		return fail(err)
	}
	return ln, release, nil
}

// Another same-user socket that explicitly enables SO_REUSEPORT can share this address.
func mcpReusePort(_, _ string, raw syscall.RawConn) error {
	var optionErr error
	err := raw.Control(func(fd uintptr) {
		optionErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, mcpReusePortOption, 1)
	})
	if err != nil {
		return err
	}
	return optionErr
}
