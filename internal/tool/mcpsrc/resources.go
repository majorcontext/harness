package mcpsrc

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/mcp"
	"github.com/majorcontext/harness/protocol"
)

// maxResources caps the listing of one server, so a large server cannot
// fill the context.
const maxResources = 500

type lister struct{ s *Source }

type reader struct{ s *Source }

func (lister) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: listName, Description: "List resources served by this session's connected MCP servers. An MCP resource " +
		"is a URI-addressed document a server serves outside its tools (e.g. skill:// guidance a " +
		"server's own instructions may tell you to load before calling its tools). Omit server to " +
		"list across every resource-capable connected server; pass it to list one server only. " +
		"Each server's own listing is capped at 500 resources; a capped server is named in truncated.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"server":{"type":"string","description":"Restrict the listing to this configured server name"}}}`)}
}

func (reader) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: readName, Description: "Read one MCP resource's contents by server and uri, as reported by list_mcp_resources.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"server":{"type":"string","description":"The configured server name"},` +
			`"uri":{"type":"string","description":"The resource URI, exactly as list_mcp_resources reported it"}},"required":["server","uri"]}`)}
}

type resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Server      string `json:"server"`
}

type listErr struct {
	Server string `json:"server"`
	Error  string `json:"error"`
}

func (l lister) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	var in struct{ Server string }
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("%s: invalid arguments: %w", listName, err)
	}
	servers := []string{in.Server}
	if in.Server == "" {
		v := l.s.view()
		servers = slices.DeleteFunc(slices.Clone(v.names), func(n string) bool { return !v.live[n].resources() })
	}
	var out struct {
		Resources []resource `json:"resources"`
		Truncated []string   `json:"truncated,omitempty"`
		Errors    []listErr  `json:"errors,omitempty"`
	}
	out.Resources = []resource{}
	for _, name := range servers {
		list, cut, err := l.s.list(ctx, name)
		switch {
		case err != nil && in.Server != "":
			return protocol.ToolResult{}, fmt.Errorf("%s: %w", listName, err)
		case err != nil:
			out.Errors = append(out.Errors, listErr{name, err.Error()})
		case cut:
			out.Truncated = append(out.Truncated, name)
		}
		for _, r := range list {
			out.Resources = append(out.Resources, resource{r.URI, r.Name, r.Title, r.Description, r.MimeType, name})
		}
	}
	b, err := json.Marshal(out)
	return protocol.ToolResult{Text: string(b)}, err
}

// list returns the resources of server, at most maxResources, and whether it cut the list.
func (s *Source) list(ctx context.Context, name string) ([]mcp.Resource, bool, error) {
	c, life, err := s.resourceClient(name)
	if err != nil {
		return nil, false, err
	}
	ctx, stop := bindLife(ctx, life)
	defer stop()
	ctx = s.sessionHeader(ctx, name)
	var all []mcp.Resource
	seen := map[string]bool{}
	for cursor := ""; ; {
		page, err := c.ListResources(ctx, cursor)
		if err != nil {
			return nil, false, hide(name, err)
		}
		all = append(all, page.Resources...)
		switch {
		case len(all) > maxResources:
			return all[:maxResources], true, nil
		case page.NextCursor == "":
			return all, false, nil
		case seen[page.NextCursor]:
			return nil, false, fmt.Errorf("mcp: server %q: resources/list repeated cursor %q", name, page.NextCursor)
		}
		seen[page.NextCursor], cursor = true, page.NextCursor
	}
}

func (s *Source) resourceClient(name string) (*mcp.Client, context.Context, error) {
	v := s.view()
	sv := v.live[name]
	switch {
	case !slices.Contains(v.names, name):
		return nil, nil, fmt.Errorf("mcp: server %q is not configured", name)
	case !sv.up():
		return nil, nil, fmt.Errorf("mcp: server %q is not connected", name)
	case !sv.resources():
		return nil, nil, fmt.Errorf("mcp: server %q does not serve resources", name)
	}
	return sv.client, v.life[name], nil
}

// bindLife returns ctx ended also when life ends, which is when the server goes.
func bindLife(ctx, life context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(life, cancel)
	return ctx, func() { stop(); cancel() }
}

func (r reader) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	var in struct{ Server, URI string }
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("%s: invalid arguments: %w", readName, err)
	}
	if in.Server == "" || in.URI == "" {
		return protocol.ToolResult{}, fmt.Errorf(`%s: requires "server" and "uri" arguments`, readName)
	}
	c, life, err := r.s.resourceClient(in.Server)
	if err != nil {
		return protocol.ToolResult{}, fmt.Errorf("%s: %w", readName, err)
	}
	ctx, stop := bindLife(ctx, life)
	defer stop()
	ctx = r.s.sessionHeader(ctx, in.Server)
	if res, handled, err := r.s.skillRead(ctx, c, in.Server, in.URI); handled {
		if err != nil {
			return protocol.ToolResult{}, fmt.Errorf("%s: %w", readName, err)
		}
		return res, nil
	}
	res, err := c.ReadResource(ctx, in.URI)
	if err != nil {
		return protocol.ToolResult{}, fmt.Errorf("%s: %w", readName, hide(in.Server, err))
	}
	var lines []string
	for _, c := range res.Contents {
		if c.Blob != nil && c.Text == "" {
			lines = append(lines, binary("binary resource: "+in.URI, *c.Blob, c.MimeType))
		} else {
			lines = append(lines, c.Text)
		}
	}
	return protocol.ToolResult{Text: strings.Join(lines, "\n")}, nil
}
