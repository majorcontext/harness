//go:build !unix

package harness

// syncDir is a no-op: a directory handle cannot be fsynced here.
func syncDir(string) error { return nil }
