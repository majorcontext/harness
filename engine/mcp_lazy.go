// Lazy MCP tool loading: a deferred server's tools reach the model as a
// NAME-ONLY catalog in the system prompt instead of as full JSON Schemas in
// the tools array. See docs/design/mcp-lazy-tools.md for the whole design;
// this file holds the mechanism.
//
// Why defer at all: every connected MCP server contributes its whole
// catalog to every request (Session.toolDefs -> MCPRegistry.Tools), tool
// defs sit at the FRONT of the cached prefix on every provider, and a box
// that wires several large servers therefore pays for hundreds of schemas
// on every turn before the model reads one word of the request. The MCP
// CONNECTION was already lazy (see mcp.go's package doc); the schema cost
// was not.
//
// The shape follows the Agent Skills progressive-disclosure model already
// in skills.go: stage 1 is one line per tool (name plus a one-line
// description) plus a header naming the contract, and stage 2 -- the
// schema -- is an explicit fetch. Here the fetch is the mcp session tool's
// select action, and the fetched schema lands in the TOOLS ARRAY of the
// next request, so a selected tool is called exactly like a statically
// registered one. runAgenticLoop rebuilds the request on every tool round,
// so a select made mid-turn takes effect on the very next round of that
// same turn.
//
// Everything here is opt-in. With no configuration (MCPToolLoading unset,
// i.e. eager) resolveMCPLoading reports eager for every server, the plan
// returns exactly the slice MCPRegistry.Tools returned, and the catalog
// segment is empty -- byte for byte the behaviour that predates this file.
//
// Two properties this file must not break:
//
//   - The tools array is byte-stable across requests (see AGENTS.md and
//     Session.toolDefs). The partition preserves the registry's order, and
//     the catalog listing is sorted by full tool name -- by THIS file, not
//     inherited from the registry -- so identical state always renders
//     identical bytes.
//   - No method joins the MCPRegistry interface. Everything needed is
//     derived from the []provider.ToolDef slice Tools already returns (the
//     name carries the server, since config.validateMCPServers bans "__"
//     inside a server name) plus the narrow, optional mcpStatusReader and
//     mcpConfigReader interfaces mcp_status.go and mcp_tool.go already
//     define. Growing MCPRegistry would force every out-of-package fake to
//     grow with it.
package engine

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/majorcontext/harness/provider"
)

// MCPToolLoading selects when a session defers MCP tool schemas. The zero
// value (MCPToolLoadingUnset) means eager: unconfigured sessions keep the
// pre-deferral behaviour exactly.
type MCPToolLoading string

const (
	// MCPToolLoadingUnset is the zero value and reads as eager everywhere.
	MCPToolLoadingUnset MCPToolLoading = ""
	// MCPToolLoadingEager registers every tool with its schema.
	MCPToolLoadingEager MCPToolLoading = "eager"
	// MCPToolLoadingAuto defers once the live catalog holds more tools than
	// the threshold. Valid as a GLOBAL mode only: the threshold measures
	// whole-catalog pressure, which is not a property of one server (see
	// config.validateMCPServers, which rejects it per server).
	MCPToolLoadingAuto MCPToolLoading = "auto"
	// MCPToolLoadingLazy always defers.
	MCPToolLoadingLazy MCPToolLoading = "lazy"
)

// defaultMCPDeferThreshold is the tool COUNT MCPToolLoadingAuto compares the
// live catalog against when Config.MCPToolLoadingThreshold is not positive.
// A count, not a token estimate: the engine has no tokenizer on the request
// path, and a count is deterministic and testable.
//
// Any non-positive value resolves to THIS default, never to a floor of 1.
// A floor of 1 would be the always-defer bug config.validateMCPToolLoading
// exists to reject: len(catalog) > 1 holds for all but the emptiest
// catalog.
const defaultMCPDeferThreshold = 20

// mcpCatalogDescriptionMax bounds one catalog line's description text, in
// bytes, cut on a UTF-8 boundary by truncateUTF8 (toolresult.go). The line
// exists to let the model recognize a tool, not to reproduce its docs.
const mcpCatalogDescriptionMax = 160

// mcpCatalogListingMax bounds how many tools the catalog segment names
// before it stops and points at search instead. An unbounded listing over a
// pathological catalog would re-create the very cost this file removes.
const mcpCatalogListingMax = 200

// mcpCatalogHeader introduces the deferred catalog and states the contract:
// a deferred tool is not callable until the model loads it. It names the
// exact call shape because that is the only in-band documentation the model
// gets at the moment it needs it.
const mcpCatalogHeader = "Deferred MCP tools. These tools exist but their input schemas are not loaded. " +
	"To use one you MUST first load it with the mcp tool: " +
	`mcp(action="select", tools=["mcp__server__tool"]). ` +
	"A selected tool appears in your tool list on the next request and is then called directly. " +
	`Select every tool you need in ONE call. Use mcp(action="search", query="...") to find a tool by keyword.`

