package mcpsrc

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// The kinds of the notices of a Source.
const (
	noticeKind  = "mcp"
	addedKind   = "mcp-set"
	loadingKind = "mcp-loading"
)

// allConnected is the text that replaces the down notice when no server is down any more.
const allConnected = "[mcp: every server is connected again.]"

const addedHeader = "[mcp: tools added after the tool list of this session was fixed — %s. They are deferred: to use one you MUST first read its definition with " +
	`mcp(action="select", tools=["mcp__server__tool"]), then run it with mcp(action="call", tool="mcp__server__tool", arguments={...}). Find one with mcp(action="search", query="...").]`

// unavailable renders the notice that names each server that is not
// connected and has failed an attempt, with the reason, or "" when every server is connected.
func (v view) unavailable() string {
	var down []string
	for _, name := range v.names {
		if v.live[name].up() {
			continue
		}
		h := v.health[name]
		if h.attempts == 0 {
			continue
		}
		if h.parked {
			down = append(down, fmt.Sprintf("%s (%s; use the mcp tool action %q to retry)", name, h.reason, "connect"))
		} else {
			down = append(down, fmt.Sprintf("%s (%s; retrying)", name, h.reason))
		}
	}
	if len(down) == 0 {
		return ""
	}
	return "[mcp: unavailable — " + strings.Join(down, ", ") +
		". Tools from these servers are temporarily absent and may return later in this session.]"
}

// cleared is the text that replaces the down notice: allConnected once every server of the view is connected, and
// "" while one is not, so that a server with no attempt yet leaves the recorded notice in place.
func (v view) cleared() string {
	for _, name := range v.names {
		if !v.live[name].up() {
			return ""
		}
	}
	return allConnected
}

// added renders the notice of the live tools that the declaration lacks, with the instructions of a server that the
// prompt does not hold, and the notice of the servers that went. undeclared tells the tools that the declaration
// lacks, and inPrompt the servers whose instructions the prompt holds; nil: none. It is "" when there is nothing to
// say, and gone lists the removals that the declaration recorded, which the notice leaves out; a server that was added again is a new one. The text
// follows the live state, so a later model call renders it again.
func (v view) added(ok func(string) bool, late []Late, undeclared, inPrompt func(string) bool, gone []turn.Gone) string {
	var servers, blocks []string
	for _, name := range v.names {
		var late []remote
		for _, r := range v.remotes(name, ok) {
			if undeclared != nil && undeclared(r.spec.Name) {
				late = append(late, r)
			}
		}
		if len(late) == 0 {
			continue
		}
		servers = append(servers, name)
		var b strings.Builder
		fmt.Fprintf(&b, `<server name="%s">%s`, attr(name), toolLines(late))
		if text := v.serverText(name); text != "" && (inPrompt == nil || !inPrompt(name)) {
			b.WriteString("\n\n" + tag.Replace(text))
		}
		blocks = append(blocks, b.String()+"\n</server>")
	}
	var out []string
	if len(servers) > 0 {
		out = append(out, fmt.Sprintf(addedHeader, strings.Join(servers, ", "))+"\n"+strings.Join(blocks, "\n"))
	}
	if text := addedPlugins(late); text != "" {
		out = append(out, text)
	}
	removed := slices.DeleteFunc(v.removedNames(), func(name string) bool { return slices.Contains(gone, turn.Gone{Name: name, Gen: v.mem[name].gen}) })
	if len(removed) > 0 {
		out = append(out, "[mcp: removed — "+strings.Join(removed, ", ")+". The servers and their tools are gone, and a call to one of those tools fails.]")
	}
	return strings.Join(out, "\n")
}

// removedNames returns the servers that connected once and are not in the live set, in name order.
func (v view) removedNames() []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(v.mem)) {
		if _, ok := v.health[name]; !ok && v.mem[name].up {
			out = append(out, name)
		}
	}
	return out
}

// gone lists the removed servers with their generation.
func (v view) gone() []turn.Gone {
	var out []turn.Gone
	for _, name := range v.removedNames() {
		out = append(out, turn.Gone{Name: name, Gen: v.mem[name].gen})
	}
	return out
}

// status returns the live state of each server, in the order of their names.
func (s *Source) status() []protocol.MCPServerStatus {
	v := s.view()
	out := make([]protocol.MCPServerStatus, 0, len(v.names))
	for _, name := range v.names {
		h := v.health[name]
		out = append(out, protocol.MCPServerStatus{Name: name, Connected: v.live[name].up(), Attempts: h.attempts, Parked: h.parked, Reason: h.reason})
	}
	return out
}

var tag = strings.NewReplacer("<mcp_instructions>", "(mcp_instructions)", "</mcp_instructions>", "(/mcp_instructions)",
	"<server", "(server", "</server>", "(/server)")

func attr(v string) string { return strings.ReplaceAll(tag.Replace(v), `"`, "'") }

