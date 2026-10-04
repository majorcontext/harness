package mcpsrc

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/majorcontext/harness/protocol"
)

const connectDescription = "Connect this session's configured MCP servers. " +
	"connect(server) makes one bounded connect attempt for a server that is not connected. " +
	"A connected server is a no-op. An unknown server name fails and lists the configured names."

const deferDescription = "Load the schemas of deferred MCP tools, and connect this session's configured MCP servers. " +
	"Some MCP tools are DEFERRED: the system prompt lists their names and one-line descriptions, " +
	"but their input schemas are not loaded and you cannot call them yet. Actions: " +
	"search(query) ranks deferred and loaded tools by keyword over their names and descriptions, " +
	"and reports whether each is already loaded; " +
	"select(tools) loads the schemas of the named tools. They appear in your tool list on the " +
	"next request and are then called directly, like any other tool. Select every tool you need " +
	"in ONE call, and do not select a tool that search reports as loaded. " +
	"connect(server) makes one bounded connect attempt for a server that is not connected, " +
	"which also discovers its tools."

const catalogHeader = "Deferred MCP tools. These tools exist but their input schemas are not loaded. " +
	"To use one you MUST first load it with the mcp tool: " +
	`mcp(action="select", tools=["mcp__server__tool"]). ` +
	"A selected tool appears in your tool list on the next request and is then called directly. " +
	`Select every tool you need in ONE call. Use mcp(action="search", query="...") to find a tool by keyword.`

const resourcesLine = "This session can also list and read MCP resources with the " + listName + " and " + readName + " tools."

type args struct {
	Action string   `json:"action"`
	Server string   `json:"server"`
	Query  string   `json:"query"`
	Limit  int      `json:"limit"`
	Tools  []string `json:"tools"`
}

// control is the mcp tool of one model call.
type control struct {
	s       *Source
	all     []remote
	loaded  map[string]bool
	servers map[string]*server
}

