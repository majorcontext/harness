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

const deferDescription = "Inspect this session's configured MCP servers, and run deferred MCP tools. " +
	"Some MCP tools are DEFERRED: the system prompt or an [mcp: ...] notice in the conversation lists their names and one-line descriptions, " +
	"but they are not in your tool list. Actions: " +
	"search(query) ranks deferred and listed tools by keyword over their names and descriptions, " +
	"and reports whether each is in your tool list; " +
	"select(tools) returns the description and input schema of the named deferred tools. Select every tool you need " +
	"in ONE call, and do not select a tool that search reports in your tool list. " +
	"call(tool, arguments) runs a deferred tool with arguments that follow its input schema. " +
	"status() reports every configured server's live connection state — connected, still " +
	"retrying in the background, or parked (background retries exhausted); " +
	"connect(server) makes ONE bounded, synchronous connect attempt for a server that is not " +
	"yet connected — the only way to bring a parked server back once its automatic background " +
	"retries have given up, and the way to discover the tools of a server that has never connected."

const catalogHeader = "Deferred MCP tools. These tools exist but are not in your tool list. " +
	"To use one you MUST first read its definition with the mcp tool: " +
	`mcp(action="select", tools=["mcp__server__tool"]). ` +
	`Then run it with mcp(action="call", tool="mcp__server__tool", arguments={...}). ` +
	`Select every tool you need in ONE call. Use mcp(action="search", query="...") to find a tool by keyword.`

const (
	instructionsMax = 4000
	toolNamesMax    = 2048
	catalogMax      = 200
	cutMarker       = "… [truncated]"
)

const resourcesLine = "This session can also list and read MCP resources with the " + listName + " and " + readName + " tools."

