package mcpsrc

import (
	"context"
	"fmt"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

// wait connects every server once and reports whether that ended before ctx.
func (s *Source) wait(ctx context.Context) bool {
	s.startOnce.Do(func() { s.send(startMsg{}) })
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

// Close closes every connection and stops every stdio server of s, and ends its goroutines. A nil Source has none.
func (s *Source) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		reply := make(chan struct{})
		if s.send(closeMsg{reply}) {
			<-reply
		}
		s.workers.Wait()
		close(s.quit)
		<-s.done
	})
}
