// The `mcp` session tool: status introspection and on-demand connect for
// MCP servers, the explicit re-trigger past retryServer's bounded
// background schedule (see mcp.go's package doc and
// docs/plans/2026-07-20-mcp-bounded-retry.md). Template: goal_tool.go — the
// same Tool{Def,Run} shape, action schema, and JSON-result convention.
//
// Two actions only: status (read-only, every configured server's live
// connection state) and connect (one bounded, synchronous attempt for a
// named server). There is no "disconnect" or "list" — status already
// enumerates every configured server, disconnecting a healthy client has no
// use case here, and every model-visible string is classified (see
// classifyMCPConnectError) — the #82 leak rule applies to this surface too.
//
// Registered in newSession only when the session's MCP registry reports at
// least one configured server (via the narrow mcpConfigReader interface,
// ConfiguredNames — a cheap, non-connecting read of the manager's static
// config, never triggering a connect attempt itself): a session with no MCP
// servers at all gets no `mcp` tool, exactly like a nil Config.Processes
// installs no `process` tool.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// mcpSessionToolName is the built-in tool's fixed name. Named distinctly
// from mcpToolPrefix (the mcp__<server>__<tool> namespace MCP-provided
// tools use — see mcp.go) since this IS one of this session's own built-in
// tools, not a namespaced passthrough to a remote server.
const mcpSessionToolName = "mcp"

// mcpConfigReader is implemented by an MCPRegistry that can report its
// configured server names without connecting to any of them — *MCPManager
// via ConfiguredNames (see mcp.go). Narrow and separate from MCPRegistry
// itself for the same reason mcp_status.go's mcpStatusReader is: the public
// MCPRegistry contract is used to build test fakes outside this package
// (cmd/harness, server), and growing it would force every one of those to
// add a new method just to keep compiling.
type mcpConfigReader interface {
	ConfiguredNames() []string
}

// mcpConnector is implemented by an MCPRegistry that can perform an
// on-demand, single, synchronous connect attempt for one named server —
// *MCPManager via Connect (see mcp.go). Same narrow-interface reasoning as
// mcpConfigReader.
type mcpConnector interface {
	Connect(ctx context.Context, name string) error
}

// mcpToolArgs is the mcp tool's input shape.
type mcpToolArgs struct {
	Action string `json:"action"`
	Server string `json:"server"`
}

// mcpServerStatusResult is one server's entry in the status action's
// result. Reason is only ever classifyMCPConnectError's output (never a
// raw error) and is omitted entirely for a connected server, which has
// nothing to report.
type mcpServerStatusResult struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Attempts  int    `json:"attempts"`
	Parked    bool   `json:"parked"`
	Reason    string `json:"reason,omitempty"`
}

// mcpToolStatusResult is the status action's result payload.
type mcpToolStatusResult struct {
	Servers []mcpServerStatusResult `json:"servers"`
}

// mcpToolConnectResult is the connect action's result payload.
type mcpToolConnectResult struct {
	Server    string `json:"server"`
	Connected bool   `json:"connected"`
	Message   string `json:"message"`
}

// mcpTool builds the `mcp` session tool. See the package doc for the action
// contract.
func mcpTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name: mcpSessionToolName,
			Description: "Inspect or reconnect this session's configured MCP servers. " +
				"Actions: " +
				"status() reports every configured server's live connection state — connected, " +
				"still retrying in the background, or parked (background retries exhausted); " +
				"connect(server) makes ONE bounded, synchronous connect attempt for a server that " +
				"is not yet connected — the only way to bring a parked server back once its " +
				"automatic background retries have given up. Already-connected is a friendly " +
				"no-op; an unknown server name errors listing the configured names.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"action": {"type": "string", "enum": ["status", "connect"], "description": "The operation to perform"},
					"server": {"type": "string", "description": "The configured server name (required for connect)"}
				},
				"required": ["action"]
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			return runMCPTool(ctx, s, args)
		},
	}
}

// runMCPTool dispatches one mcp tool call against s.
func runMCPTool(ctx context.Context, s *Session, raw json.RawMessage) (message.Parts, error) {
	var in mcpToolArgs
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("mcp: invalid arguments: %w", err)
	}

	switch in.Action {
	case "status":
		return runMCPStatus(s.cfg.MCP)
	case "connect":
		return runMCPConnect(ctx, s.cfg.MCP, in.Server)
	default:
		return nil, fmt.Errorf("mcp: unknown action %q (want %q or %q)", in.Action, "status", "connect")
	}
}

