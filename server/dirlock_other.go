//go:build !unix

package server

// DirLock is a no-op on a platform with no flock.
type DirLock struct{}

// LockSessionDir always succeeds where flock is unavailable. Harness ships
// on unix; this exists so `go build ./...` stays clean elsewhere.
func LockSessionDir(string) (*DirLock, error) { return &DirLock{}, nil }

// Close releases nothing.
func (l *DirLock) Close() error { return nil }
