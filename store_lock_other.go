//go:build !unix

package harness

import "os"

// lockFile does nothing: without flock, two processes on one root are not fenced.
func lockFile(*os.File) error { return nil }
