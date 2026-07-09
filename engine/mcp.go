// MCP (Model Context Protocol) client integration: connecting to configured
// MCP servers, registering their tools on a session's tool list, and
// routing both engine-driven tool calls and plugin-initiated
// client/mcp.call requests through the same connected clients.
//
// MCPServerConfig/MCPManager mirror the plugin Host's shape deliberately:
// exactly like plugin.Host, an *MCPManager is built once per process (see
// cmd/harness) and shared across every session via Config.MCP, not
// reconnected per session. "When a session starts, connect to each
// configured MCP server" (see the config package doc) is therefore true in
// the same lazy sense NewSession's own doc comment promises for provider
// auth and plugin spawns: nothing touches the network until first use —
// here, a session's first Prompt calling Tools() or CallTool() — and the
// connection is then cached for the rest of the process's life, not
// reattempted per session.
//
// A server that fails to connect (dial error, non-2xx, malformed
// handshake) or fails tools/list is logged and skipped: fail-open, the
// same philosophy as a crashed plugin (see plugin/PROTOCOL.md) — one bad
// server must never prevent a session from starting or take down an
// otherwise-healthy set of tools.
package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/mcp"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// mcpToolPrefix namespaces every MCP-provided tool name: mcp__<server>__<tool>,
// the Claude Code convention.
const mcpToolPrefix = "mcp__"

// mcpToolName builds the namespaced tool name for a server+tool pair.
func mcpToolName(server, tool string) string {
	return mcpToolPrefix + server + "__" + tool
}

// isMCPToolName reports whether name looks like a namespaced MCP tool name.
func isMCPToolName(name string) bool {
	return strings.HasPrefix(name, mcpToolPrefix)
}

// defaultMCPConnectTimeout bounds Initialize+ListAllTools for one server
// when MCPServerConfig.ConnectTimeout is unset. It exists on top of
// mcp.Client's own per-request timeout (30s default) as an outer,
// per-server bound on the whole connect step (initialize + however many
// tools/list pages it takes).
const defaultMCPConnectTimeout = 15 * time.Second

// MCPServerConfig configures one MCP server a session's MCPManager connects
// to. Exactly one of Command (a stdio server) or URL (a Streamable HTTP
// server) must be set — see config.MCPServerSpec, which this mirrors
// one-for-one for the cmd/harness wiring that builds it from config.
type MCPServerConfig struct {
	// Command is the argv of a stdio MCP server process.
	Command []string
	// Env is appended to the harness environment when the stdio server is
	// spawned.
	Env []string
	// Dir is the stdio server process's working directory.
	Dir string
	// URL is a Streamable HTTP MCP server's endpoint.
	URL string
	// Headers are static headers sent on every request to a Streamable
	// HTTP server.
	Headers map[string]string
	// ConnectTimeout bounds Initialize+ListAllTools for this server; <= 0
	// defaults to defaultMCPConnectTimeout.
	ConnectTimeout time.Duration
}

// MCPRegistry is the slice of MCP client integration a Session needs: the
// current namespaced tool list, routing for a namespaced tool call, and
// routing for a plugin-initiated client/mcp.call naming a server and tool
// directly. *MCPManager is the production implementation, built once per
// process (like plugin.Host) and shared across every session via
// Config.MCP; tests use fakes. A nil Config.MCP disables MCP entirely —
// toolDefs contributes nothing and no tool name is recognized as an MCP
// tool.
type MCPRegistry interface {
	// Tools returns the current namespaced tool defs (mcp__<server>__<tool>),
	// connecting to any not-yet-connected configured server on first call.
	// A server that fails to connect or list tools is skipped, never
	// causing this call itself to fail.
	Tools(ctx context.Context) []provider.ToolDef
	// CallTool routes a namespaced tool call (as returned by Tools) to the
	// underlying server. isErr distinguishes a tool-level failure
	// (CallToolResult.IsError) from err, a protocol/connectivity failure.
	CallTool(ctx context.Context, name string, args json.RawMessage) (out message.Parts, isErr bool, err error)
	// CallServerTool routes an explicit server+tool call (unnamespaced),
	// for plugin-initiated client/mcp.call requests (see
	// plugin.ClientAPI.MCPCall and Session.MCPCall).
	CallServerTool(ctx context.Context, server, tool string, args json.RawMessage) (out message.Parts, isErr bool, err error)
}

