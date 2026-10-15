package acp

import (
	"encoding/json"

	"github.com/majorcontext/harness/protocol"
)

type resourceLink struct {
	Type string `json:"type"`
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// describeSearch maps a web_search part to a tool_call update. The provider ran
// the search inside the model call, so the call arrives complete: its query is
// the raw input, and its sources are the content, as links.
func describeSearch(part protocol.LogPart) toolCallBody {
	input, _ := json.Marshal(struct {
		Query string `json:"query"`
	}{part.Query})
	body := toolCallBody{Kind: "tool_call", ID: part.CallID, Name: protocol.LogPartWebSearch, Title: "Search: " + part.Query,
		ToolKind: "fetch", Status: "completed", RawInput: input}
	if part.IsError {
		body.Status = "failed"
		if part.Text != "" {
			body.Content = []any{textItem(part.Text)}
		}
		return body
	}
	for _, r := range part.Results {
		name := r.Title
		if name == "" {
			name = r.URL
		}
		body.Content = append(body.Content, contentItem{Type: "content", Content: resourceLink{Type: "resource_link", URI: r.URL, Name: name}})
	}
	return body
}
