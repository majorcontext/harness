// Package anthropic is the provider adapter for the Anthropic Messages API.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
)

// Client is a provider.Provider for the Anthropic Messages API. The zero
// value plus APIKey is usable; nothing touches the network until Stream.
type Client struct {
	APIKey     string
	BaseURL    string       // defaults to https://api.anthropic.com
	HTTPClient *http.Client // defaults to http.DefaultClient
}

func (c *Client) Name() string { return Family }

func (c *Client) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("anthropic: no API key configured (set ANTHROPIC_API_KEY)")
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
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("X-Api-Key", c.APIKey)
	httpReq.Header.Set("Anthropic-Version", apiVersion)

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
		msg := fmt.Sprintf("anthropic: %s (%s, HTTP %d)", body.Error.Message, body.Error.Type, resp.StatusCode)
		if promptTokens, limit, ok := parseContextOverflow(body.Error.Type, resp.StatusCode, body.Error.Message); ok {
			return &provider.Error{
				Kind:         provider.ErrKindContextOverflow,
				Raw:          msg,
				PromptTokens: promptTokens,
				TokenLimit:   limit,
			}
		}
		return errors.New(msg)
	}
	return fmt.Errorf("anthropic: HTTP %d", resp.StatusCode)
}

// contextOverflowPattern matches Anthropic's context-overflow message shape,
// e.g. "prompt is too long: 205102 tokens > 200000 maximum". Anthropic gives
// no distinct error type or code for this — it is a plain invalid_request_
// error like any other bad request — so this is the one place message
// matching is tolerated (see provider.Error's doc comment): scoped to this
// adapter, never the engine, and gated on the structural signal available
// (HTTP 400 + invalid_request_error) before ever inspecting the message.
var contextOverflowPattern = regexp.MustCompile(`prompt is too long: (\d+) tokens > (\d+) maximum`)

// parseContextOverflow classifies an Anthropic error as a context/prompt
// overflow: structurally, it must be a 400 invalid_request_error (the only
// status/type combination Anthropic uses for this); within that, the
// message is matched against contextOverflowPattern to extract the prompt
// size and limit. ok is false whenever either check fails, including an
// invalid_request_error whose message doesn't name a token limit (a
// different bad-request cause entirely) — so this never over-classifies.
func parseContextOverflow(errType string, status int, message string) (promptTokens, limit int, ok bool) {
	if status != http.StatusBadRequest || errType != "invalid_request_error" {
		return 0, 0, false
	}
	m := contextOverflowPattern.FindStringSubmatch(message)
	if m == nil {
		return 0, 0, false
	}
	promptTokens, err1 := strconv.Atoi(m[1])
	limit, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return promptTokens, limit, true
}

// assembledBlock accumulates one content block across SSE deltas.
type assembledBlock struct {
	kind      string // text | tool_use | thinking | redacted_thinking
	text      bytes.Buffer
	toolID    string
	toolName  string
	inputJSON bytes.Buffer
	signature string
	redacted  string
}

