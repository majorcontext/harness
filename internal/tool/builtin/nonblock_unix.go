//go:build unix

package builtin

import "syscall"

// nonblock keeps the open of a named pipe from waiting for a writer.
const nonblock = syscall.O_NONBLOCK