// mcpToolBinding records how a namespaced tool name maps back to its
// server and remote (unnamespaced) tool name.
type mcpToolBinding struct {
	server string
	remote string
	def    provider.ToolDef
}

// MCPManager is the production MCPRegistry: it owns one mcp.Client per
// configured server, connecting (and listing tools) lazily and exactly
// once, caching the result — success or failure — for the rest of the
// manager's life. Safe for concurrent use.
type MCPManager struct {
	servers map[string]MCPServerConfig

	connectOnce sync.Once

	mu           sync.RWMutex
	clients      map[string]*mcp.Client    // only servers that connected successfully
	tools        map[string]mcpToolBinding // namespaced name -> binding
	toolsOrdered []provider.ToolDef
}

// NewMCPManager builds an MCPManager for the given servers. Nothing touches
// the network here — connecting happens lazily on the first call to Tools
// or CallTool/CallServerTool (see the package doc).
func NewMCPManager(servers map[string]MCPServerConfig) *MCPManager {
	return &MCPManager{servers: servers}
}

// ensureConnected connects to every configured server exactly once,
// concurrently, each bounded by its own ConnectTimeout. A server that fails
// is logged and simply contributes no client/tools; it never causes this
// (or any other server's) connect to fail.
func (m *MCPManager) ensureConnected(ctx context.Context) {
	m.connectOnce.Do(func() {
		clients := make(map[string]*mcp.Client, len(m.servers))
		tools := make(map[string]mcpToolBinding)

		names := make([]string, 0, len(m.servers))
		for name := range m.servers {
			names = append(names, name)
		}
		sort.Strings(names) // deterministic registration/log order

		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, name := range names {
			name, spec := name, m.servers[name]
			wg.Add(1)
			go func() {
				defer wg.Done()
				client, toolList, err := connectMCPServer(ctx, name, spec)
				if err != nil {
					log.Printf("engine: mcp server %q: %v (continuing without its tools)", name, err)
					return
				}
				mu.Lock()
				clients[name] = client
				for _, t := range toolList {
					full := mcpToolName(name, t.Name)
					tools[full] = mcpToolBinding{
						server: name,
						remote: t.Name,
						def: provider.ToolDef{
							Name:        full,
							Description: t.Description,
							InputSchema: t.InputSchema,
						},
					}
				}
				mu.Unlock()
			}()
		}
		wg.Wait()

		ordered := make([]provider.ToolDef, 0, len(tools))
		orderedNames := make([]string, 0, len(tools))
		for name := range tools {
			orderedNames = append(orderedNames, name)
		}
		sort.Strings(orderedNames)
		for _, name := range orderedNames {
			ordered = append(ordered, tools[name].def)
		}

		m.mu.Lock()
		m.clients = clients
		m.tools = tools
		m.toolsOrdered = ordered
		m.mu.Unlock()
	})
}

// connectMCPServer builds the right transport for spec, opens a client,
// performs the initialize handshake, and lists all its tools — all bounded
// by spec.ConnectTimeout (or defaultMCPConnectTimeout).
func connectMCPServer(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
	var tr mcp.Transport
	switch {
	case len(spec.Command) > 0:
		tr = &mcp.StdioTransport{Command: spec.Command, Env: spec.Env, Dir: spec.Dir}
	case spec.URL != "":
		tr = &mcp.HTTPTransport{Endpoint: spec.URL, Headers: spec.Headers}
	default:
		return nil, nil, fmt.Errorf("neither command nor url configured")
	}

	client, err := mcp.NewClient(tr, mcp.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}

	timeout := spec.ConnectTimeout
	if timeout <= 0 {
		timeout = defaultMCPConnectTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := client.Initialize(cctx); err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("initialize: %w", err)
	}
	tools, err := client.ListAllTools(cctx)
	if err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("tools/list: %w", err)
	}
	return client, tools, nil
}

// Tools implements MCPRegistry.
func (m *MCPManager) Tools(ctx context.Context) []provider.ToolDef {
	m.ensureConnected(ctx)
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]provider.ToolDef(nil), m.toolsOrdered...)
}

