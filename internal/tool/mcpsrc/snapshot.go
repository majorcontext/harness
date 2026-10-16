package mcpsrc

import (
	"context"
	"maps"
	"slices"

	"github.com/majorcontext/harness/config"
)

// snapshot is the immutable state that the actor of a Source publishes after each change. A reader never changes it.
type snapshot struct {
	// names are the servers that exist now, sorted.
	names  []string
	live   map[string]*server
	life   map[string]context.Context
	health map[string]health
	specs  map[string]config.MCPServerSpec
	// mem holds every server name that the runtime read, also a removed one.
	mem     map[string]*memory
	reached []string
	// reads counts the servers with a read of the tool list in flight or wanted.
	reads int
	// changed closes when the next snapshot is published.
	changed chan struct{}
}

// view is what one model call reads of the servers of a Source and of its parent.
type view struct {
	src     *Source
	names   []string
	live    map[string]*server
	life    map[string]context.Context
	health  map[string]health
	mem     map[string]*memory
	reached []string
}

// view returns the servers of s and of its parent from their current snapshots.
func (s *Source) view() view {
	sn := s.snap.Load()
	v := view{src: s, names: sn.names, live: sn.live, life: sn.life, health: sn.health, mem: sn.mem, reached: sn.reached}
	if s.parent == nil {
		return v
	}
	pv := s.parent.view()
	v.names = slices.Sorted(slices.Values(slices.Concat(pv.names, sn.names)))
	v.live, v.life, v.health, v.mem = maps.Clone(pv.live), maps.Clone(pv.life), maps.Clone(pv.health), maps.Clone(pv.mem)
	maps.Copy(v.live, sn.live)
	maps.Copy(v.life, sn.life)
	maps.Copy(v.health, sn.health)
	maps.Copy(v.mem, sn.mem)
	v.reached = slices.Concat(pv.reached, sn.reached)
	return v
}

// serverNow returns the connected server of name in the current snapshot, with its lifetime. It returns nil
// when no server of that name is connected.
func (s *Source) serverNow(name string) (*server, context.Context) {
	for ; s != nil; s = s.parent {
		sn := s.snap.Load()
		if _, ok := sn.health[name]; ok {
			return sn.live[name], sn.life[name]
		}
	}
	return nil, nil
}

// owner returns the Source that has the server name now, or nil.
func (s *Source) owner(name string) *Source {
	for ; s != nil; s = s.parent {
		if _, ok := s.snap.Load().health[name]; ok {
			return s
		}
	}
	return nil
}

// Has reports whether a server named name exists in s or its parent.
func (s *Source) Has(name string) bool { return s.owner(name) != nil }

// Active reports whether s or its parent ever had a server.
func (s *Source) Active() bool {
	return s != nil && (len(s.snap.Load().mem) > 0 || s.parent.Active())
}

// Populated reports whether s or its parent has a server now.
func (s *Source) Populated() bool {
	return s != nil && (len(s.snap.Load().names) > 0 || s.parent.Populated())
}

// Specs returns a copy of the specs of the servers of s, without those of its parent.
func (s *Source) Specs() map[string]config.MCPServerSpec {
	out := map[string]config.MCPServerSpec{}
	for name, sp := range s.snap.Load().specs {
		out[name] = cloneSpec(sp)
	}
	return out
}

// settle waits until no read of a tool list is in flight, in s and its parent. It reports false when ctx ends first.
func (s *Source) settle(ctx context.Context) bool {
	if s == nil {
		return true
	}
	if !s.parent.settle(ctx) {
		return false
	}
	for {
		sn := s.snap.Load()
		if sn.reads == 0 {
			return true
		}
		select {
		case <-sn.changed:
		case <-ctx.Done():
			return false
		}
	}
}