// splitMCPToolName splits a namespaced tool name into its server and remote
// halves. It reports false for any name that is not
// mcp__<server>__<tool> shaped with both halves non-empty.
//
// The split is unambiguous because config.validateMCPServers rejects a
// server name containing "__" (and one starting with "mcp__"), so the FIRST
// "__" after the prefix always ends the server name.
func splitMCPToolName(name string) (server, remote string, ok bool) {
	if !strings.HasPrefix(name, mcpToolPrefix) {
		return "", "", false
	}
	rest := name[len(mcpToolPrefix):]
	i := strings.Index(rest, "__")
	if i <= 0 {
		return "", "", false
	}
	server, remote = rest[:i], rest[i+2:]
	if remote == "" {
		return "", "", false
	}
	return server, remote, true
}

// mcpPolicyMode reports one server's POLICY mode: its per-server override
// when set, the global mode otherwise, with the unset zero value normalized
// to eager. It is deliberately catalog-independent -- auto is returned as
// auto, not resolved against the threshold -- so callers that must be
// stable for a session's whole life (sessionCanDefer, and through it the
// mcp tool's action schema) can use it.
func (s *Session) mcpPolicyMode(server string) MCPToolLoading {
	if m, ok := s.cfg.MCPToolLoadingByServer[server]; ok && m != MCPToolLoadingUnset {
		return m
	}
	if s.cfg.MCPToolLoading == MCPToolLoadingUnset {
		return MCPToolLoadingEager
	}
	return s.cfg.MCPToolLoading
}

// mcpDeferThreshold resolves the effective auto threshold.
func (s *Session) mcpDeferThreshold() int {
	if s.cfg.MCPToolLoadingThreshold > 0 {
		return s.cfg.MCPToolLoadingThreshold
	}
	return defaultMCPDeferThreshold
}

// sessionCanDefer reports whether ANY configured server could ever defer in
// this session. It is the gate for the mcp tool's search/select actions
// (mcp_tool.go) and for use-implies-selection recording.
//
// Two halves, and both matter in both directions:
//
//   - The session must hold the `mcp` tool. Never defer what the session
//     cannot select. A subagent restricted by an agent definition that
//     omits "mcp" (restrictTools, session_manager.go) would otherwise lose
//     every MCP schema AND the only path to load one back -- a lockout
//     that turns a harmless restriction into a total loss of MCP.
//   - Some CONFIGURED server's policy mode must not be eager. Reading the
//     global mode alone gets this wrong twice over: a global eager with one
//     per-server lazy DOES defer (under-advertising), and a global lazy
//     whose every server is pinned eager defers nothing (over-advertising).
//
// auto counts as "not eager" here, because a catalog can cross the
// threshold at any moment. Every input is session config, fixed for the
// session's life, so a def gated on this stays byte-stable across requests.
func (s *Session) sessionCanDefer() bool {
	if _, ok := s.tools[mcpSessionToolName]; !ok {
		return false
	}
	cr, ok := s.cfg.MCP.(mcpConfigReader)
	if !ok {
		return false
	}
	for _, name := range cr.ConfiguredNames() {
		if s.mcpPolicyMode(name) != MCPToolLoadingEager {
			return true
		}
	}
	return false
}

// mcpToolPlan is one request's decision about MCP tools: which defs enter
// the tools array, and what the stage-1 catalog segment says. streamTurn
// computes it ONCE per request and uses both halves, so MCPRegistry.Tools
// -- the call that triggers a server's first connect attempt -- still
// happens exactly once per request.
type mcpToolPlan struct {
	// defs are the MCP tool defs that belong in this request's tools array,
	// in the registry's own (server, then tool) order.
	defs []provider.ToolDef
	// catalog is the stage-1 system segment, or "" when nothing is
	// deferred.
	catalog string
}

// planMCPTools builds this request's plan. It calls MCPRegistry.Tools once,
// reaps stale selections, partitions the catalog, and renders the segment.
//
// A nil registry yields the zero plan, so a session with no MCP configured
// pays nothing.
func (s *Session) planMCPTools(ctx context.Context) mcpToolPlan {
	if s.cfg.MCP == nil {
		return mcpToolPlan{}
	}
	all := s.cfg.MCP.Tools(ctx)
	if len(all) == 0 {
		return mcpToolPlan{}
	}

	deferring := s.sessionCanDefer()
	if !deferring {
		return mcpToolPlan{defs: all}
	}

	selected := s.reapMCPSelections(all)
	overThreshold := len(all) > s.mcpDeferThreshold()

	defs := make([]provider.ToolDef, 0, len(all))
	var deferred []provider.ToolDef
	for _, d := range all {
		server, _, ok := splitMCPToolName(d.Name)
		if !ok {
			// A name this session cannot attribute to a server cannot be
			// selected either (select rejects the same shape), so it stays
			// eagerly registered rather than becoming unreachable.
			defs = append(defs, d)
			continue
		}
		if !mcpServerDefers(s.mcpPolicyMode(server), overThreshold) {
			defs = append(defs, d)
			continue
		}
		if selected[d.Name] {
			defs = append(defs, d)
			continue
		}
		deferred = append(deferred, d)
	}
	return mcpToolPlan{defs: defs, catalog: mcpCatalogSegment(deferred)}
}

