// Package openai is the provider adapter for the OpenAI Responses API.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

const defaultBaseURL = "https://api.openai.com"

// Client is a provider.Provider for the OpenAI Responses API. The zero value
// plus APIKey is usable; nothing touches the network until Stream.
type Client struct {
	APIKey     string
	BaseURL    string       // defaults to https://api.openai.com
	HTTPClient *http.Client // defaults to http.DefaultClient
}

func (c *Client) Name() string { return Family }

func (c *Client) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("openai: no API key configured (set OPENAI_API_KEY)")
	}
	wire, err := transcodeRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}

	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, apiError(resp)
	}
	return &stream{
		body:  resp.Body,
		r:     bufio.NewReader(resp.Body),
		model: req.Model,
	}, nil
}

func apiError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Error.Message != "" {
		return fmt.Errorf("openai: %s (%s, HTTP %d)", body.Error.Message, body.Error.Type, resp.StatusCode)
	}
	return fmt.Errorf("openai: HTTP %d", resp.StatusCode)
}

// assembledItem accumulates one output item across SSE events, keyed by the
// item's output_index.
type assembledItem struct {
	kind   string // message | function_call | reasoning
	text   bytes.Buffer
	callID string
	name   string
	args   json.RawMessage
	raw    json.RawMessage // reasoning: the entire item JSON, replayed verbatim
}

// stream implements provider.Stream over the Responses API SSE protocol. It
// forwards deltas as they arrive and assembles the canonical assistant
// message, delivered with EventDone on response.completed.
type stream struct {
	body  io.Closer
	r     *bufio.Reader
	model message.ModelRef

	respID      string
	items       []*assembledItem
	usage       provider.Usage
	hasToolCall bool

	queue []provider.Event
	done  bool
}

func (s *stream) Close() error { return s.body.Close() }

func (s *stream) Next() (provider.Event, error) {
	for {
		if len(s.queue) > 0 {
			ev := s.queue[0]
			s.queue = s.queue[1:]
			return ev, nil
		}
		if s.done {
			return provider.Event{}, io.EOF
		}
		name, data, err := s.readSSE()
		if err != nil {
			return provider.Event{}, err
		}
		if err := s.handle(name, data); err != nil {
			return provider.Event{}, err
		}
	}
}

// readSSE reads one server-sent event: an "event:" line, "data:" lines
// (concatenated), terminated by a blank line.
func (s *stream) readSSE() (name string, data []byte, err error) {
	var buf bytes.Buffer
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			if err == io.EOF && (name != "" || buf.Len() > 0) {
				return name, buf.Bytes(), nil
			}
			return "", nil, err
		}
		line = trimEOL(line)
		switch {
		case line == "":
			if name != "" || buf.Len() > 0 {
				return name, buf.Bytes(), nil
			}
		case len(line) > 6 && line[:6] == "event:":
			name = trimSpaceLeft(line[6:])
		case len(line) > 5 && line[:5] == "data:":
			buf.WriteString(trimSpaceLeft(line[5:]))
		}
		// Comments and unknown fields are ignored per the SSE spec.
	}
}

func trimEOL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func trimSpaceLeft(s string) string {
	for len(s) > 0 && s[0] == ' ' {
		s = s[1:]
	}
	return s
}

// itemAt returns the assembled item at output_index idx, growing the slice as
// needed.
func (s *stream) itemAt(idx int) *assembledItem {
	for len(s.items) <= idx {
		s.items = append(s.items, nil)
	}
	if s.items[idx] == nil {
		s.items[idx] = &assembledItem{}
	}
	return s.items[idx]
}

