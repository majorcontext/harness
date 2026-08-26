// The mcp session tool's search and select actions: the surface that finds
// a deferred MCP tool by keyword and loads its schema into the tools array.
// See docs/design/mcp-lazy-tools.md §4, and mcp_lazy.go for the deferral
// mechanism these two actions drive.
//
// They are verbs on the EXISTING mcp tool rather than a tool of their own.
// Its registration gate -- at least one configured server -- is already the
// condition under which a deferred catalog can exist; every comparable
// engine surface (goal, model, process, and mcp itself) is one tool with an
// action enum; and a new tool def would cost a permanent slot in the tools
// array of every session, in a feature whose whole point is to spend fewer
// bytes there.
//
// The argument shape follows the engine's own convention rather than Claude
// Code's "select:<name>" query-string form: an action plus structured
// arguments the provider validates against the schema. A single overloaded
// query string would move parsing, and a whole class of malformed-input
// errors, into the tool body for no gain.
//
// Neither action echoes a schema. select reports names only, because the
// tools array is the one authoritative copy: echoing schemas would write
// every one of them a second time into DURABLE history, where every later
// turn of the session re-sends it -- the opposite of what deferral is for.
package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// Search scoring. Each row scores at most ONCE per distinct query token per
// field, whatever the number of occurrences, and matching is plain
// substring containment over lowercased text -- never whole-word, never
// stemmed, so a token must match inside "create_issue" and "createIssue"
// alike. The exact-name bonus fires once for the whole query.
const (
	mcpSearchScoreExactName   = 100
	mcpSearchScoreRemoteName  = 50
	mcpSearchScoreDescription = 10
	mcpSearchScoreServerName  = 5
)

// mcpSearchDefaultLimit and mcpSearchMaxLimit bound one search result set.
// A limit below 1 falls back to the default rather than erroring: the model
// asked for results, not for a lecture about pagination.
const (
	mcpSearchDefaultLimit = 20
	mcpSearchMaxLimit     = 50
)

// mcpSearchMatch is one ranked tool in the search result.
type mcpSearchMatch struct {
	Name        string `json:"name"`
	Server      string `json:"server"`
	Description string `json:"description,omitempty"`
	// Loaded reports whether this tool's schema is in the tools array RIGHT
	// NOW -- true for a tool of an eager server and for a selected tool of a
	// deferred one. It deliberately does NOT report set membership: the
	// question the model must answer before calling a tool is "must I select
	// this first", and under auto below the threshold, where nothing defers,
	// the honest answer for every tool is yes-loaded. A membership flag would
	// answer false there and send the model into a select call it does not
	// need.
	Loaded bool `json:"loaded"`
}

// mcpSearchResult is the search action's payload. Total counts every tool
// that scored above zero, BEFORE limit cuts the list, and Truncated is
// exactly Total > len(Matches), so the two can never disagree.
type mcpSearchResult struct {
	Matches   []mcpSearchMatch `json:"matches"`
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated"`
}

// mcpSelectResult is the select action's payload. Every requested name
// lands in exactly one bucket; see mcpSelectBucket for the order.
type mcpSelectResult struct {
	// Selected are names that entered the selected set and whose tools the
	// live catalog holds.
	Selected []string `json:"selected"`
	// Already are names that were in the selected set before this call.
	Already []string `json:"already"`
	// Pending are names whose server is configured but not connected. They
	// entered the set and arm themselves when that server connects.
	Pending []string `json:"pending"`
	// Missing are names no configured, connected server holds, plus any
	// malformed name. They do not enter the set.
	Missing []string `json:"missing"`
	Note    string   `json:"note"`
}

// The select note is CONDITIONAL on what the batch produced, because the
// model acts on it. runAgenticLoop rebuilds the request on every tool
// round, so a selected tool is live on the very next round of that same
// turn -- but a PENDING name is not: its server is down, so its schema
// enters no tools array until that server reconnects. An unconditional
// "callable next request" would send the model straight into calling a
// tool of a degraded server.
const (
	mcpSelectNoteCallable = "selected tools are callable from the next request in this turn"
	mcpSelectNoteMixed    = "the loaded tools are callable from the next request in this turn; the pending ones load once their server reconnects"
	mcpSelectNotePending  = "no tool was loaded: every name you selected belongs to a server that is not connected. They load once that server reconnects; see the mcp tool's status and connect actions"
	mcpSelectNoteNone     = "no tool was loaded"
)

