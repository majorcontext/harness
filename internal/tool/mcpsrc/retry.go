package mcpsrc

import (
	"math/rand/v2"
	"time"
)

const (
	retryBase  = time.Second
	maxRetries = 3
)

// Option changes how New builds a Source.
type Option func(*Source)

// HoldRetries with hold true stops every background retry of the Source and of its session Sources.
// Only a test host passes true.
func HoldRetries(hold bool) Option { return func(s *Source) { s.holdRetries = hold } }

// Detach makes Close hand the close of the HTTP clients of a Source to run and return at once. The close of an HTTP
// client can wait on the remote server, so run owns the goroutine and bounds the wait; it runs f exactly once.
func Detach(run func(f func())) Option { return func(s *Source) { s.detach = run } }

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
