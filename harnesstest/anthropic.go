package harnesstest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
)

type wireRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	Thinking  struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	} `json:"thinking"`
	OutputConfig struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
	ServiceTier string          `json:"service_tier"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	System      json.RawMessage `json:"system"`
	Messages    []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
}

type wireBlock struct {
	Type         string          `json:"type"`
	CacheControl json.RawMessage `json:"cache_control"`
	Text         string          `json:"text"`
	Thinking     string          `json:"thinking"`
	Signature    string          `json:"signature"`
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Input        map[string]any  `json:"input"`
	ToolUseID    string          `json:"tool_use_id"`
	Content      json.RawMessage `json:"content"`
	IsError      bool            `json:"is_error"`
	Source       struct {
		Type      string `json:"type"`
		URL       string `json:"url"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

func decodeRequest(body []byte) (Request, error) {
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		return Request{}, err
	}
	req := Request{
		System: joinText(w.System), Model: w.Model, MaxTokens: w.MaxTokens, ServiceTier: w.ServiceTier,
		Effort: w.OutputConfig.Effort, ThinkingType: w.Thinking.Type, ThinkingBudget: w.Thinking.BudgetTokens, Temperature: w.Temperature, TopP: w.TopP,
	}
	for _, t := range w.Tools {
		req.Tools = append(req.Tools, t.Name)
		if t.Description == "" && len(t.InputSchema) == 0 {
			continue
		}
		if req.ToolSpecs == nil {
			req.ToolSpecs = map[string]ToolSpec{}
		}
		req.ToolSpecs[t.Name] = ToolSpec{Description: t.Description, InputSchema: t.InputSchema}
	}
	sort.Strings(req.Tools)

	for i, b := range blocks(w.System) {
		if len(b.CacheControl) > 0 {
			req.CacheBreakpoints = append(req.CacheBreakpoints, CacheBreakpoint{Message: -1, Block: i})
		}
	}

	toolNames := map[string]string{}
	var pending []string
	for mi, m := range w.Messages {
		msg := Message{Role: m.Role}
		answered, leading := 0, true
		awaiting := slices.Clone(pending)
		for bi, b := range blocks(m.Content) {
			if len(b.CacheControl) > 0 {
				req.CacheBreakpoints = append(req.CacheBreakpoints, CacheBreakpoint{Message: mi, Block: bi})
			}
			if b.Type != "tool_result" {
				leading = false
			}
			switch b.Type {
			case "text":
				msg.Parts = append(msg.Parts, Part{Kind: "text", Text: b.Text})
			case "thinking":
				msg.Parts = append(msg.Parts, Part{Kind: "thinking", Text: b.Thinking, Signature: b.Signature})
			case "image", "document":
				msg.Parts = append(msg.Parts, Part{Kind: b.Type, Text: sourceURI(b)})
			case "server_tool_use", "web_search_tool_result":
				msg.Parts = append(msg.Parts, searchPart(b))
			case "tool_use":
				toolNames[b.ID] = b.Name
				msg.Parts = append(msg.Parts, Part{Kind: "tool_use", ToolName: b.Name, ToolInput: b.Input, ToolUseID: b.ID})
			case "tool_result":
				at := slices.Index(awaiting, b.ToolUseID)
				if !leading || m.Role != "user" || at < 0 {
					return Request{}, fmt.Errorf("tool_result %q is not at the start of the user message that follows its tool_use, or answers it twice", b.ToolUseID)
				}
				awaiting = slices.Delete(awaiting, at, at+1)
				answered++
				msg.Parts = append(msg.Parts, Part{
					Kind: "tool_result", Text: joinText(b.Content), ToolName: toolNames[b.ToolUseID],
					ToolUseID: b.ToolUseID, IsError: b.IsError, Images: imageURIs(b.Content),
				})
			}
		}
		if answered != len(pending) {
			return Request{}, fmt.Errorf("%d tool_use blocks have no tool_result at the start of the next user message", len(pending)-answered)
		}
		pending = pending[:0]
		for _, p := range msg.Parts {
			if m.Role == "assistant" && p.Kind == "tool_use" {
				pending = append(pending, p.ToolUseID)
			}
		}
		req.Messages = append(req.Messages, msg)
	}
	if len(pending) > 0 {
		return Request{}, fmt.Errorf("%d tool_use blocks have no tool_result message", len(pending))
	}
	return req, nil
}

