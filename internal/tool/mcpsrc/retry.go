package mcpsrc

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

const (
	retryBase  = time.Second
	maxRetries = 3
)

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

// retry dials name again until it connects or maxRetries retries fail. A
// server that is parked then waits for the connect action of the mcp tool.
func (s *Source) retry(name string) {
	for failures := 0; failures < maxRetries; failures++ {
		t := time.NewTimer(retryWait(failures))
		select {
		case <-t.C:
		case <-s.ctx.Done():
			t.Stop()
			return
		}
		if s.server(name).up() {
			return
		}
		err := s.connect(s.ctx, name)
		if s.ctx.Err() != nil {
			return
		}
		if err == nil {
			s.log.Info("mcp: server connected after a retry", "server", name)
			return
		}
		s.log.Warn("mcp: retry did not connect", "server", name, "err", hide(name, err))
	}
	s.mu.Lock()
	parked := !s.servers[name].up()
	if parked {
		s.health[name].parked = true
	}
	s.mu.Unlock()
	if parked {
		s.log.Warn("mcp: server parked; the mcp tool can connect it", "server", name)
	}
}

// noticeKind is the kind of the notice of the servers that are down, and
// recovered is what the conversation says when the last of them connects.
const (
	noticeKind = "mcp"
	recovered  = "[mcp: every configured server is connected again.]"
)

// unavailable renders the notice that names each server that is not
// connected, with the reason, or "" when every server is connected. The
// caller holds s.mu.
func (s *Source) unavailable() string {
	var down []string
	for _, name := range s.names {
		up, h := s.state(name)
		if up {
			continue
		}
		if h.parked {
			down = append(down, fmt.Sprintf("%s (%s; use the mcp tool action %q to retry)", name, h.reason, "connect"))
		} else {
			down = append(down, fmt.Sprintf("%s (%s; retrying)", name, h.reason))
		}
	}
	if len(down) == 0 {
		return ""
	}
	return "[mcp: unavailable — " + strings.Join(down, ", ") +
		". Tools from these servers are temporarily absent and may return later in this session.]"
}

// state returns whether server is connected and what its connects left behind. The caller holds s.mu.
// A server of the parent is read under the lock of the parent.
func (s *Source) state(server string) (bool, health) {
	if !s.owns(server) && s.parent != nil {
		s.parent.mu.Lock()
		defer s.parent.mu.Unlock()
		return s.parent.servers[server].up(), *s.parent.health[server]
	}
	return s.servers[server].up(), *s.health[server]
}

// serverStatus is one server in the result of the status action.
type serverStatus struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Attempts  int    `json:"attempts"`
	Parked    bool   `json:"parked"`
	Reason    string `json:"reason,omitempty"`
}

// status returns the live state of each server, in the order of their names.
func (s *Source) status() []serverStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]serverStatus, 0, len(s.names))
	for _, name := range s.names {
		up, h := s.state(name)
		out = append(out, serverStatus{Name: name, Connected: up, Attempts: h.attempts, Parked: h.parked, Reason: h.reason})
	}
	return out
}
