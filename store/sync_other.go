//go:build !unix

package store

// syncDir is a no-op: a directory handle cannot be fsynced here.
func syncDir(string) error { return nil }