// mcpServerDefers turns one server's policy mode plus the live
// over-threshold answer into the request's actual decision.
//
// The over-threshold answer counts the WHOLE catalog, including the tools
// of a server pinned eager (see planMCPTools). A pinned server's schemas
// fill the prompt like any other, so they are part of the pressure the
// threshold measures: a pin says "always keep these loaded", never "ignore
// their cost".
func mcpServerDefers(mode MCPToolLoading, overThreshold bool) bool {
	switch mode {
	case MCPToolLoadingLazy:
		return true
	case MCPToolLoadingAuto:
		return overThreshold
	default:
		return false
	}
}

// reapMCPSelections returns a snapshot of the selected set with every stale
// name dropped, and drops those names from the session's own set too.
//
// A selection is stale when its server is CONNECTED and the live catalog
// does not hold the name. That is the rule that keeps an invented name out
// of durable state: select cannot tell an invented name from a real one
// while a server is unconnected -- the catalog is unknown -- so it accepts
// the name as pending, and this reap removes it the moment the server
// connects without it. A selection whose server is still unconnected is
// KEPT, so it arms itself when that server does connect.
//
// Memory-only, by design. Replay unions the log again on the next reload,
// and this same rule prunes the same name again on that session's first
// plan, so no removal record is needed.
func (s *Session) reapMCPSelections(catalog []provider.ToolDef) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.mcpSelected) == 0 {
		return nil
	}
	live := make(map[string]bool, len(catalog))
	for _, d := range catalog {
		live[d.Name] = true
	}
	connected := mcpConnectedServers(s.cfg.MCP)
	out := make(map[string]bool, len(s.mcpSelected))
	for name := range s.mcpSelected {
		if !live[name] {
			server, _, ok := splitMCPToolName(name)
			if ok && connected[server] {
				delete(s.mcpSelected, name)
				continue
			}
		}
		out[name] = true
	}
	return out
}

// mcpConnectedServers reports which configured servers currently have a
// live connection, read through the narrow mcpStatusReader interface. A
// registry with no status surface reports none, which makes the reap a
// no-op: without connection state there is no evidence a name is stale.
func mcpConnectedServers(reg MCPRegistry) map[string]bool {
	sr, ok := reg.(mcpStatusReader)
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, st := range sr.Status() {
		if st.Connected {
			out[st.Name] = true
		}
	}
	return out
}

// markMCPToolsSelected adds well-formed namespaced names to the session's
// selected set and reports the names that actually ENTERED it. A malformed
// name is ignored, matching select's own malformed-name rule and the replay
// guard: a name no server can own must never enter durable state.
//
// This is the only writer in this slice. It does not journal: the durable
// record and its two callers (the mcp tool's select action, and
// use-implies-selection on a routed call) land with the surface that needs
// them.
func (s *Session) markMCPToolsSelected(names ...string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var added []string
	for _, name := range names {
		if _, _, ok := splitMCPToolName(name); !ok {
			continue
		}
		if s.mcpSelected[name] {
			continue
		}
		if s.mcpSelected == nil {
			s.mcpSelected = map[string]bool{}
		}
		s.mcpSelected[name] = true
		added = append(added, name)
	}
	return added
}

// mcpToolSelected reports whether name is in the selected set.
func (s *Session) mcpToolSelected(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mcpSelected[name]
}

// mcpCatalogSegment renders the stage-1 listing for the deferred, unselected
// tools of one request, or "" when there are none.
//
// Lines are sorted by full tool name, ascending, by THIS function. The
// registry's own order is server-then-tool, which usually agrees but can
// differ (for servers "a" and "a0", mcp__a0__b sorts before mcp__a__z by
// name and after it by server). One stated order keeps the segment
// byte-stable whatever the registry does.
func mcpCatalogSegment(deferred []provider.ToolDef) string {
	if len(deferred) == 0 {
		return ""
	}
	names := make([]string, 0, len(deferred))
	desc := make(map[string]string, len(deferred))
	for _, d := range deferred {
		names = append(names, d.Name)
		desc[d.Name] = d.Description
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString(mcpCatalogHeader)
	b.WriteString("\n\n")
	shown := names
	if len(shown) > mcpCatalogListingMax {
		shown = shown[:mcpCatalogListingMax]
	}
	for _, name := range shown {
		b.WriteString(name)
		if line := mcpCatalogDescription(desc[name]); line != "" {
			b.WriteString(" — ")
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	if rest := len(names) - len(shown); rest > 0 {
		b.WriteString("... and ")
		b.WriteString(strconv.Itoa(rest))
		b.WriteString(` more tools; use mcp(action="search", query="...") to find them`)
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// mcpCatalogDescription reduces one tool's description to a single catalog
// line: its first line, whitespace-trimmed, truncated on a UTF-8 boundary.
func mcpCatalogDescription(d string) string {
	if i := strings.IndexAny(d, "\r\n"); i >= 0 {
		d = d[:i]
	}
	d = strings.TrimSpace(d)
	return truncateUTF8(d, mcpCatalogDescriptionMax)
}
