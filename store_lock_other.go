//go:build !unix

package harness

import (
	"context"
	"os"
)

// lockFile does nothing: without flock, two processes on one root are not fenced.
func lockFile(context.Context, *os.File, bool) error { return nil }