func (c control) Spec() protocol.ToolSpec {
	if !c.s.defers {
		return protocol.ToolSpec{Name: controlName, Description: connectDescription, InputSchema: json.RawMessage(`{"type":"object",` +
			`"properties":{"action":{"type":"string","enum":["connect"]},"server":{"type":"string","description":"The configured server name"}},"required":["action"]}`)}
	}
	return protocol.ToolSpec{Name: controlName, Description: deferDescription, InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"action":{"type":"string","enum":["connect","search","select"]},` +
		`"server":{"type":"string","description":"The configured server name (required for connect)"},` +
		`"query":{"type":"string","description":"Keywords to rank tools by (required for search)"},` +
		`"limit":{"type":"integer","description":"Maximum search results (default 20, max 50)"},` +
		`"tools":{"type":"array","items":{"type":"string"},"description":"Namespaced tool names to load, e.g. mcp__github__create_issue (required for select)"}},` +
		`"required":["action"]}`)}
}

func (c control) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	var in args
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("mcp: invalid arguments: %w", err)
	}
	var out any
	var err error
	switch {
	case in.Action == "connect":
		out, err = c.connect(ctx, in.Server)
	case in.Action == "search" && c.s.defers:
		out, err = c.search(in.Query, in.Limit)
	case in.Action == "select" && c.s.defers:
		out, err = c.choose(in.Tools)
	case c.s.defers:
		err = fmt.Errorf(`mcp: unknown action %q (want "connect", "search" or "select")`, in.Action)
	default:
		err = fmt.Errorf(`mcp: unknown action %q (want "connect")`, in.Action)
	}
	if err != nil {
		return protocol.ToolResult{}, err
	}
	b, err := json.Marshal(out)
	return protocol.ToolResult{Text: string(b)}, err
}

type connected struct {
	Server    string `json:"server"`
	Connected bool   `json:"connected"`
	Message   string `json:"message"`
}

func (c control) connect(ctx context.Context, name string) (any, error) {
	switch {
	case name == "":
		return nil, fmt.Errorf(`mcp: connect requires a "server" argument`)
	case !slices.Contains(c.s.names, name):
		return nil, fmt.Errorf("mcp: unknown server %q (configured: %s)", name, strings.Join(c.s.names, ", "))
	case c.s.server(name).up():
		return connected{name, true, "already connected"}, nil
	}
	if err := c.s.connect(ctx, name); err != nil {
		return nil, fmt.Errorf("mcp: connect for %q failed: %s", name, reason(err))
	}
	return connected{name, true, "connected"}, nil
}

type match struct {
	Name        string `json:"name"`
	Server      string `json:"server"`
	Description string `json:"description,omitempty"`
	Loaded      bool   `json:"loaded"`
}

// search ranks the tools by the tokens of query: an exact name scores 100,
// a token in the tool name 50, in the description 10, in the server name 5.
func (c control) search(query string, limit int) (any, error) {
	tokens := slices.Compact(slices.Sorted(slices.Values(strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}))))
	if len(tokens) == 0 {
		return nil, fmt.Errorf(`mcp: search requires a non-empty "query" argument`)
	}
	whole := strings.ToLower(strings.TrimSpace(query))
	type hit struct {
		r     remote
		score int
	}
	var hits []hit
	for _, r := range c.all {
		name, server, desc := strings.ToLower(r.name), strings.ToLower(r.server), strings.ToLower(r.spec.Description)
		score := 0
		if whole == name || whole == strings.ToLower(r.spec.Name) {
			score += 100
		}
		for _, t := range tokens {
			for i, field := range []string{name, desc, server} {
				if strings.Contains(field, t) {
					score += []int{50, 10, 5}[i]
				}
			}
		}
		if score > 0 {
			hits = append(hits, hit{r, score})
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return cmp.Or(b.score-a.score, strings.Compare(a.r.spec.Name, b.r.spec.Name)) })
	limit = min(cmp.Or(max(limit, 0), 20), 50)
	matches := []match{}
	for _, h := range hits[:min(limit, len(hits))] {
		matches = append(matches, match{h.r.spec.Name, h.r.server, line(h.r.spec.Description), c.loaded[h.r.spec.Name]})
	}
	return struct {
		Matches   []match `json:"matches"`
		Total     int     `json:"total"`
		Truncated bool    `json:"truncated"`
	}{matches, len(hits), len(hits) > len(matches)}, nil
}

type choice struct {
	Selected []string `json:"selected"`
	Already  []string `json:"already"`
	Pending  []string `json:"pending"`
	Missing  []string `json:"missing"`
	Note     string   `json:"note"`
}

// choose sorts each name. A name of a configured server that is not
// connected is pending: the log holds it, so it loads once the server connects.
func (c control) choose(names []string) (any, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf(`mcp: select requires a non-empty "tools" array`)
	}
	out := choice{Selected: []string{}, Already: []string{}, Pending: []string{}, Missing: []string{}}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		server, _, _ := strings.Cut(strings.TrimPrefix(n, prefix), "__")
		switch {
		case c.loaded[n]:
			out.Already = append(out.Already, n)
		case slices.ContainsFunc(c.all, func(r remote) bool { return r.spec.Name == n }):
			out.Selected = append(out.Selected, n)
		case strings.HasPrefix(n, prefix) && slices.Contains(c.s.names, server) && !c.servers[server].up():
			out.Pending = append(out.Pending, n)
		default:
			out.Missing = append(out.Missing, n)
		}
	}
	loaded := len(out.Selected)+len(out.Already) > 0
	switch {
	case loaded && len(out.Pending) > 0:
		out.Note = "the loaded tools are callable from the next request in this turn; the pending ones load once their server connects"
	case loaded:
		out.Note = "selected tools are callable from the next request in this turn"
	case len(out.Pending) > 0:
		out.Note = "no tool was loaded: every name you selected belongs to a server that is not connected. They load once that server connects; see the mcp tool's connect action"
	default:
		out.Note = "no tool was loaded"
	}
	return out, nil
}

// catalog lists the deferred tools with the first line of each description.
func catalog(deferred []remote) string {
	if len(deferred) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(catalogHeader + "\n")
	for _, r := range deferred {
		b.WriteString("\n" + r.spec.Name)
		if d := line(r.spec.Description); d != "" {
			b.WriteString(" — " + d)
		}
	}
	return b.String()
}

// line returns the first line of d, cut at 160 bytes.
func line(d string) string {
	d, _, _ = strings.Cut(d, "\n")
	d = strings.TrimSpace(d)
	if len(d) <= 160 {
		return d
	}
	d = d[:157]
	for !utf8.ValidString(d) {
		d = d[:len(d)-1]
	}
	return d + "..."
}

// instructions is the segment of the reached servers: each one's own
// instructions with its allowed tool names, and a line about the resource tools.
func (s *Source) instructions(servers map[string]*server, reached []string, ok func(string) bool) string {
	tag := strings.NewReplacer("<mcp_instructions>", "(mcp_instructions)", "</mcp_instructions>", "(/mcp_instructions)",
		"<server", "(server", "</server>", "(/server)")
	attr := func(v string) string { return strings.ReplaceAll(tag.Replace(v), `"`, "'") }
	var b strings.Builder
	for _, name := range reached {
		sv := servers[name]
		text := strings.TrimSpace(sv.client.Instructions())
		if text == "" {
			continue
		}
		var tools []string
		for _, t := range sv.tools {
			if n := prefix + name + "__" + t.Name; ok(n) {
				tools = append(tools, attr(n))
			}
		}
		fmt.Fprintf(&b, "\n<server name=\"%s\" tools=\"%s\">\n%s\n</server>", attr(name), strings.Join(tools, ", "), tag.Replace(text))
	}
	if slices.ContainsFunc(reached, func(n string) bool { return servers[n].resources() }) {
		return "<mcp_instructions>\n" + resourcesLine + b.String() + "\n</mcp_instructions>"
	}
	if b.Len() == 0 {
		return ""
	}
	return "<mcp_instructions>" + b.String() + "\n</mcp_instructions>"
}
