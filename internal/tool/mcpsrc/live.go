package mcpsrc

import (
	"context"
	"fmt"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// Start begins the first connect of s and returns at once. A nil Source has none.
func (s *Source) Start() {
	if s != nil {
		s.startOnce.Do(func() { s.send(startMsg{}) })
	}
}

// wait connects every server once and reports whether that ended before ctx.
func (s *Source) wait(ctx context.Context) bool {
	s.Start()
	if s.parent != nil && !s.parent.wait(ctx) {
		return false
	}
	select {
	case <-s.ready:
		return true
	case <-ctx.Done():
		return false
	}
}

// Set makes servers the servers of s and returns their status. A server that is not in servers goes; a server whose
// spec differs goes and connects again; a server whose spec is the same stays as it is. A new or changed server
// starts to connect at once, with the retry rules of a server of the first connect, and Set does not wait for it. A
// call in flight to a server that goes ends with an error. The next client of a name dials only after every
// earlier client of that name closed. The caller has validated the specs.
func (s *Source) Set(servers map[string]config.MCPServerSpec) []protocol.MCPServerStatus {
	reply := make(chan []protocol.MCPServerStatus, 1)
	if !s.send(setMsg{servers, reply}) {
		return nil
	}
	return <-reply
}

// refresh reads the tool list of the connected server name again and waits for
// the read. A read that a reconnect discards releases no waiter, so the wait has
// its own bound.
func (s *Source) refresh(ctx context.Context, name string) bool {
	owner := s.owner(name)
	if owner == nil {
		return false
	}
	sv, _ := owner.serverNow(name)
	if !sv.up() || !s.wait(ctx) {
		return false
	}
	sv, _ = owner.serverNow(name)
	if !sv.up() {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	reply := make(chan bool, 1)
	if !owner.send(refreshMsg{name, reply}) {
		return false
	}
	select {
	case refreshed := <-reply:
		return refreshed
	case <-ctx.Done():
		return false
	case <-owner.done:
		return false
	}
}

// connectNow makes one attempt to connect the server name unless it is connected or an attempt runs, which it waits
// for. It reports whether it started the attempt.
func (s *Source) connectNow(ctx context.Context, name string) (bool, error) {
	reply := make(chan connectResult, 1)
	if !s.send(connectMsg{name, reply}) {
		return false, fmt.Errorf("mcp: closed")
	}
	select {
	case r := <-reply:
		return r.dialed, r.err
	case <-ctx.Done():
		return false, context.Cause(ctx)
	case <-s.done:
		return false, fmt.Errorf("mcp: closed")
	}
}

// Close ends the goroutines of s, waits for the calls of its auth callback in flight, and closes every connection and stops every stdio server of s. With Detach the close of the HTTP clients runs after Close returns. A nil Source has none.
func (s *Source) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.authMu.Lock()
		s.authClosed = true
		s.authMu.Unlock()
		defer s.authCalls.Wait()
		reply := make(chan struct{})
		if s.send(closeMsg{reply}) {
			<-reply
		}
		s.workers.Wait()
		close(s.quit)
		<-s.done
	})
}

// AwaitFirstConnect makes a turn of each session Source wait for its first connect, as the warm-up does. A test host
// passes true.
func AwaitFirstConnect(wait bool) Option { return func(s *Source) { s.awaitFirst = wait } }

// Await waits until every server of s has ended its first attempt, and the first connect of its parent ended, and
// reports false when ctx ends first. A turn never calls it.
func (s *Source) Await(ctx context.Context) bool {
	s.Start()
	if s.parent != nil && !s.parent.wait(ctx) {
		return false
	}
	for {
		sn := s.snap.Load()
		if !pendingIn(sn) {
			return s.settle(ctx)
		}
		select {
		case <-sn.changed:
		case <-ctx.Done():
			return false
		}
	}
}

// pendingIn reports whether a server of sn has not ended its first attempt.
func pendingIn(sn *snapshot) bool {
	for name := range sn.health {
		if loadingIn(sn, name) {
			return true
		}
	}
	return false
}

// loadingWait bounds the wait of a call to a tool of a server that has not ended its first attempt.
const loadingWait = time.Second

// turnReady waits for the first connect of the servers that a model call needs. The first connect of the servers of a
// session never holds a turn: the model call reads the servers that are connected now, and the others load in the
// background. A read outside a turn, the warm-up, reads the servers that are connected and does not end the first
// connect, because a turn waits for the warm-up.
func (s *Source) turnReady(ctx context.Context) bool {
	if !s.session || s.awaitFirst {
		return s.wait(ctx) && s.settle(ctx)
	}
	s.Start()
	if s.parent != nil && (!s.parent.wait(ctx) || !s.parent.settle(ctx)) {
		return false
	}
	if turn.ID(ctx) == "" {
		return true
	}
	if !s.send(freezeMsg{}) {
		return true
	}
	select {
	case <-s.ready:
		return true
	case <-ctx.Done():
		return false
	}
}

// loading reports whether the server name of s has not ended its first attempt.
func (s *Source) loading(name string) bool { return loadingIn(s.snap.Load(), name) }

func loadingIn(sn *snapshot, name string) bool {
	h, ok := sn.health[name]
	return ok && h.attempts == 0 && !sn.live[name].up()
}

// awaitAttempt waits until the server name of s ends its first attempt, for at most loadingWait.
func (s *Source) awaitAttempt(ctx context.Context, name string) {
	t := time.NewTimer(loadingWait)
	defer t.Stop()
	for {
		sn := s.snap.Load()
		if !loadingIn(sn, name) {
			return
		}
		select {
		case <-sn.changed:
		case <-t.C:
			return
		case <-ctx.Done():
			return
		}
	}
}
