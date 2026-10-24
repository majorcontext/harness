package mcpsrc

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// The kinds of the notices of a Source. recovered is what the conversation says when the last server that
// was down connects.
const (
	noticeKind = "mcp"
	addedKind  = "mcp-set"
	recovered  = "[mcp: every configured server is connected again.]"
)

const addedHeader = "[mcp: tools added after the first connect — %s. They are deferred: to use one you MUST first read its definition with " +
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

// added renders the notice of the tools that arrived after the first connect, with the instructions of a
// server that the first connect did not reach, and the notice of the servers that went. It is "" when there
// is neither. The text follows the live state, so a later model call renders it again.
func (v view) added(ok func(string) bool, late []Late) string {
	var servers, blocks []string
	for _, name := range v.names {
		m := v.mem[name]
		var late []remote
		for _, r := range v.remotes(name, ok) {
			if r.listed && !m.base[r.name] {
				late = append(late, r)
			}
		}
		if len(late) == 0 {
			continue
		}
		servers = append(servers, name)
		var b strings.Builder
		fmt.Fprintf(&b, `<server name="%s">%s`, attr(name), toolLines(late))
		if text := cutRunes(strings.TrimSpace(v.live[name].client.Instructions()), instructionsMax); text != "" && !slices.Contains(v.reached, name) {
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
	var removed []string
	for _, name := range slices.Sorted(maps.Keys(v.mem)) {
		if _, ok := v.health[name]; !ok && v.mem[name].up {
			removed = append(removed, name)
		}
	}
	if len(removed) > 0 {
		out = append(out, "[mcp: removed — "+strings.Join(removed, ", ")+". The servers and their tools are gone, and a call to one of those tools fails.]")
	}
	return strings.Join(out, "\n")
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

// instructions is the segment of the servers that the first connect reached: each one's own
// instructions with its allowed tool names, and a line about the resource tools. It reads what the first
// connect froze, so it never changes.
func (v view) instructions(ok func(string) bool) string {
	var b strings.Builder
	for _, name := range v.reached {
		m := v.mem[name]
		text := v.serverText(name)
		if text == "" {
			continue
		}
		var tools []string
		for _, t := range slices.Sorted(maps.Keys(m.base)) {
			if n := prefix + name + "__" + t; ok(n) {
				tools = append(tools, attr(n))
			}
		}
		fmt.Fprintf(&b, "\n<server name=\"%s\" tools=\"%s\">\n%s\n</server>", attr(name), listNames(tools), tag.Replace(text))
	}
	if v.resources() {
		return "<mcp_instructions>\n" + resourcesLine + b.String() + "\n</mcp_instructions>"
	}
	if b.Len() == 0 {
		return ""
	}
	return "<mcp_instructions>" + b.String() + "\n</mcp_instructions>"
}

// perServer is the guidance of the segment for an external harness, which shows the instructions of each
// server under the name of that server and cuts each one by itself: the line about the resource tools, and
// the instructions of each server that the first connect reached, cut as in the segment.
func (v view) perServer() turn.MCPInstructions {
	var out turn.MCPInstructions
	if v.resources() {
		out.Common = resourcesLine
	}
	for _, name := range v.reached {
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

// serverText is the instructions of the server name that the first connect froze, cut at instructionsMax.
func (v view) serverText(name string) string {
	return cutRunes(strings.TrimSpace(v.mem[name].inst), instructionsMax)
}

// resources reports whether a server that the first connect reached serves resources.
func (v view) resources() bool {
	return slices.ContainsFunc(v.reached, func(n string) bool { return v.mem[n].resources })
}

const pluginsHeader = "[plugins: tools added after the first tool read — %s. They are deferred: to use one you MUST first read its definition with " +
	`mcp(action="select", tools=["tool_name"]), then run it with mcp(action="call", tool="tool_name", arguments={...}). Find one with mcp(action="search", query="...").]`

// addedPlugins renders the notice of the live plugin tools that the first tool read did not reach, one block for
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
