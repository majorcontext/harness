//go:build !unix

package server

// DirLock is a no-op on a platform with no flock.
//
// Nothing fences the session directory here. This is an absent safety net,
// not parity with the unix build: two servers on one directory will both
// serve and interleave their writes. Harness ships on unix, so no deployment
// relies on this; a port elsewhere must supply its own single-writer fence.
type DirLock struct{}

// LockSessionDir always succeeds where flock is unavailable — see DirLock.
// This exists so `go build ./...` stays clean off unix.
func LockSessionDir(string) (*DirLock, error) { return &DirLock{}, nil }

// Close releases nothing.
func (l *DirLock) Close() error { return nil }
