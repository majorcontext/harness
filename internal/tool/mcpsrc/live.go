package mcpsrc

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/mcp"
	"github.com/majorcontext/harness/protocol"
)

// refreshTimeout bounds the read of a tool list after a list_changed notification.
const refreshTimeout = 15 * time.Second

func cloneSpec(sp config.MCPServerSpec) config.MCPServerSpec {
	sp.Command, sp.Env, sp.Headers = slices.Clone(sp.Command), slices.Clone(sp.Env), maps.Clone(sp.Headers)
	return sp
}

func sameSpec(a, b config.MCPServerSpec) bool {
	return slices.Equal(a.Command, b.Command) && slices.Equal(a.Env, b.Env) && a.Dir == b.Dir && a.URL == b.URL &&
		maps.Equal(a.Headers, b.Headers) && a.ConnectTimeoutS == b.ConnectTimeoutS && a.ToolLoading == b.ToolLoading
}

// Replace makes servers the servers of s and returns the function that finishes the change. A server that is not in
// servers goes; a server whose spec differs goes and connects again; a server whose spec is the same stays as it
// is. A new or changed server starts to connect at once, with the retry rules of a server of the first connect, and
// Replace does not wait for it. A call in flight to a server that goes ends with an error. The caller has validated the
// specs. Replace does no I/O. The returned function closes the clients that went and returns the status of the servers.
func (s *Source) Replace(servers map[string]config.MCPServerSpec) func() []protocol.MCPServerStatus {
	var gone, fresh []*entry
	type closing struct {
		c    *mcp.Client
		done chan struct{}
	}
	var clients []closing
	gates := map[string]chan struct{}{}
	s.mu.Lock()
	if !s.closed {
		for name, e := range s.entries {
			if sp, ok := servers[name]; !ok || !sameSpec(e.spec, sp) {
				delete(s.entries, name)
				gone = append(gone, e)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(servers)) {
			if s.entries[name] == nil {
				fresh = append(fresh, s.add(name, servers[name]))
			}
		}
		for _, e := range gone {
			e.cancel()
			if e.sv.up() {
				gates[e.name] = make(chan struct{})
				clients = append(clients, closing{e.sv.client, gates[e.name]})
			}
			e.sv = nil
		}
		switch {
		case s.started:
			for _, e := range fresh {
				e.dialing <- struct{}{}
				s.group.Go(func() { s.bring(e, gates[e.name]) })
			}
		case len(s.entries) > 0:
			s.start.Do(func() { s.group.Go(s.firstConnect) })
		}
	}
	s.mu.Unlock()
	return func() []protocol.MCPServerStatus {
		var wg sync.WaitGroup
		for _, c := range clients {
			wg.Go(func() {
				_ = c.c.Close()
				close(c.done)
			})
		}
		wg.Wait()
		return s.status()
	}
}

// Has reports whether a server named name exists in s or its parent.
func (s *Source) Has(name string) bool {
	_, e := s.find(name)
	return e != nil
}

// bring makes the first connect of e, which holds e.dialing, after closed ends (the close of the client that e replaces), and retries in the background when that fails. The
// slot is held from Replace on, so a connect that the model asks for after the call waits for this one.
func (s *Source) bring(e *entry, closed <-chan struct{}) {
	if closed != nil {
		select {
		case <-closed:
		case <-e.ctx.Done():
		}
	}
	ctx, cancel := context.WithCancel(e.ctx)
	_, err := s.dialHeld(ctx, e)
	cancel()
	<-e.dialing
	if err != nil && e.ctx.Err() == nil {
		s.log.Warn("mcp: server did not connect", "server", e.name, "err", hide(e.name, err))
		s.retry(e)
	}
}

// listChanged reads the tool list of c again after a notification. A refresh in flight holds the next model
// call, so that the call reads the new list.
func (s *Source) listChanged(e *entry, c *mcp.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.entries[e.name] != e {
		return
	}
	if s.refreshing == 0 {
		s.idle = make(chan struct{})
	}
	s.refreshing++
	e.gen++
	gen := e.gen
	s.group.Go(func() {
		defer s.refreshed()
		s.refresh(e, c, gen)
	})
}

func (s *Source) refreshed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshing--; s.refreshing == 0 {
		close(s.idle)
	}
}

func (s *Source) refresh(e *entry, c *mcp.Client, gen int) {
	ctx, cancel := context.WithTimeout(e.ctx, refreshTimeout)
	defer cancel()
	tools, err := c.ListAllTools(ctx)
	if err != nil {
		if e.ctx.Err() == nil {
			s.log.Warn("mcp: tool list did not refresh", "server", e.name, "err", hide(e.name, err))
		}
		return
	}
	slices.SortFunc(tools, func(a, b mcp.Tool) int { return strings.Compare(a.Name, b.Name) })
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[e.name] != e || !e.sv.up() || e.sv.client != c || gen < e.applied {
		return
	}
	e.applied = gen
	e.sv = newServer(c, tools)
	s.remember(e.name, e.sv)
}

// settle waits until no refresh of a tool list is in flight, in s and its parent. It reports false when ctx ends first.
func (s *Source) settle(ctx context.Context) bool {
	if s == nil {
		return true
	}
	if !s.parent.settle(ctx) {
		return false
	}
	s.mu.Lock()
	idle := s.idle
	if s.refreshing == 0 {
		idle = nil
	}
	s.mu.Unlock()
	if idle == nil {
		return true
	}
	select {
	case <-idle:
		return true
	case <-ctx.Done():
		return false
	}
}

// Specs returns a copy of the specs of the servers of s, without those of its parent.
func (s *Source) Specs() map[string]config.MCPServerSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]config.MCPServerSpec, len(s.entries))
	for name, e := range s.entries {
		out[name] = cloneSpec(e.spec)
	}
	return out
}

// Populated reports whether s or its parent has a server now.
func (s *Source) Populated() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	n := len(s.entries)
	s.mu.Unlock()
	return n > 0 || s.parent.Populated()
}