// mcpSelectNoteFor picks the note for one batch's outcome. Four cases, and
// the split is by CALLABILITY rather than by bucket name:
//
//   - already counts as loaded. Those tools were selected on an earlier
//     call and are in the tools array right now, so a batch of nothing but
//     already-selected names must not report "no tool was loaded" -- that
//     reads as "nothing is available" about tools the model can call.
//   - pending counts as not loaded, whatever else the batch contains, so a
//     batch holding both says both things rather than only the happy half.
func mcpSelectNoteFor(res mcpSelectResult) string {
	loaded := len(res.Selected) > 0 || len(res.Already) > 0
	switch {
	case loaded && len(res.Pending) > 0:
		return mcpSelectNoteMixed
	case loaded:
		return mcpSelectNoteCallable
	case len(res.Pending) > 0:
		return mcpSelectNotePending
	default:
		return mcpSelectNoteNone
	}
}

// runMCPSearch implements the search action: rank the live catalog by
// keyword and report what is loaded.
//
// It changes no SELECTION of its own -- no name enters the set here -- but
// it is not side-effect free: computing the loaded set runs the same plan a
// request builds, and that plan reaps a stale selection (see
// reapMCPSelections). A search made after a server reconnects without a
// previously selected tool therefore drops that name, exactly as the next
// request would have.
func runMCPSearch(ctx context.Context, s *Session, query string, limit int) (message.Parts, error) {
	tokens := mcpSearchTokens(query)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("mcp: search requires a non-empty %q argument", "query")
	}
	if limit < 1 {
		limit = mcpSearchDefaultLimit
	}
	if limit > mcpSearchMaxLimit {
		limit = mcpSearchMaxLimit
	}

	catalog := s.mcpCatalog(ctx)
	loaded := s.mcpLoadedNames(catalog)

	type scored struct {
		def   provider.ToolDef
		score int
	}
	// Normalized once, not once per catalog tool: identical for every call
	// in this search.
	whole := strings.ToLower(strings.TrimSpace(query))
	var hits []scored
	for _, d := range catalog {
		if score := mcpSearchScore(d, whole, tokens); score > 0 {
			hits = append(hits, scored{def: d, score: score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].def.Name < hits[j].def.Name
	})

	total := len(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	matches := make([]mcpSearchMatch, 0, len(hits))
	for _, h := range hits {
		server, _, _ := splitMCPToolName(h.def.Name)
		matches = append(matches, mcpSearchMatch{
			Name:        h.def.Name,
			Server:      server,
			Description: mcpCatalogDescription(h.def.Description),
			Loaded:      loaded[h.def.Name],
		})
	}
	return jsonResult(mcpSearchResult{Matches: matches, Total: total, Truncated: total > len(matches)})
}

// mcpSearchTokens lowercases query and splits it on every run of characters
// that are neither letters nor digits, then deduplicates: a token repeated
// in the query scores once, not twice. It returns nil for a blank query,
// which is what makes a whole-catalog dump impossible.
//
// The split is over UNICODE letter/digit classes, not the ASCII ranges. An
// ASCII-only split silently truncated an accented word ("café" searched for
// "caf") and reduced a CJK query to NO tokens at all, which surfaced as the
// blank-query error even though the model had supplied a real query. Tool
// names and descriptions are not guaranteed to be English.
func mcpSearchTokens(query string) []string {
	lower := strings.ToLower(query)
	fields := strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := make(map[string]bool, len(fields))
	var out []string
	for _, f := range fields {
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// mcpSearchScore scores one tool against the deduplicated token set. See
// the score constants for the rules; the arithmetic is deliberately simple
// and total, so a golden ranking test has exactly one right answer.
//
// whole is the already-lowercased, already-trimmed WHOLE query (see
// runMCPSearch), passed in rather than recomputed per tool.
func mcpSearchScore(d provider.ToolDef, whole string, tokens []string) int {
	server, remote, ok := splitMCPToolName(d.Name)
	if !ok {
		return 0
	}
	name := strings.ToLower(d.Name)
	remote = strings.ToLower(remote)
	server = strings.ToLower(server)
	desc := strings.ToLower(d.Description)

	score := 0
	if whole == name || whole == remote {
		score += mcpSearchScoreExactName
	}
	for _, tok := range tokens {
		if strings.Contains(remote, tok) {
			score += mcpSearchScoreRemoteName
		}
		if strings.Contains(desc, tok) {
			score += mcpSearchScoreDescription
		}
		if strings.Contains(server, tok) {
			score += mcpSearchScoreServerName
		}
	}
	return score
}

// runMCPSelect implements the select action: load the named tools'
// schemas. Every input shape but an empty tools array yields a result
// rather than an error, so one bad name never voids a batch of good ones.
func runMCPSelect(ctx context.Context, s *Session, names []string) (message.Parts, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("mcp: select requires a non-empty %q array", "tools")
	}

	catalog := s.mcpCatalog(ctx)
	live := make(map[string]bool, len(catalog))
	for _, d := range catalog {
		live[d.Name] = true
	}
	connected, haveStatus := mcpConnectedServersKnown(s.cfg.MCP)
	configured := map[string]bool{}
	if cr, ok := s.cfg.MCP.(mcpConfigReader); ok {
		for _, n := range cr.ConfiguredNames() {
			configured[n] = true
		}
	}

	out := mcpSelectResult{
		Selected: []string{},
		Already:  []string{},
		Pending:  []string{},
		Missing:  []string{},
	}
	var toAdd []string
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		switch s.mcpSelectBucket(name, live, connected, configured, haveStatus) {
		case mcpBucketAlready:
			out.Already = append(out.Already, name)
		case mcpBucketSelected:
			out.Selected = append(out.Selected, name)
			// Both writers of the record apply the SAME per-server gate
			// (see mcpToolUseImpliesSelection): a record exists only to
			// survive a flip to deferred, so "can this server ever flip"
			// must have one answer whichever writer asks. A server pinned
			// eager can never flip, so its tool is reported selected -- it
			// is loaded and callable, which is what the model asked for --
			// and nothing is recorded, because the record could never pay
			// for itself.
			if s.mcpToolUseImpliesSelection(name) {
				toAdd = append(toAdd, name)
			}
		case mcpBucketPending:
			out.Pending = append(out.Pending, name)
			if s.mcpToolUseImpliesSelection(name) {
				toAdd = append(toAdd, name)
			}
		default:
			out.Missing = append(out.Missing, name)
		}
	}
	s.markMCPToolsSelected(toAdd...)
	out.Note = mcpSelectNoteFor(out)
	return jsonResult(out)
}

// mcpSelectBucket names the four outcomes of one requested name.
type mcpSelectBucket int

const (
	// mcpBucketMissing: no configured, connected server holds the name, or
	// the name is malformed. It never enters the set.
	mcpBucketMissing mcpSelectBucket = iota
	// mcpBucketAlready: the name is in the selected set already, whatever
	// its server's state.
	mcpBucketAlready
	// mcpBucketSelected: the live catalog holds the name.
	mcpBucketSelected
	// mcpBucketPending: the name's server is configured but not connected.
	mcpBucketPending
)

// mcpSelectBucket decides one name's bucket. The order below is the
// contract, not an implementation detail: ALREADY is tested first, so a
// restored selection whose server has since parked reports already and
// records nothing, rather than producing a second, duplicate pending
// record.
//
// A malformed name -- one that is not mcp__<server>__<tool> shaped -- is
// missing. It carries no server, so no other bucket could hold it, and it
// must never be recorded: that is the same shape the replay guard skips, so
// one rule holds at both ends of the record's life.
//
// haveStatus is that fourth answer for "cannot tell". A registry that
// implements mcpConfigReader but NOT mcpStatusReader (only a test fake can
// be shaped this way -- *MCPManager implements both) reports no connected
// server at all, so without this flag every name for a configured server
// would look pending, an invented one would be recorded, and the reap --
// which needs the same missing evidence -- could never remove it. With it,
// a status-less registry simply never produces a pending name.
func (s *Session) mcpSelectBucket(name string, live, connected, configured map[string]bool, haveStatus bool) mcpSelectBucket {
	if s.mcpToolSelected(name) {
		return mcpBucketAlready
	}
	server, _, ok := splitMCPToolName(name)
	if !ok {
		return mcpBucketMissing
	}
	if live[name] {
		return mcpBucketSelected
	}
	// pending needs POSITIVE evidence that the server is not connected, and
	// that evidence is the status surface. Without one (a registry with no
	// Status method -- see mcpConnectedServersKnown) every configured
	// server would look unconnected, so an invented name would be accepted
	// as pending and, because the reap needs the same absent evidence,
	// could never be removed again. No evidence therefore means missing:
	// nothing is recorded that nothing can later verify.
	if haveStatus && configured[server] && !connected[server] {
		return mcpBucketPending
	}
	return mcpBucketMissing
}

// mcpCatalog returns the live MCP catalog: every tool of every connected
// server, deferred or not. Both actions rank and resolve against this,
// never against the filtered tools array, so search can find a tool the
// model cannot yet call -- which is the whole point of searching.
func (s *Session) mcpCatalog(ctx context.Context) []provider.ToolDef {
	if s.cfg.MCP == nil {
		return nil
	}
	return s.cfg.MCP.Tools(ctx)
}

// mcpLoadedNames reports which MCP tools are in the tools array right now,
// computed from the same plan a request would build (see planMCPTools) over
// the catalog the caller already fetched -- one MCPRegistry.Tools call for
// the whole action. This is what search's Loaded flag reports.
func (s *Session) mcpLoadedNames(catalog []provider.ToolDef) map[string]bool {
	plan := s.planMCPToolsFrom(catalog, skipCatalogSegment)
	out := make(map[string]bool, len(plan.defs))
	for _, d := range plan.defs {
		out[d.Name] = true
	}
	return out
}
