package mcpsrc

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/majorcontext/harness/protocol"
)

// The kinds of the notices of a Source. recovered is what the conversation says when the last server that
// was down connects.
const (
	noticeKind = "mcp"
	addedKind  = "mcp-set"
	recovered  = "[mcp: every configured server is connected again.]"
)

const addedHeader = "[mcp: tools added after the first connect — %s. They are deferred: to use one you MUST first load it with " +
	`mcp(action="select", tools=["mcp__server__tool"]), or find one with mcp(action="search", query="...").]`

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
func (v view) added(ok func(string) bool) string {
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
		text := cutRunes(strings.TrimSpace(m.inst), instructionsMax)
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
	if slices.ContainsFunc(v.reached, func(n string) bool { return v.mem[n].resources }) {
		return "<mcp_instructions>\n" + resourcesLine + b.String() + "\n</mcp_instructions>"
	}
	if b.Len() == 0 {
		return ""
	}
	return "<mcp_instructions>" + b.String() + "\n</mcp_instructions>"
}
