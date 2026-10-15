package anthropic

import (
	"bytes"
	"encoding/json"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/prompt"
)

const (
	// webSearchType is the server tool type of the Messages API. The API runs
	// the search inside the model call.
	webSearchType = "web_search_20260209"
	webSearchName = "web_search"
	// webSearchMaxUses bounds the searches of one model call, which the API bills per request.
	webSearchMaxUses = 5
)

// NativeWebSearch reports that the Messages API runs a web search inside a model call.
func (c *Client) NativeWebSearch(string) bool { return true }

// webSearchData is the entry of ProviderData[Family] on a WebSearch part: the
// content of the web_search_tool_result block, which holds encrypted result
// bodies that the next request must send back unchanged.
type webSearchData struct {
	Content json.RawMessage `json:"content"`
}

// webSearch maps a server_tool_use block and the result block that follows
// it to a canonical part. A block of another server tool maps to nothing.
func (s *stream) webSearch(b *assembledBlock) *message.WebSearch {
	if b.toolName != webSearchName {
		return nil
	}
	var in struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(b.inputJSON.Bytes(), &in)
	w := &message.WebSearch{ID: b.toolID, Query: in.Query}
	for _, r := range s.blocks {
		if r.kind != "web_search_tool_result" || r.toolID != b.toolID {
			continue
		}
		w.Results, w.Error = searchOutcome(r.result)
		if data, err := json.Marshal(webSearchData{Content: r.result}); err == nil && w.Error == "" {
			w.ProviderData.Set(Family, data)
		}
	}
	return w
}

// searchOutcome reads the content of a web_search_tool_result block: a list
// of results, or an error object with an error_code.
func searchOutcome(content json.RawMessage) (results []message.SearchResult, errCode string) {
	var list []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(content, &list); err == nil {
		for _, r := range list {
			results = append(results, message.SearchResult{Title: r.Title, URL: r.URL})
		}
		return results, ""
	}
	var e struct {
		ErrorCode string `json:"error_code"`
	}
	if json.Unmarshal(content, &e) == nil && e.ErrorCode != "" {
		return nil, e.ErrorCode
	}
	return nil, "unknown"
}

// searchBlocks renders a WebSearch part for a replay. A search of this family
// goes back as the server_tool_use block and its result block. Any other
// search goes as one text block, which keeps the query and the sources.
func searchBlocks(w *message.WebSearch) []apiBlock {
	raw, ok := w.ProviderData.Get(Family)
	var data webSearchData
	if !ok || w.ID == "" || json.Unmarshal(raw, &data) != nil || len(data.Content) == 0 {
		return []apiBlock{{Type: "text", Text: prompt.NeutralizeEngineContextSentinel(w.Summary())}}
	}
	input, err := json.Marshal(struct {
		Query string `json:"query"`
	}{w.Query})
	if err != nil {
		return []apiBlock{{Type: "text", Text: prompt.NeutralizeEngineContextSentinel(w.Summary())}}
	}
	return []apiBlock{
		{Type: "server_tool_use", ID: w.ID, Name: webSearchName, Input: input},
		{Type: "web_search_tool_result", ToolUseID: w.ID, ResultContent: bytes.Clone(data.Content)},
	}
}

// webSearchTool is the server tool definition that a request with Request.WebSearch sends.
func webSearchTool() apiToolDef {
	return apiToolDef{Type: webSearchType, Name: webSearchName, MaxUses: webSearchMaxUses, AllowedCallers: []string{"direct"}}
}

// MarshalJSON sends ResultContent, when set, as the content of the block.
func (b apiBlock) MarshalJSON() ([]byte, error) {
	type plain apiBlock
	if len(b.ResultContent) == 0 {
		return json.Marshal(plain(b))
	}
	return json.Marshal(struct {
		plain
		Content json.RawMessage `json:"content"`
	}{plain(b), b.ResultContent})
}