func (s *stream) handle(name string, data []byte) error {
	switch name {
	case "response.created":
		var ev struct {
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("openai: bad response.created: %w", err)
		}
		s.respID = ev.Response.ID

	case "response.output_text.delta":
		var ev struct {
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("openai: bad response.output_text.delta: %w", err)
		}
		it := s.itemAt(ev.OutputIndex)
		if it.kind == "" {
			it.kind = "message"
		}
		it.text.WriteString(ev.Delta)
		s.queue = append(s.queue, provider.Event{Type: provider.EventTextDelta, Text: ev.Delta})

	case "response.reasoning_summary_text.delta":
		var ev struct {
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("openai: bad response.reasoning_summary_text.delta: %w", err)
		}
		it := s.itemAt(ev.OutputIndex)
		if it.kind == "" {
			it.kind = "reasoning"
		}
		it.text.WriteString(ev.Delta)
		s.queue = append(s.queue, provider.Event{Type: provider.EventReasoningDelta, Text: ev.Delta})

	case "response.output_item.done":
		var ev struct {
			OutputIndex int             `json:"output_index"`
			Item        json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("openai: bad response.output_item.done: %w", err)
		}
		var head struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal(ev.Item, &head); err != nil {
			return fmt.Errorf("openai: bad output item: %w", err)
		}
		it := s.itemAt(ev.OutputIndex)
		switch head.Type {
		case "function_call":
			it.kind = "function_call"
			it.callID = head.CallID
			it.name = head.Name
			it.args = argsRaw(head.Arguments)
			s.hasToolCall = true
			s.queue = append(s.queue, provider.Event{Type: provider.EventToolCall, ToolCall: it.toolCall()})
		case "reasoning":
			it.kind = "reasoning"
			it.raw = append(json.RawMessage(nil), ev.Item...)
		case "message":
			if it.kind == "" {
				it.kind = "message"
			}
		}

	case "response.completed", "response.incomplete":
		// Both are terminal: response.incomplete is a truncated-but-usable
		// response whose incomplete_details.reason maps to the stop reason.
		var ev struct {
			Response struct {
				IncompleteDetails struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
				Usage struct {
					InputTokens        int `json:"input_tokens"`
					OutputTokens       int `json:"output_tokens"`
					InputTokensDetails struct {
						CachedTokens int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("openai: bad %s: %w", name, err)
		}
		// The Responses API reports input_tokens INCLUSIVE of the cached
		// portion (input_tokens_details.cached_tokens is a subset). The
		// provider.Usage contract wants disjoint components, so report the
		// uncached remainder; the sum reconstructs the wire total.
		cached := ev.Response.Usage.InputTokensDetails.CachedTokens
		uncached := ev.Response.Usage.InputTokens - cached
		if uncached < 0 {
			uncached = 0
		}
		s.usage.InputTokens = uncached
		s.usage.OutputTokens = ev.Response.Usage.OutputTokens
		s.usage.CacheReadTokens = cached

		var stop provider.StopReason
		switch {
		case name == "response.incomplete":
			stop = mapIncompleteReason(ev.Response.IncompleteDetails.Reason)
		case s.hasToolCall:
			stop = provider.StopToolUse
		default:
			stop = provider.StopEndTurn
		}
		s.queue = append(s.queue, provider.Event{
			Type:       provider.EventDone,
			Message:    s.assemble(),
			StopReason: stop,
			Usage:      s.usage,
		})
		s.done = true

	case "response.failed", "error":
		var ev struct {
			Message  string `json:"message"`
			Response struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("openai: stream error: %s", data)
		}
		switch {
		case ev.Response.Error.Message != "":
			return fmt.Errorf("openai: %s (%s)", ev.Response.Error.Message, ev.Response.Error.Code)
		case ev.Error.Message != "":
			return fmt.Errorf("openai: %s (%s)", ev.Error.Message, ev.Error.Code)
		case ev.Message != "":
			return fmt.Errorf("openai: %s", ev.Message)
		default:
			return fmt.Errorf("openai: stream error: %s", data)
		}
	}
	return nil
}

// mapIncompleteReason maps response.incomplete_details.reason to a canonical
// stop reason.
func mapIncompleteReason(reason string) provider.StopReason {
	switch reason {
	case "max_output_tokens":
		return provider.StopMaxTokens
	case "content_filter":
		return provider.StopRefusal
	default:
		return provider.StopOther
	}
}

func argsRaw(args string) json.RawMessage {
	if args == "" {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(args)
}

func (it *assembledItem) toolCall() *message.ToolCall {
	args := it.args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	return &message.ToolCall{
		// The provider's call_id becomes the canonical CallID: it is wire-safe
		// here by construction, so same-provider replay preserves it and keeps
		// the prompt cache warm.
		CallID:    it.callID,
		Name:      it.name,
		Arguments: append(json.RawMessage(nil), args...),
	}
}

// assemble builds the canonical assistant message from accumulated items.
func (s *stream) assemble() *message.Message {
	msg := &message.Message{
		ID:        s.respID,
		Role:      message.RoleAssistant,
		Model:     s.model,
		CreatedAt: time.Now().UTC(),
	}
	for _, it := range s.items {
		if it == nil {
			continue
		}
		switch it.kind {
		case "message":
			if it.text.Len() > 0 {
				msg.Parts = append(msg.Parts, &message.Text{Text: it.text.String()})
			}
		case "reasoning":
			if len(it.raw) == 0 {
				continue
			}
			msg.Parts = append(msg.Parts, &message.Reasoning{
				Text:         it.text.String(),
				ProviderData: message.ProviderData{Family: it.raw},
			})
		case "function_call":
			msg.Parts = append(msg.Parts, it.toolCall())
		}
	}
	return msg
}