// stream implements provider.Stream over the Messages API SSE protocol. It
// forwards deltas as they arrive and assembles the canonical assistant
// message, which it delivers with EventDone on message_stop.
type stream struct {
	body  io.Closer
	r     *bufio.Reader
	model message.ModelRef

	msgID      string
	blocks     []*assembledBlock
	usage      provider.Usage
	stopReason provider.StopReason

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

func (s *stream) handle(name string, data []byte) error {
	switch name {
	case "message_start":
		var ev struct {
			Message struct {
				ID    string `json:"id"`
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("anthropic: bad message_start: %w", err)
		}
		s.msgID = ev.Message.ID
		s.usage.InputTokens = ev.Message.Usage.InputTokens
		s.usage.CacheWriteTokens = ev.Message.Usage.CacheCreationInputTokens
		s.usage.CacheReadTokens = ev.Message.Usage.CacheReadInputTokens

	case "content_block_start":
		var ev struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Text      string `json:"text"`
				Thinking  string `json:"thinking"`
				Data      string `json:"data"`
				Signature string `json:"signature"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("anthropic: bad content_block_start: %w", err)
		}
		b := &assembledBlock{kind: ev.ContentBlock.Type}
		switch ev.ContentBlock.Type {
		case "text":
			b.text.WriteString(ev.ContentBlock.Text)
		case "tool_use":
			b.toolID = ev.ContentBlock.ID
			b.toolName = ev.ContentBlock.Name
		case "thinking":
			b.text.WriteString(ev.ContentBlock.Thinking)
			b.signature = ev.ContentBlock.Signature
		case "redacted_thinking":
			b.redacted = ev.ContentBlock.Data
		}
		s.blocks = append(s.blocks, b)

	case "content_block_delta":
		var ev struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("anthropic: bad content_block_delta: %w", err)
		}
		if ev.Index < 0 || ev.Index >= len(s.blocks) {
			return fmt.Errorf("anthropic: delta for unknown block %d", ev.Index)
		}
		b := s.blocks[ev.Index]
		switch ev.Delta.Type {
		case "text_delta":
			b.text.WriteString(ev.Delta.Text)
			s.queue = append(s.queue, provider.Event{Type: provider.EventTextDelta, Text: ev.Delta.Text})
		case "input_json_delta":
			b.inputJSON.WriteString(ev.Delta.PartialJSON)
		case "thinking_delta":
			b.text.WriteString(ev.Delta.Thinking)
			s.queue = append(s.queue, provider.Event{Type: provider.EventReasoningDelta, Text: ev.Delta.Thinking})
		case "signature_delta":
			b.signature += ev.Delta.Signature
		}

	case "content_block_stop":
		var ev struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("anthropic: bad content_block_stop: %w", err)
		}
		if ev.Index >= 0 && ev.Index < len(s.blocks) {
			if b := s.blocks[ev.Index]; b.kind == "tool_use" {
				s.queue = append(s.queue, provider.Event{Type: provider.EventToolCall, ToolCall: b.toolCall()})
			}
		}

	case "message_delta":
		var ev struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("anthropic: bad message_delta: %w", err)
		}
		if ev.Delta.StopReason != "" {
			s.stopReason = mapStopReason(ev.Delta.StopReason)
		}
		if ev.Usage.OutputTokens > 0 {
			s.usage.OutputTokens = ev.Usage.OutputTokens
		}

	case "message_stop":
		msg := s.assemble()
		s.queue = append(s.queue, provider.Event{
			Type:       provider.EventDone,
			Message:    msg,
			StopReason: s.stopReason,
			Usage:      s.usage,
		})
		s.done = true

	case "error":
		var ev struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("anthropic: stream error: %s", data)
		}
		return fmt.Errorf("anthropic: %s (%s)", ev.Error.Message, ev.Error.Type)

	case "ping":
		// Keep-alive; nothing to do.
	}
	return nil
}

func (b *assembledBlock) toolCall() *message.ToolCall {
	args := b.inputJSON.Bytes()
	if len(args) == 0 {
		args = []byte(`{}`)
	}
	return &message.ToolCall{
		// The provider's ID becomes the canonical CallID: it is wire-safe
		// here by construction, so same-provider replay preserves it and
		// keeps the prompt cache warm.
		CallID:    b.toolID,
		Name:      b.toolName,
		Arguments: json.RawMessage(bytes.Clone(args)),
	}
}

// assemble builds the canonical assistant message from accumulated blocks.
func (s *stream) assemble() *message.Message {
	msg := &message.Message{
		ID:        s.msgID,
		Role:      message.RoleAssistant,
		Model:     s.model,
		CreatedAt: time.Now().UTC(),
	}
	for _, b := range s.blocks {
		switch b.kind {
		case "text":
			if b.text.Len() > 0 {
				msg.Parts = append(msg.Parts, &message.Text{Text: b.text.String()})
			}
		case "tool_use":
			msg.Parts = append(msg.Parts, b.toolCall())
		case "thinking":
			data, _ := json.Marshal(anthropicReasoningData{Signature: b.signature})
			msg.Parts = append(msg.Parts, &message.Reasoning{
				Text:         b.text.String(),
				ProviderData: message.ProviderData{Family: data},
			})
		case "redacted_thinking":
			data, _ := json.Marshal(anthropicReasoningData{Redacted: b.redacted})
			msg.Parts = append(msg.Parts, &message.Reasoning{
				ProviderData: message.ProviderData{Family: data},
			})
		}
	}
	return msg
}

func mapStopReason(s string) provider.StopReason {
	switch s {
	case "end_turn", "stop_sequence":
		return provider.StopEndTurn
	case "tool_use":
		return provider.StopToolUse
	case "max_tokens":
		return provider.StopMaxTokens
	case "refusal":
		return provider.StopRefusal
	default:
		return provider.StopOther
	}
}