// runMCPStatus implements the status action: every configured server's
// live state, sorted by name (matching MCPManager.Status's own order). A
// registry that doesn't implement mcpStatusReader (or is nil, though
// newSession's gate makes that unreachable in practice — see mcp_tool.go's
// package doc) reports an empty list rather than erroring: status is
// read-only introspection, so "nothing to report" is a safe, honest answer.
func runMCPStatus(reg MCPRegistry) (message.Parts, error) {
	sr, ok := reg.(mcpStatusReader)
	if !ok {
		return jsonResult(mcpToolStatusResult{Servers: []mcpServerStatusResult{}})
	}
	statuses := sr.Status()
	out := make([]mcpServerStatusResult, 0, len(statuses))
	for _, st := range statuses {
		reason := ""
		if !st.Connected {
			// classifyMCPConnectError, never st.LastErr.Error() directly:
			// this becomes model-visible tool output, and a raw connect
			// error can embed the server's endpoint URL (and any secret it
			// carries) — see classifyMCPConnectError's doc comment. A
			// connected server has nothing to classify: its reason stays
			// empty (omitted from the JSON via omitempty).
			reason = classifyMCPConnectError(st.LastErr)
		}
		out = append(out, mcpServerStatusResult{
			Name:      st.Name,
			Connected: st.Connected,
			Attempts:  st.Attempts,
			Parked:    st.Parked,
			Reason:    reason,
		})
	}
	return jsonResult(mcpToolStatusResult{Servers: out})
}

// runMCPConnect implements the connect action: validate server is named and
// configured, short-circuit an already-connected server as a friendly
// no-op, then delegate the actual bounded attempt to MCPManager.Connect (via
// the narrow mcpConnector interface). Every error returned here is either a
// fixed, safe message (unknown server, missing argument, in-progress) or
// routed through classifyMCPConnectError — never a raw connect error — so
// this surface obeys the same #82 leak rule as status and the ambient
// block.
func runMCPConnect(ctx context.Context, reg MCPRegistry, server string) (message.Parts, error) {
	if server == "" {
		return nil, fmt.Errorf("mcp: connect requires a %q argument", "server")
	}

	cr, ok := reg.(mcpConfigReader)
	if !ok {
		return nil, fmt.Errorf("mcp: this session has no connectable MCP registry")
	}
	names := cr.ConfiguredNames()
	if !containsString(names, server) {
		return nil, fmt.Errorf("mcp: unknown server %q (configured: %s)", server, strings.Join(names, ", "))
	}

	if sr, ok := reg.(mcpStatusReader); ok {
		for _, st := range sr.Status() {
			if st.Name == server && st.Connected {
				return jsonResult(mcpToolConnectResult{Server: server, Connected: true, Message: "already connected"})
			}
		}
	}

	connector, ok := reg.(mcpConnector)
	if !ok {
		return nil, fmt.Errorf("mcp: this session's MCP registry does not support on-demand connect")
	}

	if err := connector.Connect(ctx, server); err != nil {
		if errors.Is(err, errMCPConnectInProgress) {
			return nil, fmt.Errorf("mcp: connect for %q: %s", server, errMCPConnectInProgress)
		}
		return nil, fmt.Errorf("mcp: connect for %q failed: %s", server, classifyMCPConnectError(err))
	}
	return jsonResult(mcpToolConnectResult{Server: server, Connected: true, Message: "connected"})
}

// containsString reports whether s is present in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// mcpConfiguredCount is newSession's cheap registration gate: how many
// servers reg is configured with, WITHOUT connecting to any of them —
// mirrors ConfiguredNames' non-connecting contract, just as a count. A nil
// reg, or one that doesn't implement mcpConfigReader, reports 0.
func mcpConfiguredCount(reg MCPRegistry) int {
	if reg == nil {
		return 0
	}
	cr, ok := reg.(mcpConfigReader)
	if !ok {
		return 0
	}
	return len(cr.ConfiguredNames())
}
