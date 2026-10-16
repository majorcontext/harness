package mcpsrc

import (
	"math/rand/v2"
	"sync/atomic"
	"time"
)

const (
	retryBase  = time.Second
	maxRetries = 3
)

var retriesHeld atomic.Bool

// HoldRetries stops every background retry of this process from firing before its Source closes. A test
// host calls it before it builds a runtime, so a test controls when a down server connects. Production code
// never calls it.
func HoldRetries() { retriesHeld.Store(true) }

// health is what the attempts to connect one server left behind. reason is
// empty while the server is connected.
type health struct {
	attempts int
	reason   string
	parked   bool
}

// retryWait is the wait before the retry after failures failed retries: retryBase,
// doubled for each failure, with half of it fixed and half random, so the servers
// of one cold start do not all return at once.
func retryWait(failures int) time.Duration {
	d := retryBase << failures
	return d/2 + rand.N(d/2)
}