// CallTool implements MCPRegistry.
func (m *MCPManager) CallTool(ctx context.Context, name string, args json.RawMessage) (message.Parts, bool, error) {
	m.ensureConnected(ctx)
	m.mu.RLock()
	binding, ok := m.tools[name]
	m.mu.RUnlock()
	if !ok {
		return nil, false, fmt.Errorf("engine: mcp: unknown tool %q", name)
	}
	return m.callTool(ctx, binding.server, binding.remote, args)
}

// CallServerTool implements MCPRegistry.
func (m *MCPManager) CallServerTool(ctx context.Context, server, tool string, args json.RawMessage) (message.Parts, bool, error) {
	m.ensureConnected(ctx)
	return m.callTool(ctx, server, tool, args)
}

func (m *MCPManager) callTool(ctx context.Context, server, tool string, args json.RawMessage) (message.Parts, bool, error) {
	m.mu.RLock()
	client, ok := m.clients[server]
	m.mu.RUnlock()
	if !ok {
		return nil, false, fmt.Errorf("engine: mcp: server %q is not configured or failed to connect", server)
	}

	var argVal any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argVal); err != nil {
			return nil, false, fmt.Errorf("engine: mcp: invalid arguments for %s: %w", tool, err)
		}
	}
	res, err := client.CallTool(ctx, tool, argVal)
	if err != nil {
		return nil, false, err
	}
	return mcpContentToParts(res.Content), res.IsError, nil
}

// mcpCloseTimeout bounds the whole Close, on top of the bounded timeouts
// each mcp.Client.Close already self-enforces (see that method's doc
// comment) — a defensive outer bound so a pathological client
// implementation can't wedge shutdown indefinitely.
const mcpCloseTimeout = 10 * time.Second

// Close closes every connected client concurrently, bounded by
// mcpCloseTimeout. Safe to call even if no server was ever connected.
func (m *MCPManager) Close(ctx context.Context) error {
	m.mu.RLock()
	clients := make([]*mcp.Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.mu.RUnlock()
	if len(clients) == 0 {
		return nil
	}

	done := make(chan error, len(clients))
	for _, c := range clients {
		c := c
		go func() { done <- c.Close() }()
	}

	cctx, cancel := context.WithTimeout(ctx, mcpCloseTimeout)
	defer cancel()
	var firstErr error
	for i := 0; i < len(clients); i++ {
		select {
		case err := <-done:
			if err != nil && firstErr == nil {
				firstErr = err
			}
		case <-cctx.Done():
			return cctx.Err()
		}
	}
	return firstErr
}

// mcpContentToParts converts MCP tool-result content into message.Parts.
// message.ToolResult.Content may hold Text and Blob parts only, so image
// and audio content becomes Blob, resource content becomes Text (its
// embedded text) or Blob (its embedded base64 blob), and resource links
// become a descriptive Text line. An empty result still yields one empty
// Text part so a ToolResult is never left with zero content parts.
func mcpContentToParts(content []mcp.Content) message.Parts {
	var parts message.Parts
	for _, c := range content {
		switch c.Type {
		case mcp.ContentTypeText:
			parts = append(parts, &message.Text{Text: c.Text})
		case mcp.ContentTypeImage, mcp.ContentTypeAudio:
			parts = append(parts, &message.Blob{MediaType: c.MimeType, Data: decodeMCPBase64(c.Data)})
		case mcp.ContentTypeResource:
			if c.Resource == nil {
				continue
			}
			if c.Resource.Text != "" {
				parts = append(parts, &message.Text{Text: c.Resource.Text})
			} else if c.Resource.Blob != "" {
				parts = append(parts, &message.Blob{MediaType: c.Resource.MimeType, Data: decodeMCPBase64(c.Resource.Blob)})
			}
		case mcp.ContentTypeResourceLink:
			parts = append(parts, &message.Text{Text: fmt.Sprintf("resource: %s (%s)", c.URI, c.Name)})
		default:
			if c.Text != "" {
				parts = append(parts, &message.Text{Text: c.Text})
			}
		}
	}
	if len(parts) == 0 {
		parts = message.Parts{&message.Text{Text: ""}}
	}
	return parts
}

func decodeMCPBase64(s string) []byte {
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return data
}
