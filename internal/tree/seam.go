package tree

import "sync/atomic"

// Seam lets a test host see and stop two steps of a child report. Production
// code never sets it.
type Seam struct {
	// Settling runs in the goroutine of Report before it settles the child. The
	// report counts as in flight until Settling returns.
	Settling func(parent, child string)
	// SendWaits runs when a send waits for an in-flight report of child.
	SendWaits func(child string)
}

var seam atomic.Pointer[Seam]

// SetSeam installs s for this process and returns the function that removes it.
func SetSeam(s *Seam) (restore func()) {
	seam.Store(s)
	return func() { seam.CompareAndSwap(s, nil) }
}

func seamSettling(parent, child string) {
	if s := seam.Load(); s != nil && s.Settling != nil {
		s.Settling(parent, child)
	}
}

func seamSendWaits(child string) {
	if s := seam.Load(); s != nil && s.SendWaits != nil {
		s.SendWaits(child)
	}
}
