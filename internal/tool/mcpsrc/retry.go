package mcpsrc

import (
	"math"
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

// retry dials e again until it connects or maxRetries retries fail. A
// server that is parked then waits for the connect action of the mcp tool.
func (s *Source) retry(e *entry) {
	for failures := 0; failures < maxRetries; failures++ {
		wait := retryWait(failures)
		if retriesHeld.Load() {
			wait = time.Duration(math.MaxInt64)
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-e.ctx.Done():
			t.Stop()
			return
		}
		if s.connected(e) {
			return
		}
		_, err := s.connect(e.ctx, e)
		if e.ctx.Err() != nil {
			return
		}
		if err == nil {
			s.log.Info("mcp: server connected after a retry", "server", e.name)
			return
		}
		s.log.Warn("mcp: retry did not connect", "server", e.name, "err", hide(e.name, err))
	}
	s.mu.Lock()
	parked := !e.sv.up() && s.entries[e.name] == e
	if parked {
		e.health.parked = true
	}
	s.mu.Unlock()
	if parked {
		s.log.Warn("mcp: server parked; the mcp tool can connect it", "server", e.name)
	}
}
