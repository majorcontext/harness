package harness

import (
	"fmt"
	"maps"
	"slices"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/tool/mcpsrc"
)

// SessionOption changes how Create or Open starts one session. It is Go-only:
// no protocol field carries it, and no HTTP client can set it.
type SessionOption func(*sessionOptions)

type sessionOptions struct {
	mcp           map[string]config.MCPServerSpec
	mcpAuthFailed func(server string, status int)
	allowedTools  *[]string
	maxSteps      *int
}

// WithAllowedTools replaces the recorded tool allowlist on Open; nil allows all tools, and an empty list allows none.
func WithAllowedTools(names []string) SessionOption {
	own := slices.Clone(names)
	return func(o *sessionOptions) { o.allowedTools = &own }
}

// WithMaxSteps replaces the recorded cap of model requests for each turn on Open; zero clears the cap, and a negative value is invalid.
func WithMaxSteps(n int) SessionOption {
	return func(o *sessionOptions) { o.maxSteps = &n }
}

// WithSessionMCP gives the session MCP servers beside the servers of Config.MCPServers. The map key
// names a server, as in Config.MCPServers. The runtime connects a server at the first model call of
// the session and stops it when the session stops. Nothing persists: not the specs and not the
// secrets in their env and headers, so a later Open has the servers only when it passes this option
// again. An Open of a session that this runtime already runs keeps the servers that it has.
func WithSessionMCP(servers map[string]config.MCPServerSpec) SessionOption {
	own := make(map[string]config.MCPServerSpec, len(servers))
	for name, sp := range servers {
		sp.Command, sp.Env, sp.Headers = slices.Clone(sp.Command), slices.Clone(sp.Env), maps.Clone(sp.Headers)
		if sp.ToolTimeoutS != nil {
			sp.ToolTimeoutS = &[]int{*sp.ToolTimeoutS}[0]
		}
		own[name] = sp
	}
	return func(o *sessionOptions) { o.mcp = own }
}

// WithSessionMCPAuthFailed calls fn with the server name and the status when a server of WithSessionMCP answers
// 401 or 403 to initialize, tools/list, tools/call, or a resource or skill request. A server that fails to
// connect is retried and then parked as before, so fn can run more than once for one server. Each call runs on
// its own goroutine, so fn must be safe for concurrent use. It must not stop its own session: the end of a session waits for it. It carries no body and no header. A runtime server of
// Config.MCPServers never calls it. Nothing persists: a later Open has fn only when it passes this option again,
// and an Open of a session that this runtime already runs keeps the first fn.
func WithSessionMCPAuthFailed(fn func(server string, status int)) SessionOption {
	return func(o *sessionOptions) { o.mcpAuthFailed = fn }
}

// sessionOptions applies opts and checks the session servers: each must meet the rules of
// Config.MCPServers, and none may take the name of a runtime server.
func (r *Runtime) sessionOptions(opts []SessionOption) (sessionOptions, error) {
	var so sessionOptions
	for _, o := range opts {
		o(&so)
	}
	if len(so.mcp) == 0 {
		return so, nil
	}
	if err := (&config.Config{MCPServers: so.mcp}).Validate(); err != nil {
		return so, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	for name := range so.mcp {
		if r.mcp.Has(name) {
			return so, fmt.Errorf("%w: session MCP server %q has the name of a runtime server", ErrInvalidRequest, name)
		}
	}
	return so, nil
}

// sessionSource returns the source of the session servers, or nil for none.
func (r *Runtime) sessionSource(servers map[string]config.MCPServerSpec, authFailed func(server string, status int)) *mcpsrc.Source {
	if len(servers) == 0 {
		return nil
	}
	return mcpsrc.NewSession(r.mcpCfg, r.mcp, servers, r.workDir, r.log, authFailed, mcpsrc.Detach(r.detach))
}

// detach runs f on a goroutine that Close waits for. After Close started it runs f before it returns.
func (r *Runtime) detach(f func()) {
	if r.hold() != nil {
		f()
		return
	}
	go func() {
		defer r.group.Done()
		f()
	}()
}

// checkSessionTools fails when a session that has MCP servers would share a tool name with an
// embedder tool or a started plugin tool: the names of the MCP tools are reserved for it.
func (r *Runtime) checkSessionTools(servers map[string]config.MCPServerSpec) error {
	if len(servers) == 0 {
		return nil
	}
	names := r.plugins.Names()
	for _, t := range r.tools {
		names = append(names, t.Spec().Name)
	}
	for _, n := range names {
		if mcpsrc.Reserved(n) {
			return fmt.Errorf("%w: tool %q has a name that session MCP servers reserve", ErrInvalidRequest, n)
		}
	}
	return nil
}