// sourceURI is the URL of an image or document block, or the data URI of its bytes.
func sourceURI(b wireBlock) string {
	if b.Source.Type == "url" {
		return b.Source.URL
	}
	return "data:" + b.Source.MediaType + ";base64," + b.Source.Data
}

func blocks(raw json.RawMessage) []wireBlock {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []wireBlock{{Type: "text", Text: s}}
	}
	var bs []wireBlock
	_ = json.Unmarshal(raw, &bs)
	return bs
}

// imageURIs is the data URI of each image block of raw, in order.
func imageURIs(raw json.RawMessage) []string {
	var uris []string
	for _, b := range blocks(raw) {
		if b.Type == "image" {
			uris = append(uris, "data:"+b.Source.MediaType+";base64,"+b.Source.Data)
		}
	}
	return uris
}

func joinText(raw json.RawMessage) string {
	var texts []string
	for _, b := range blocks(raw) {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

var errorTypes = map[int]string{
	http.StatusBadRequest:      "invalid_request_error",
	http.StatusUnauthorized:    "authentication_error",
	http.StatusNotFound:        "not_found_error",
	http.StatusTooManyRequests: "rate_limit_error",
	529:                        "overloaded_error",
}

func writeError(w http.ResponseWriter, status int, msg string) {
	typ, ok := errorTypes[status]
	if !ok {
		typ = "api_error"
	}
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeReplyError(w http.ResponseWriter, rep Reply) {
	if rep.RetryAfter != "" {
		w.Header().Set("Retry-After", rep.RetryAfter)
	}
	msg := rep.ErrorMessage
	if msg == "" {
		msg = "harnesstest: scripted error"
	}
	writeError(w, rep.HTTPStatus, msg)
}

type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (s sseWriter) event(name string, data any) {
	b, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b)
	s.f.Flush()
}

type obj = map[string]any

func (s *Server) stream(w http.ResponseWriter, r *http.Request, n int, name string, rep Reply) {
	f, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no flusher")
		return
	}
	usage := rep.Usage
	if usage == (Usage{}) {
		usage = Usage{Input: 5, Output: 3}
	}
	stop := rep.StopReason
	if stop == "" {
		stop = "end_turn"
		if len(rep.ToolCalls) > 0 {
			stop = "tool_use"
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	e := sseWriter{w, f}
	e.event("message_start", obj{"type": "message_start", "message": obj{
		"id": fmt.Sprintf("msg_fake_%d", n), "usage": obj{"input_tokens": usage.Input},
	}})

	idx := 0
	gate := rep.Block && rep.Text == ""
	if rep.Reasoning != "" {
		e.event("content_block_start", obj{"type": "content_block_start", "index": idx, "content_block": obj{"type": "thinking", "thinking": "", "signature": ""}})
		e.event("content_block_delta", obj{"type": "content_block_delta", "index": idx, "delta": obj{"type": "thinking_delta", "thinking": rep.Reasoning}})
		e.event("content_block_delta", obj{"type": "content_block_delta", "index": idx, "delta": obj{"type": "signature_delta", "signature": "SIGNATURE_FIXTURE_1"}})
		e.event("content_block_stop", obj{"type": "content_block_stop", "index": idx})
		idx++
	}
	if rep.WebSearch != nil {
		idx = searchBlocks(e, idx, rep.WebSearch)
	}
	if rep.Text != "" || len(rep.ToolCalls) == 0 {
		e.event("content_block_start", obj{"type": "content_block_start", "index": idx, "content_block": textBlockStart(rep.WebSearch)})
		citeSources(e, idx, rep.WebSearch)
		e.event("content_block_delta", obj{"type": "content_block_delta", "index": idx, "delta": obj{"type": "text_delta", "text": rep.Text}})
		if rep.Block && !s.block(r, name) {
			return
		}
		if rep.Tail != "" {
			e.event("content_block_delta", obj{"type": "content_block_delta", "index": idx, "delta": obj{"type": "text_delta", "text": rep.Tail}})
			if rep.TailBlock && !s.block(r, name+":tail") {
				return
			}
		}
		e.event("content_block_stop", obj{"type": "content_block_stop", "index": idx})
		idx++
	}
	for _, tc := range rep.ToolCalls {
		input := tc.Input
		if input == nil {
			input = map[string]any{}
		}
		args, _ := json.Marshal(input)
		e.event("content_block_start", obj{"type": "content_block_start", "index": idx, "content_block": obj{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": obj{}}})
		e.event("content_block_delta", obj{"type": "content_block_delta", "index": idx, "delta": obj{"type": "input_json_delta", "partial_json": string(args)}})
		if gate {
			gate = false
			if !s.block(r, name) {
				return
			}
		}
		e.event("content_block_stop", obj{"type": "content_block_stop", "index": idx})
		idx++
	}
	e.event("message_delta", obj{"type": "message_delta", "delta": obj{"stop_reason": stop}, "usage": usageDelta(usage, rep.WebSearch)})
	e.event("message_stop", obj{"type": "message_stop"})
}

// searchBlocks streams the server_tool_use block of a search and the block of
// its results, and returns the index of the next block.
func searchBlocks(e sseWriter, idx int, ws *WebSearch) int {
	id := ws.ID
	if id == "" {
		id = "srvtoolu_1"
	}
	input, _ := json.Marshal(obj{"query": ws.Query})
	e.event("content_block_start", obj{"type": "content_block_start", "index": idx, "content_block": obj{"type": "server_tool_use", "id": id, "name": "web_search", "input": obj{}}})
	e.event("content_block_delta", obj{"type": "content_block_delta", "index": idx, "delta": obj{"type": "input_json_delta", "partial_json": string(input)}})
	e.event("content_block_stop", obj{"type": "content_block_stop", "index": idx})
	if ws.Pending {
		return idx + 1
	}
	var results any = ws.resultBlocks()
	if ws.Error != "" {
		results = obj{"type": "web_search_tool_result_error", "error_code": ws.Error}
	}
	e.event("content_block_start", obj{"type": "content_block_start", "index": idx + 1, "content_block": obj{"type": "web_search_tool_result", "tool_use_id": id, "content": results}})
	e.event("content_block_stop", obj{"type": "content_block_stop", "index": idx + 1})
	return idx + 2
}

// resultURLs lists the url of each web_search_result in the content of a web_search_tool_result block.
func resultURLs(raw json.RawMessage) []string {
	var list []struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(raw, &list)
	urls := make([]string, len(list))
	for i, r := range list {
		urls[i] = r.URL
	}
	return urls
}

// textBlockStart is the text block of a reply. A reply with a search carries the citations of the block.
func textBlockStart(ws *WebSearch) obj {
	start := obj{"type": "text", "text": ""}
	if ws != nil {
		start["citations"] = []obj{}
	}
	return start
}

// citeSources streams one citation of the text block idx for each source of a search.
func citeSources(e sseWriter, idx int, ws *WebSearch) {
	if ws == nil {
		return
	}
	for _, src := range ws.Sources {
		e.event("content_block_delta", obj{"type": "content_block_delta", "index": idx, "delta": obj{"type": "citations_delta", "citation": obj{
			"type": "web_search_result_location", "cited_text": src.Title, "url": src.URL, "title": src.Title, "encrypted_index": "ENCRYPTED_INDEX_FIXTURE_1"}}})
	}
}

// usageDelta is the usage of the message_delta event. A search adds its count.
func usageDelta(u Usage, ws *WebSearch) obj {
	usage := obj{"output_tokens": u.Output}
	if ws != nil {
		if ws.StepInput > 0 {
			usage["input_tokens"] = u.Input + ws.StepInput
		}
		usage["server_tool_use"] = obj{"web_search_requests": 1, "web_fetch_requests": 0}
	}
	return usage
}

// searchPart is the part of a server_tool_use block of a search, or of the block of its results.
func searchPart(b wireBlock) Part {
	if b.Type == "server_tool_use" {
		query, _ := b.Input["query"].(string)
		return Part{Kind: "web_search", Text: query, ToolName: b.Name, ToolUseID: b.ID}
	}
	return Part{Kind: "web_search_result", Text: strings.Join(resultURLs(b.Content), "\n"), ToolUseID: b.ToolUseID}
}

func (ws *WebSearch) resultBlocks() []obj {
	results := make([]obj, len(ws.Sources))
	for i, src := range ws.Sources {
		results[i] = obj{"type": "web_search_result", "title": src.Title, "url": src.URL, "encrypted_content": fmt.Sprintf("ENCRYPTED_CONTENT_FIXTURE_%d", i+1), "page_age": "1 day ago"}
	}
	return results
}
