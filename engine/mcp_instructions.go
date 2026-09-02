// Connected-MCP-server instructions block. Unlike its sibling
// engine/mcp_status.go — which reports LIVE connection state and therefore
// rides the newest user message, changing turn to turn — this segment is
// STATIC context and belongs in the system prompt, where it is written to
// the prompt cache once and read back for the rest of the session.
//
// That split is the whole design. An MCP server's initialize instructions
// say how to use the server; they do not change while it is connected. Its
// liveness does change, and putting anything that changes into the system
// array invalidates the system and messages caches together (the tools
// cache survives), which on a long session re-processes the entire
// conversation at the cache-WRITE price. So: instructions here, liveness in
// mcpStatusSegment, and the two never trade places.
//
// Session.mcpInstructionsSegment memoizes the rendered block for exactly
// this reason — see its doc comment for what a late-connecting server costs.
package engine

import (
	"sort"
	"strings"
)

// mcpInstructionsReader is implemented by an MCPRegistry that can also
// report per-server initialize instructions — *MCPManager satisfies it via
// Instructions (see mcp.go). Narrow, for the same reason mcpStatusReader is
// narrow: MCPRegistry is a public contract that cmd/harness and server
// already build fakes against, and growing it would force those packages to
// add a method they have no use for. A registry that does not implement it
// is treated exactly like "no MCP configured": no block, every time.
type mcpInstructionsReader interface {
	Instructions() []MCPServerInstructions
}

// Sentinel tags for the instructions block. The shape follows the
// convention every major agent already renders for this — a named element
// per server — so a model that has seen one recognizes ours.
const (
	mcpInstructionsOpenTag  = "<mcp_instructions>"
	mcpInstructionsCloseTag = "</mcp_instructions>"
)

// renderMCPInstructions renders one system segment listing every connected
// server that supplied initialize instructions, sorted by name:
//
//	<mcp_instructions>
//	<server name="boxes-orchestration" tools="mcp__boxes-orchestration__spawn_box, ...">
//	Fleet orchestration over every box...
//	</server>
//	</mcp_instructions>
//
// Renders "" when there is nothing to say: reg is nil, reg does not
// implement mcpInstructionsReader, no server has connected yet, or no
// connected server set any instructions. An absent block costs nothing and
// keeps the system prefix byte-identical for the sessions that have no MCP
// servers at all.
//
// A server's text is UNTRUSTED input — it arrives from whatever process the
// server config points at — so it is neutralized before it reaches the
// block: a server cannot emit the block's own tags and cannot forge a
// sibling <server> element attributed to a name it does not own. This is
// the same defense renderTaskNotifications applies to a child's Result text
// (see neutralizeNotificationText), for the same reason.
func renderMCPInstructions(reg MCPRegistry) string {
	if reg == nil {
		return ""
	}
	reader, ok := reg.(mcpInstructionsReader)
	if !ok {
		return ""
	}
	entries := reader.Instructions()
	if len(entries) == 0 {
		return ""
	}
	// Sort here as well as in MCPManager.Instructions. This string is a
	// prompt-cache prefix, so its bytes must not depend on a caller's
	// ordering: a registry that ever returned map order would otherwise
	// hand a different system prompt to every session for the same set of
	// servers, and the defect would show up only as a bill.
	entries = append([]MCPServerInstructions(nil), entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	var b strings.Builder
	b.WriteString(mcpInstructionsOpenTag)
	for _, e := range entries {
		b.WriteString("\n<server name=\"")
		b.WriteString(neutralizeMCPAttr(e.Name))
		b.WriteString("\"")
		if len(e.Tools) > 0 {
			tools := append([]string(nil), e.Tools...)
			sort.Strings(tools)
			for i, t := range tools {
				tools[i] = neutralizeMCPAttr(t)
			}
			b.WriteString(" tools=\"")
			b.WriteString(strings.Join(tools, ", "))
			b.WriteString("\"")
		}
		b.WriteString(">\n")
		b.WriteString(neutralizeMCPInstructions(strings.TrimSpace(e.Text)))
		b.WriteString("\n</server>")
	}
	b.WriteString("\n")
	b.WriteString(mcpInstructionsCloseTag)
	return b.String()
}

// neutralizeMCPInstructions defangs the block's own markup in server-
// supplied BODY text, so only this renderer can emit the structure. A
// collision with legitimate prose is harmless and visible (an angle bracket
// becomes a parenthesis), never a silent drop — the same trade
// message.NeutralizeEngineContextSentinel already makes. Quotes are left
// alone here: prose legitimately contains them, and the body is not inside
// an attribute.
func neutralizeMCPInstructions(s string) string {
	r := strings.NewReplacer(
		mcpInstructionsOpenTag, "(mcp_instructions)",
		mcpInstructionsCloseTag, "(/mcp_instructions)",
		"<server", "(server",
		"</server>", "(/server)",
	)
	return r.Replace(s)
}

// neutralizeMCPAttr is neutralizeMCPInstructions for an ATTRIBUTE value
// (a server name, a tool name), where a double quote would end the
// attribute early and let a crafted name inject markup of its own.
func neutralizeMCPAttr(s string) string {
	return strings.ReplaceAll(neutralizeMCPInstructions(s), "\"", "'")
}

// mcpInstructionsSegment returns the session's frozen instructions block,
// rendering it on first call and caching the result — including the empty
// result, which is why mcpInstrLoaded exists rather than a "" check.
//
// Called from streamTurn AFTER the tool plan has run (see the numbered
// ordering note at the top of that function), so the first call already
// sees post-connect state: every server that came up in the one-time first
// batch contributes. What a freeze costs is a server whose FIRST attempt
// failed and whose background retry succeeds on a later turn — its tools
// become callable but its instructions never join this block for the rest
// of the session. That is the deliberate trade: a mid-session system-prompt
// rewrite costs the whole conversation's cached prefix on the turn it
// happens, every session it happens in, while the missing text costs one
// server's guidance in the rarer degraded-then-recovered case — and
// mcpStatusSegment still tells the model that server exists and is now
// healthy. Revisit only with a cache-cost measurement in hand.
func (s *Session) mcpInstructionsSegment() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mcpInstrLoaded {
		return s.mcpInstrSeg
	}
	s.mcpInstrSeg = renderMCPInstructions(s.cfg.MCP)
	s.mcpInstrLoaded = true
	return s.mcpInstrSeg
}
