//go:build !unix

package codexauth

import (
	"context"
	"os"
)

// lockFile does nothing: without flock, two processes are not fenced. The Source of one process still fences its goroutines.
func lockFile(context.Context, *os.File) error { return nil }
