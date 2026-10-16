package harness

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

// SetMCPServers makes servers the live set of runtime MCP servers. Config.MCPServers is the set at the start of the
// runtime, and the set does not persist. A server that is new connects at once, in the background; a server that the
// map leaves out closes; a server whose spec differs closes and connects again; a server whose spec is the same stays
// as it is. It does not wait for a connect. The servers of a session (WithSessionMCP) are no part of the set. It
// returns the status of every server, in name order. A spec that breaks a rule of Config.MCPServers, a name of a
// session server of a session that this runtime runs, and a name that an embedder tool or a started plugin tool
// reserves fail with ErrInvalidRequest, and change nothing. The tools of a server that arrives after the first connect
// are deferred, so the request prefix of a session does not change.
func (r *Runtime) SetMCPServers(ctx context.Context, servers map[string]config.MCPServerSpec) ([]protocol.MCPServerStatus, error) {
	if err := r.hold(); err != nil {
		return nil, err
	}
	defer r.group.Done()
	if err := (&config.Config{MCPServers: servers}).Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if err := r.startPlugins(ctx, false); err != nil {
		return nil, err
	}
	if err := r.checkSessionTools(servers); err != nil {
		return nil, err
	}
	r.mu.Lock()
	for id, e := range r.sessions {
		for _, name := range e.mcp {
			if _, ok := servers[name]; ok {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w: MCP server %q has the name of a session server of session %s", ErrInvalidRequest, name, id)
			}
		}
	}
	status := r.mcp.Set(servers)
	r.mu.Unlock()
	return status, nil
}

// mcpClash reports a name of session servers that a runtime server has. The caller holds r.mu.
func (r *Runtime) mcpClash(session map[string]config.MCPServerSpec) (string, bool) {
	for _, name := range slices.Sorted(maps.Keys(session)) {
		if r.mcp.Has(name) {
			return name, true
		}
	}
	return "", false
}