// serverOpen is the start of the tag of the server name in the instructions segment.
func serverOpen(name string) string { return fmt.Sprintf(`<server name="%s" tools="`, attr(name)) }

// instructions is the segment of the connected servers: each one's own instructions with its allowed tool names,
// and a line about the resource tools.
func (v view) instructions(ok func(string) bool) string {
	var b strings.Builder
	for _, name := range v.names {
		text := v.serverText(name)
		if text == "" {
			continue
		}
		var tools []string
		for _, t := range v.live[name].toolList() {
			if n := prefix + name + "__" + t.Name; ok(n) {
				tools = append(tools, attr(n))
			}
		}
		fmt.Fprintf(&b, "\n%s%s\">\n%s\n</server>", serverOpen(name), listNames(tools), tag.Replace(text))
	}
	if v.resources() {
		return "<mcp_instructions>\n" + v.common() + b.String() + "\n</mcp_instructions>"
	}
	if b.Len() == 0 {
		return ""
	}
	return "<mcp_instructions>" + b.String() + "\n</mcp_instructions>"
}

// perServer is the guidance of the segment for an external harness, which shows the instructions of each
// server under the name of that server and cuts each one by itself: the line about the resource tools, and
// the instructions of each connected server, cut as in the segment.
func (v view) perServer() turn.MCPInstructions {
	var out turn.MCPInstructions
	if v.resources() {
		out.Common = v.common()
	}
	for _, name := range v.names {
		if !v.live[name].up() {
			continue
		}
		if m := v.mem[name]; m != nil && m.sessionIDHeader {
			if out.SessionIDHeaderServers == nil {
				out.SessionIDHeaderServers = map[string]bool{}
			}
			out.SessionIDHeaderServers[name] = true
		}
		if text := v.serverText(name); text != "" {
			if out.Servers == nil {
				out.Servers = map[string]string{}
			}
			out.Servers[name] = text
		}
	}
	return out
}

// serverText is the instructions of the connected server name, cut at instructionsMax.
func (v view) serverText(name string) string {
	sv := v.live[name]
	if !sv.up() {
		return ""
	}
	return cutRunes(strings.TrimSpace(sv.client.Instructions()), instructionsMax)
}

func (v view) common() string {
	if list := v.skillsBlock(); list != "" {
		return resourcesLine + "\n" + list
	}
	return resourcesLine
}

// resources reports whether a connected server serves resources.
func (v view) resources() bool {
	return slices.ContainsFunc(v.names, func(n string) bool { return v.live[n].resources() })
}

const pluginsHeader = "[plugins: tools added after the tool list of this session was fixed — %s. They are deferred: to use one you MUST first read its definition with " +
	`mcp(action="select", tools=["tool_name"]), then run it with mcp(action="call", tool="tool_name", arguments={...}). Find one with mcp(action="search", query="...").]`

// addedPlugins renders the notice of the live plugin tools that the declaration lacks, one block for
// each plugin in name order, or "" when there is none.
func addedPlugins(late []Late) string {
	byPlugin := map[string][]remote{}
	for _, l := range late {
		byPlugin[l.Plugin] = append(byPlugin[l.Plugin], remote{spec: l.Tool.Spec()})
	}
	names := slices.Sorted(maps.Keys(byPlugin))
	if len(names) == 0 {
		return ""
	}
	blocks := make([]string, len(names))
	for i, n := range names {
		blocks[i] = fmt.Sprintf(`<plugin name="%s">%s`, attr(n), toolLines(byPlugin[n])) + "\n</plugin>"
	}
	return fmt.Sprintf(pluginsHeader, strings.Join(names, ", ")) + "\n" + strings.Join(blocks, "\n")
}

// loading renders the notice of the servers of the session that have not ended their first attempt, or "".
func (v view) loading() string {
	var names []string
	for _, name := range v.names {
		if v.src.session && v.src.loading(name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "[mcp: " + strings.Join(names, ", ") + " tools loading. Call a tool of such a server through the mcp tool once it has loaded.]"
}

// loadingTool is a tool of a server that has not ended its first attempt. A call waits for it for a short bound, runs
// the tool when the server has listed it, and otherwise says that the tools are still loading.
type loadingTool struct {
	r            toolResolver
	server, name string
}

func (t loadingTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: t.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (t loadingTool) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	owner := t.r.s.owner(t.server)
	if owner == nil {
		return protocol.ToolResult{Text: "no such tool available: " + t.name, IsError: true}, nil
	}
	owner.awaitAttempt(ctx, t.server)
	if live := t.r.live(t.name); live != nil {
		return live.Run(ctx, call)
	}
	if owner.loading(t.server) {
		return protocol.ToolResult{Text: fmt.Sprintf("mcp: %s tools are still loading; try again shortly", t.server), IsError: true}, nil
	}
	return protocol.ToolResult{Text: "no such tool available: " + t.name, IsError: true}, nil
}
