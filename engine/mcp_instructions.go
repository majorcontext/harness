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
	"context"
	"encoding/json"

	"github.com/majorcontext/harness/provider"

	"github.com/majorcontext/harness/message"

	"sort"
	"strconv"
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

// mcpInstructionsPerServerCap bounds ONE server's instructions text, in
// runes. The text is UNTRUSTED -- it arrives from whatever process the
// server config points at -- and this block is a system-prompt segment,
// written to the prompt cache once and re-read for every turn of the
// session. An unbounded server therefore does not cost one large message;
// it inflates the cached prefix for the whole session, and far enough out
// it crowds the context window the conversation itself needs. Bounding it
// is the same trade taskNotificationResultCap makes for a child's Result,
// and the cut is MARKED (capRunes appends taskLogTruncationMarker) so a
// model reading truncated guidance can tell it is truncated.
//
// PER SERVER, not per block, because the two inputs have different trust:
// the text is remote and unbounded, while the SET of servers is operator
// configuration in this box's own harness.json. Capping the block would
// let one verbose server silently swallow a later server's guidance;
// capping each server bounds the total at cap x servers, which the
// operator already controls.
//
// 4000 matches taskNotificationResultCap. Real server instructions run a
// few hundred to a couple thousand runes (the boxes-orchestration and
// braintrust servers both sit well inside it), so this cuts a server that
// is misconfigured or hostile, not one that is merely thorough.
const mcpInstructionsPerServerCap = 4000

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
// mcpRegistryFromServers narrows a registry to the server set its caller's
// tool plan already read — including deferred servers, whose tools sit in
// the catalog rather than the request's tools array. The frozen segment then
// advertises neither more nor fewer servers than the plan saw, so a retry
// committing between two registry reads cannot desynchronize them.
func mcpRegistryFromServers(reg MCPRegistry, servers map[string]bool) MCPRegistry {
	if reg == nil {
		return nil
	}
	// An empty set means no server held tools on the plan's read: render
	// nothing, never a live unfiltered read — a retry committing between
	// the plan and this render must not leak a server the plan never saw.
	if len(servers) == 0 {
		return nil
	}
	return mcpToolsSnapshotRegistry{byServer: servers, inner: reg}
}

// mcpToolsSnapshotRegistry answers Instructions() filtered to the servers
// present in the shared tool snapshot; everything else delegates to the
// owning manager.
type mcpToolsSnapshotRegistry struct {
	byServer map[string]bool
	inner    MCPRegistry
}

func (r mcpToolsSnapshotRegistry) Instructions() []MCPServerInstructions {
	reader, ok := r.inner.(mcpInstructionsReader)
	if !ok {
		return nil
	}
	entries := reader.Instructions()
	out := make([]MCPServerInstructions, 0, len(entries))
	for _, e := range entries {
		if !r.byServer[e.Name] {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (r mcpToolsSnapshotRegistry) Tools(ctx context.Context) []provider.ToolDef {
	return r.inner.Tools(ctx)
}

func (r mcpToolsSnapshotRegistry) CallTool(ctx context.Context, name string, args json.RawMessage) (message.Parts, bool, error) {
	return r.inner.CallTool(ctx, name, args)
}

func (r mcpToolsSnapshotRegistry) CallServerTool(ctx context.Context, server, name string, args json.RawMessage) (message.Parts, bool, error) {
	return r.inner.CallServerTool(ctx, server, name, args)
}

// mcpResourcesInstructionLine tells the model the native resource tools
// exist, appended when a connected server advertised the resources
// capability.
const mcpResourcesInstructionLine = "This session can also list and read MCP resources with the " +
	mcpListResourcesToolName + " and " + mcpReadResourceToolName + " tools."

func renderMCPInstructions(reg MCPRegistry, hasResources bool) string {
	// hasResources must decide even when reg is nil: a resources-only
	// server has no tool defs, so it never lands in the plan's server set.
	var entries []MCPServerInstructions
	if reg != nil {
		if reader, ok := reg.(mcpInstructionsReader); ok {
			entries = reader.Instructions()
		}
	}
	// Drop an entry with nothing to say BEFORE anything is written: an
	// unfiltered empty entry would otherwise render an empty <server>
	// element at full prefix cost. Trimming once here also settles the
	// display form for the write loop below.
	kept := make([]MCPServerInstructions, 0, len(entries))
	for _, e := range entries {
		text := strings.TrimSpace(e.Text)
		if text == "" {
			continue
		}
		text, _ = capRunes(text, mcpInstructionsPerServerCap)
		e.Text = text
		kept = append(kept, e)
	}
	if len(kept) == 0 && !hasResources {
		return ""
	}
	// Sort here as well as in MCPManager.Instructions. This string is a
	// prompt-cache prefix, so its bytes must not depend on a caller's
	// ordering: a registry that ever returned map order would otherwise
	// hand a different system prompt to every session for the same set of
	// servers, and the defect would show up only as a bill.
	entries = kept
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	var b strings.Builder
	b.WriteString(mcpInstructionsOpenTag)
	if hasResources {
		b.WriteString("\n")
		b.WriteString(mcpResourcesInstructionLine)
	}
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
			// The catalog already bounds a deferred listing at 200 tools;
			// this cached system segment must not exceed it, so an oversized
			// server degrades to one attribute carrying the count and the
			// first 200 names, never two tools attributes.
			const maxTools = 200
			const maxToolsBytes = 2048
			truncated := ""
			if len(tools) > maxTools {
				truncated = " " + strconv.Itoa(len(tools)) + " tools: first " + strconv.Itoa(maxTools) + " listed"
				tools = tools[:maxTools]
			}
			var sb strings.Builder
			for i, t := range tools {
				need := len(t)
				if i > 0 {
					need += 2
				}
				if sb.Len()+need > maxToolsBytes {
					truncated = " names truncated at byte budget"
					break
				}
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(t)
			}
			joined := sb.String() + truncated
			b.WriteString(" tools=\"")
			b.WriteString(joined)
			b.WriteString("\"")
		}
		b.WriteString(">\n")
		b.WriteString(neutralizeMCPInstructions(e.Text))
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
	return s.mcpInstructionsSegmentFrom(s.liveMCPToolServers(), len(mcpResourceCapableServers(context.Background(), s.cfg.MCP)) > 0)
}

// mcpInstructionsSegmentFrom renders the frozen segment from the SAME tool
// snapshot and resources-capability gate the request's plan already read,
// so a retry committing between two registry reads cannot desynchronize
// them.
func (s *Session) mcpInstructionsSegmentFrom(servers map[string]bool, hasResources bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mcpInstrLoaded {
		return s.mcpInstrSeg
	}
	s.mcpInstrSeg = renderMCPInstructions(mcpRegistryFromServers(s.cfg.MCP, servers), hasResources)
	s.mcpInstrLoaded = true
	return s.mcpInstrSeg
}
