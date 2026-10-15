package claudecode

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/majorcontext/harness/protocol"
)

// searchTool is the CLI built-in that searches the web. The CLI runs it in its
// own model call, where no hook of a plugin can run, so a run offers it only
// when Request.WebSearch asks for it.
const searchTool = "WebSearch"

// allowSearch adds WebSearch to the pre-approved tools of args when the run
// offers it, so the CLI runs the search with no prompt. args holds at most one
// --allowedTools flag.
func allowSearch(args []string, search bool) []string {
	if !search {
		return args
	}
	if i := slices.Index(args, "--allowedTools"); i >= 0 && i+1 < len(args) {
		out := slices.Clone(args)
		out[i+1] += "," + searchTool
		return out
	}
	return append(slices.Clone(args), "--allowedTools", searchTool)
}

// searchQuery reads the query of a WebSearch call.
func searchQuery(args json.RawMessage) string {
	var in struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(args, &in)
	return in.Query
}

// structuredResults reads the sources of the tool_use_result of a frame:
// {"results":[{"content":[{"title","url"}]}, "a note"]}. An item that is not an object is skipped.
func structuredResults(raw json.RawMessage) []protocol.SearchResult {
	var in struct {
		Results []json.RawMessage `json:"results"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	var out []protocol.SearchResult
	for _, item := range in.Results {
		var r struct {
			Content []protocol.SearchResult `json:"content"`
		}
		if json.Unmarshal(item, &r) == nil {
			out = append(out, r.Content...)
		}
	}
	return out
}

// searchPart maps the result of a WebSearch call to a web_search part. The
// sources are the structured results of the frame, else the list that follows
// "Links: " in the result text, a JSON list of {title, url}. A result with
// neither maps to a search with no sources.
func searchPart(callID, query string, res protocol.LogPart, structured []protocol.SearchResult) protocol.LogPart {
	p := protocol.LogPart{Type: protocol.LogPartWebSearch, CallID: callID, Query: query}
	if res.IsError {
		p.IsError, p.Text = true, res.Text
		return p
	}
	if len(structured) > 0 {
		p.Results = structured
		return p
	}
	_, links, ok := strings.Cut(res.Text, "Links: ")
	if !ok {
		return p
	}
	var list []protocol.SearchResult
	if err := json.NewDecoder(strings.NewReader(links)).Decode(&list); err == nil {
		p.Results = list
	}
	return p
}

// holdSearches removes the WebSearch calls from parts and
// remembers their queries. A search is one item, which its result records.
func (r *run) holdSearches(parts []protocol.LogPart) []protocol.LogPart {
	if !r.allowed[searchTool] {
		return parts
	}
	return slices.DeleteFunc(parts, func(p protocol.LogPart) bool {
		if p.Type != protocol.LogPartToolCall || p.Name != searchTool {
			return false
		}
		if r.searches == nil {
			r.searches = map[string]string{}
		}
		r.searches[p.CallID] = searchQuery(p.Arguments)
		r.names[p.CallID] = searchTool
		r.calls[p.CallID] = true
		return true
	})
}

// recordSearches records the results of held WebSearch calls as assistant
// items, and returns the other results.
func (r *run) recordSearches(parts []protocol.LogPart, toolUseResult json.RawMessage) ([]protocol.LogPart, error) {
	structured := structuredResults(toolUseResult)
	if len(parts) != 1 {
		structured = nil
	}
	var rest []protocol.LogPart
	for _, p := range parts {
		query, ok := r.searches[p.CallID]
		if !ok || p.Type != protocol.LogPartToolResult {
			rest = append(rest, p)
			continue
		}
		delete(r.searches, p.CallID)
		r.spoke = true
		item := protocol.LogMessage{Role: protocol.RoleAssistant, Parts: []protocol.LogPart{searchPart(p.CallID, query, p, structured)}}
		if err := r.thread().Item(item); err != nil {
			return nil, err
		}
		r.replay()
	}
	return rest, nil
}
