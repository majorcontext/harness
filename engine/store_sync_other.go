//go:build !unix

package engine

// syncDir is a no-op off unix: directory-handle Sync is a unix-ism (it
// errors on Windows), and the durable-enqueue deployments that rely on
// directory-entry durability (see the unix implementation) run on unix
// volumes. Off-unix callers keep working with file-level durability only.
func syncDir(string) error { return nil }