type args struct {
	Action string   `json:"action"`
	Server string   `json:"server"`
	Query  string   `json:"query"`
	Limit  int      `json:"limit"`
	Tools  []string `json:"tools"`
	// Tool and Arguments are the deferred tool that the call action runs.
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// control is the mcp tool of one model call.
type control struct {
	s      *Source
	all    []remote
	loaded map[string]bool
	v      view
	// ok is the allowed-tools filter of the model call.
	ok func(string) bool
	// late returns the plugin tools that the mcp tool loads too. nil: none.
	late func() []Late
}

func (c control) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: controlName, Description: deferDescription, InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"action":{"type":"string","enum":["status","connect","search","select","call"],"description":"The operation to perform"},` +
		`"server":{"type":"string","description":"The configured server name (required for connect)"},` +
		`"query":{"type":"string","description":"Keywords to rank tools by (required for search)"},` +
		`"limit":{"type":"integer","description":"Maximum search results (default 20, max 50)"},` +
		`"tools":{"type":"array","items":{"type":"string"},"description":"Namespaced tool names to load, e.g. mcp__github__create_issue (required for select)"},` +
		`"tool":{"type":"string","description":"The namespaced name of the deferred tool to run (required for call)"},` +
		`"arguments":{"type":"object","description":"The arguments of the tool, as its input schema describes (for call)"}},` +
		`"required":["action"]}`)}
}

// Forward makes the call action run the deferred tool that it names, between the hooks of that tool.
func (control) Forward(call protocol.ToolCall) (protocol.ToolCall, bool) {
	var in args
	if json.Unmarshal(call.Arguments, &in) != nil || in.Action != "call" || in.Tool == "" {
		return call, false
	}
	return protocol.ToolCall{ID: call.ID, Name: in.Tool, Arguments: in.Arguments}, true
}

// Alone makes the mcp tool run with no other call of its model call in flight.
func (control) Alone() {}

func (c control) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	var in args
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("mcp: invalid arguments: %w", err)
	}
	if in.Action == "search" || in.Action == "select" {
		c.v = c.s.view()
		c.all = c.v.liveAll(c.ok)
		if c.late != nil {
			for _, l := range c.late() {
				c.all = append(c.all, remote{server: l.Plugin, name: l.Tool.Spec().Name, spec: l.Tool.Spec(), listed: true})
			}
		}
	}
	var out any
	var err error
	switch {
	case in.Action == "status":
		out = protocol.MCPServers{Servers: c.s.status()}
	case in.Action == "connect":
		out, err = c.connect(ctx, in.Server)
	case in.Action == "search":
		out, err = c.search(in.Query, in.Limit)
	case in.Action == "select":
		out, err = c.choose(in.Tools)
	case in.Action == "call":
		err = fmt.Errorf(`mcp: call requires a "tool" argument`)
	default:
		err = fmt.Errorf(`mcp: unknown action %q (want "status", "connect", "search", "select" or "call")`, in.Action)
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
	if name == "" {
		return nil, fmt.Errorf(`mcp: connect requires a "server" argument`)
	}
	owner := c.s.owner(name)
	if owner == nil {
		return nil, fmt.Errorf("mcp: unknown server %q (configured: %s)", name, strings.Join(c.s.view().names, ", "))
	}
	dialed, err := owner.connectNow(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect for %q failed: %s", name, connectReason(err))
	}
	if !dialed {
		return connected{name, true, "already connected"}, nil
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

type definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type choice struct {
	Tools   []definition `json:"tools"`
	Already []string     `json:"already"`
	Pending []string     `json:"pending"`
	Missing []string     `json:"missing"`
	Note    string       `json:"note"`
}

// choose gives the definition of each deferred tool that names holds, and sorts the other names. A name of a configured server that is not
// connected is pending: it has no definition until the server connects.
func (c control) choose(names []string) (any, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf(`mcp: select requires a non-empty "tools" array`)
	}
	out := choice{Tools: []definition{}, Already: []string{}, Pending: []string{}, Missing: []string{}}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		server, _, _ := strings.Cut(strings.TrimPrefix(n, prefix), "__")
		found := slices.IndexFunc(c.all, func(r remote) bool { return r.spec.Name == n })
		switch {
		case c.loaded[n]:
			out.Already = append(out.Already, n)
		case found >= 0:
			r := c.all[found]
			out.Tools = append(out.Tools, definition{r.spec.Name, r.spec.Description, r.spec.InputSchema})
		case strings.HasPrefix(n, prefix) && slices.Contains(c.v.names, server) && !c.v.live[server].up():
			out.Pending = append(out.Pending, n)
		default:
			out.Missing = append(out.Missing, n)
		}
	}
	switch {
	case len(out.Tools) > 0:
		out.Note = `call a tool with mcp(action="call", tool="<name>", arguments={...})`
		if len(out.Already) > 0 {
			out.Note += "; a tool in already is in your tool list, so call it directly"
		}
		if len(out.Pending) > 0 {
			out.Note += "; the pending tools have no definition until their server reconnects"
		}
	case len(out.Already) > 0:
		out.Note = "every tool you selected is in your tool list: call it directly"
	case len(out.Pending) > 0:
		out.Note = "no definition returned: every name you selected belongs to a server that is not connected. See the mcp tool's status and connect actions"
	default:
		out.Note = "no tool matched"
	}
	return out, nil
}

// catalog lists the first catalogMax deferred tools with the first line of
// each description, and counts the rest.
func catalog(deferred []remote) string {
	if len(deferred) == 0 {
		return ""
	}
	return catalogHeader + "\n" + toolLines(deferred)
}

// toolLines lists the first catalogMax tools, one on each line, and counts the rest.
func toolLines(deferred []remote) string {
	shown := deferred[:min(len(deferred), catalogMax)]
	var b strings.Builder
	for _, r := range shown {
		b.WriteString("\n" + r.spec.Name)
		if d := line(r.spec.Description); d != "" {
			b.WriteString(" — " + d)
		}
	}
	if rest := len(deferred) - len(shown); rest > 0 {
		fmt.Fprintf(&b, "\n... and %d more tools; use mcp(action=\"search\", query=\"...\") to find them", rest)
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

// cutRunes cuts s to at most n runes and marks a cut.
func cutRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + cutMarker
		}
		count++
	}
	return s
}

// listNames joins names, and stops before the list passes toolNamesMax bytes.
// Any 200 names need more than that bound, so it also bounds their count.
func listNames(names []string) string {
	var b strings.Builder
	for i, n := range names {
		need := len(n)
		if i > 0 {
			need += 2
		}
		if b.Len()+need > toolNamesMax {
			return b.String() + " names truncated at byte budget"
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(n)
	}
	return b.String()
}
