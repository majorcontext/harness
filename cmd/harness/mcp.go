package main

import (
	"context"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
)

// mcpCloseTimeout bounds shutting down every configured MCP server
// connection, on top of the bounded timeouts engine.MCPManager.Close
// already enforces per client — a process-level backstop mirroring the
// plugin host's own shutdown bound.
const mcpCloseTimeout = 10 * time.Second

// buildMCPManager converts a config's mcp_servers section into an
// *engine.MCPManager, built once per process and shared across every
// session (run mode's one session, or every session `harness serve` hosts)
// via engine.Config.MCP / server.Options.MCP — see engine/mcp.go's package
// doc for why this is process-wide rather than per-session.
//
// It returns a nil *engine.MCPManager (and so a nil engine.MCPRegistry once
// routed through mcpRegistry below) when no servers are configured,
// mirroring buildPluginHost's nil-when-empty convention. Nothing here
// touches the network — connecting happens lazily on first use (see
// MCPManager.Tools).
func buildMCPManager(servers map[string]config.MCPServerSpec) *engine.MCPManager {
	if len(servers) == 0 {
		return nil
	}
	out := make(map[string]engine.MCPServerConfig, len(servers))
	for name, spec := range servers {
		out[name] = mcpServerConfig(spec)
	}
	return engine.NewMCPManager(out)
}

// mcpServerConfig converts one config.MCPServerSpec into the
// engine.MCPServerConfig buildMCPManager wires it into — factored out (and
// so directly unit-testable, see mcp_test.go) from buildMCPManager's loop
// since MCPManager itself keeps its per-server config unexported. spec's
// ConnectTimeoutS (integer seconds; <= 0/absent means "use the engine
// default") threads straight into ConnectTimeout as a time.Duration.
func mcpServerConfig(spec config.MCPServerSpec) engine.MCPServerConfig {
	return engine.MCPServerConfig{
		Command:        spec.Command,
		Env:            spec.Env,
		Dir:            spec.Dir,
		URL:            spec.URL,
		Headers:        spec.Headers,
		ConnectTimeout: time.Duration(spec.ConnectTimeoutS) * time.Second,
	}
}

// mcpRegistry adapts a possibly-nil *engine.MCPManager to engine.MCPRegistry,
// the same typed-nil guard pluginHooks applies to *plugin.Host: assigning a
// typed-nil *engine.MCPManager directly to an engine.MCPRegistry-typed
// field would produce a non-nil interface, so every `cfg.MCP != nil` check
// in the engine would be true for a session with no MCP servers configured
// at all.
func mcpRegistry(mgr *engine.MCPManager) engine.MCPRegistry {
	if mgr == nil {
		return nil
	}
	return mgr
}

// closeMCPManager closes mgr (a no-op if mgr is nil), bounded by
// mcpCloseTimeout.
func closeMCPManager(mgr *engine.MCPManager) {
	if mgr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpCloseTimeout)
	defer cancel()
	_ = mgr.Close(ctx)
}
