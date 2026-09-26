// Native MCP resource tools: list_mcp_resources and read_mcp_resource.
package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/majorcontext/harness/mcp"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// mcpListResourcesToolName and mcpReadResourceToolName are the built-in
// tools' fixed names.
const (
	mcpListResourcesToolName = "list_mcp_resources"
	mcpReadResourceToolName  = "read_mcp_resource"
)

// mcpResourceRegistry is implemented by an MCPRegistry that can also serve
// MCP resources.
type mcpResourceRegistry interface {
	ResourceCapableServers(ctx context.Context) []string
	ListResources(ctx context.Context, server string) (resources []mcp.Resource, truncated bool, err error)
	ReadResource(ctx context.Context, server, uri string) (*mcp.ReadResourceResult, error)
}

// mcpResourceCapableServers reports reg's resource-capable connected server
// names, or nil when reg is absent or does not implement mcpResourceRegistry.
func mcpResourceCapableServers(ctx context.Context, reg MCPRegistry) []string {
	rr, ok := reg.(mcpResourceRegistry)
	if !ok {
		return nil
	}
	return rr.ResourceCapableServers(ctx)
}

// mcpListResourcesTool and mcpReadResourceTool build the two session tools.
func mcpListResourcesTool() Tool {
	return Tool{Def: mcpListResourcesToolDef(), Run: runMCPListResourcesTool}
}

func mcpReadResourceTool() Tool {
	return Tool{Def: mcpReadResourceToolDef(), Run: runMCPReadResourceTool}
}

func mcpListResourcesToolDef() provider.ToolDef {
	return provider.ToolDef{
		Name: mcpListResourcesToolName,
		Description: "List resources served by this session's connected MCP servers. An MCP resource " +
			"is a URI-addressed document a server serves outside its tools (e.g. skill:// guidance a " +
			"server's own instructions may tell you to load before calling its tools). Omit server to " +
			"list across every resource-capable connected server; pass it to list one server only. " +
			"Each server's own listing is capped at 500 resources; a capped server is named in truncated.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"server": {"type": "string", "description": "Restrict the listing to this configured server name"}
			}
		}`),
	}
}

func mcpReadResourceToolDef() provider.ToolDef {
	return provider.ToolDef{
		Name:        mcpReadResourceToolName,
		Description: "Read one MCP resource's contents by server and uri, as reported by list_mcp_resources.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"server": {"type": "string", "description": "The configured server name"},
				"uri": {"type": "string", "description": "The resource URI, exactly as list_mcp_resources reported it"}
			},
			"required": ["server", "uri"]
		}`),
	}
}

// mcpListResourcesArgs is list_mcp_resources' input shape.
type mcpListResourcesArgs struct {
	Server string `json:"server"`
}

// mcpResourceResult is one resource in list_mcp_resources' result.
type mcpResourceResult struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Server      string `json:"server"`
}

// mcpResourceListError is one server's failure in an unfiltered listing.
type mcpResourceListError struct {
	Server string `json:"server"`
	Error  string `json:"error"`
}

type mcpListResourcesResult struct {
	Resources []mcpResourceResult    `json:"resources"`
	Truncated []string               `json:"truncated,omitempty"` // servers capped at mcpResourcesCap
	Errors    []mcpResourceListError `json:"errors,omitempty"`
}

// runMCPListResourcesTool implements list_mcp_resources. A server named
// explicitly fails the whole call on error; an unfiltered listing instead
// collects a per-server error and still returns every other server's
// resources.
func runMCPListResourcesTool(ctx context.Context, s *Session, raw json.RawMessage) (message.Parts, error) {
	var in mcpListResourcesArgs
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("%s: invalid arguments: %w", mcpListResourcesToolName, err)
	}
	rr, ok := s.cfg.MCP.(mcpResourceRegistry)
	if !ok {
		return nil, fmt.Errorf("%s: this session has no MCP resource registry", mcpListResourcesToolName)
	}

	explicit := in.Server != ""
	servers := []string{in.Server}
	if !explicit {
		servers = rr.ResourceCapableServers(ctx)
	}

	out := make([]mcpResourceResult, 0)
	var errs []mcpResourceListError
	var truncated []string
	for _, server := range servers {
		resources, serverTruncated, err := rr.ListResources(ctx, server)
		if err != nil {
			if explicit {
				return nil, fmt.Errorf("%s: %w", mcpListResourcesToolName, err)
			}
			errs = append(errs, mcpResourceListError{Server: server, Error: err.Error()})
			continue
		}
		if serverTruncated {
			truncated = append(truncated, server)
		}
		for _, r := range resources {
			out = append(out, mcpResourceResult{
				URI: r.URI, Name: r.Name, Title: r.Title, Description: r.Description,
				MimeType: r.MimeType, Server: server,
			})
		}
	}
	return jsonResult(mcpListResourcesResult{Resources: out, Truncated: truncated, Errors: errs})
}

// mcpReadResourceArgs is read_mcp_resource's input shape.
type mcpReadResourceArgs struct {
	Server string `json:"server"`
	URI    string `json:"uri"`
}

// runMCPReadResourceTool implements read_mcp_resource: text contents
// verbatim, or a short mimeType/size placeholder for a blob — never a raw
// base64 dump.
func runMCPReadResourceTool(ctx context.Context, s *Session, raw json.RawMessage) (message.Parts, error) {
	var in mcpReadResourceArgs
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("%s: invalid arguments: %w", mcpReadResourceToolName, err)
	}
	if in.Server == "" || in.URI == "" {
		return nil, fmt.Errorf("%s: requires %q and %q arguments", mcpReadResourceToolName, "server", "uri")
	}
	rr, ok := s.cfg.MCP.(mcpResourceRegistry)
	if !ok {
		return nil, fmt.Errorf("%s: this session has no MCP resource registry", mcpReadResourceToolName)
	}

	res, err := rr.ReadResource(ctx, in.Server, in.URI)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", mcpReadResourceToolName, err)
	}

	var parts message.Parts
	for _, c := range res.Contents {
		switch {
		case c.Text != "":
			parts = append(parts, &message.Text{Text: c.Text})
		case c.Blob != "":
			mime := c.MimeType
			if mime == "" {
				mime = "application/octet-stream"
			}
			data, ok := decodeMCPBase64(in.Server, in.URI, c.Blob)
			if !ok {
				parts = append(parts, &message.Text{Text: fmt.Sprintf("[binary resource: %s, malformed payload, %s]", in.URI, mime)})
				continue
			}
			parts = append(parts, &message.Text{Text: fmt.Sprintf("[binary resource: %s, %d bytes, %s]", in.URI, len(data), mime)})
		}
	}
	if len(parts) == 0 {
		parts = message.Parts{&message.Text{Text: ""}}
	}
	return parts, nil
}
