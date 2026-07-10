// Package openaicompat is a generic provider adapter for the OpenAI
// chat-completions wire format spoken by OpenRouter, Ollama, vLLM, and
// similar deployments — not OpenAI's own Responses API, which
// provider/openai implements.
//
// Family (the ModelRef.Provider value and ProviderData tag) is configurable
// because many unrelated deployments speak this same wire, each under its
// own provider family name in config.
package openaicompat

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

// Client is a provider.Provider for OpenAI-compatible chat-completions
// endpoints. The zero value plus Family, APIKey, and BaseURL is usable;
// nothing touches the network until Stream.
type Client struct {
	// Family is the provider family key: it becomes ModelRef.Provider and
	// the ProviderData tag this adapter reads. It is configurable (rather
	// than a package constant, as in provider/openai and
	// provider/anthropic) because many distinct deployments — OpenRouter,
	// Ollama, vLLM, and others — all speak this same wire under their own
	// family name.
	Family string
	APIKey string
	// BaseURL is the API root; the client POSTs to BaseURL+"/chat/completions".
	BaseURL string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
	// ExtraHeaders are sent verbatim on every request, e.g. OpenRouter's
	// HTTP-Referer and X-Title attribution headers.
	ExtraHeaders map[string]string
}

func (c *Client) Name() string { return c.Family }

func (c *Client) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if c.Family == "" {
		return nil, fmt.Errorf("openaicompat: no Family configured")
	}
	if c.APIKey == "" {
		return nil, fmt.Errorf("openaicompat(%s): no API key configured", c.Family)
	}
	wire, err := transcodeRequest(req, c.Family)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	for k, v := range c.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}

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
		return nil, apiError(c.Family, resp)
	}
	return &stream{
		body:   resp.Body,
		r:      bufio.NewReader(resp.Body),
		model:  req.Model,
		family: c.Family,
	}, nil
}

func apiError(family string, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	var err error
	if json.Unmarshal(raw, &body) == nil && body.Error.Message != "" {
		msg := fmt.Sprintf("openaicompat(%s): %s (%s, HTTP %d)", family, body.Error.Message, body.Error.Type, resp.StatusCode)
		// Context overflow is deterministic and disjoint from the retryable
		// statuses below — classify it first so it never reaches the
		// retryable mark.
		if promptTokens, limit, ok := parseContextOverflow(resp.StatusCode, body.Error.Code, body.Error.Message); ok {
			return &provider.Error{
				Kind:         provider.ErrKindContextOverflow,
				Raw:          msg,
				PromptTokens: promptTokens,
				TokenLimit:   limit,
			}
		}
		err = errors.New(msg)
	} else {
		err = fmt.Errorf("openaicompat(%s): HTTP %d", family, resp.StatusCode)
	}
	if class, ok := classifyStatus(resp.StatusCode); ok {
		return provider.MarkRetryable(err, class)
	}
	return err
}

// classifyStatus classifies an HTTP response status into a
// provider.RetryableClass (see GitHub issue #61): 429 is a rate limit, any
// other 5xx is a generic server error — both transient provider weather
// worth the goal loop's long backoff (engine/goal.go). Every other status
// (400s, auth) reports ok=false and stays a deterministic, fail-fast error
// exactly as before. Unlike provider/anthropic, this generic wire has no
// dedicated "overloaded" status distinct from a plain 5xx.
func classifyStatus(status int) (provider.RetryableClass, bool) {
	switch {
	case status == http.StatusTooManyRequests:
		return provider.RetryableRateLimited, true
	case status == 529:
		// Not a registered status, but OpenRouter proxies Anthropic's 529
		// overload responses through verbatim — classify it the same way
		// the anthropic adapter does rather than as a generic 5xx.
		return provider.RetryableOverloaded, true
	case status >= 500 && status <= 599:
		return provider.RetryableServerError, true
	default:
		return "", false
	}
}

// classifyWireCode classifies a mid-stream error "code", which is an int
// HTTP status on some wires (OpenRouter) and a string constant on others
// (OpenAI: "rate_limit_exceeded", "server_error"). Unknown or null codes
// classify as nothing — the error still surfaces, just without a
// retryable mark.
func classifyWireCode(raw json.RawMessage) (provider.RetryableClass, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var status int
	if err := json.Unmarshal(raw, &status); err == nil {
		return classifyStatus(status)
	}
	var code string
	if err := json.Unmarshal(raw, &code); err == nil {
		switch code {
		case "rate_limit_exceeded", "rate_limited":
			return provider.RetryableRateLimited, true
		case "server_error", "internal_server_error":
			return provider.RetryableServerError, true
		case "overloaded", "overloaded_error":
			return provider.RetryableOverloaded, true
		}
	}
	return "", false
}

// contextLimitPattern and contextResultPattern extract the model's input
// limit and the request's actual size from OpenAI's context-overflow
// message text, e.g. "This model's maximum context length is 8192 tokens.
// However, your messages resulted in 10191 tokens." Used both to enrich the
// structural (code-based) classification below with token counts, and as
// the sole classifier for compat deployments (self-hosted vLLM/Ollama/etc.)
// that emit this same wording but omit the "code" field OpenAI itself sets
// — the message-matching fallback the adapter (never the engine) tolerates.
var (
	contextLimitPattern  = regexp.MustCompile(`maximum context length is (\d+) tokens`)
	contextResultPattern = regexp.MustCompile(`resulted in (\d+) tokens`)
)

// parseContextOverflow classifies an OpenAI-compatible error as a context/
// prompt overflow. It prefers the structural signal the OpenAI API
// contract guarantees — code == "context_length_exceeded" on a 400 — over
// any message inspection; when that structural signal is present but the
// token counts can't be parsed from the message (a wording OpenAI hasn't
// used yet), it still classifies, just without PromptTokens/TokenLimit
// detail. Only when the structural signal is ABSENT does it fall back to
// message-matching the same wording other compat deployments echo,
// tolerated here inside the adapter per provider.Error's doc comment.
func parseContextOverflow(status int, code, message string) (promptTokens, limit int, ok bool) {
	if status != http.StatusBadRequest {
		return 0, 0, false
	}
	structural := code == "context_length_exceeded"
	limitMatch := contextLimitPattern.FindStringSubmatch(message)
	resultMatch := contextResultPattern.FindStringSubmatch(message)
	if !structural && (limitMatch == nil || resultMatch == nil) {
		return 0, 0, false
	}
	if limitMatch != nil {
		limit, _ = strconv.Atoi(limitMatch[1])
	}
	if resultMatch != nil {
		promptTokens, _ = strconv.Atoi(resultMatch[1])
	}
	return promptTokens, limit, true
}

// assembledToolCall accumulates one tool_calls fragment stream, keyed by its
// wire "index".
type assembledToolCall struct {
	id   string
	name string
	args bytes.Buffer
}

func (tc *assembledToolCall) toolCall() *message.ToolCall {
	args := tc.args.Bytes()
	if len(args) == 0 {
		args = []byte(`{}`)
	}
	return &message.ToolCall{
		CallID:    tc.id,
		Name:      tc.name,
		Arguments: json.RawMessage(bytes.Clone(args)),
	}
}

// stream implements provider.Stream over the chat-completions SSE protocol:
// lines "data: {json}" terminated by a final "data: [DONE]". It forwards
// deltas as they arrive and assembles the canonical assistant message,
// delivered with EventDone once [DONE] is seen (or once usage/finish_reason
// close the response out).
type stream struct {
	body   io.Closer
	r      *bufio.Reader
	model  message.ModelRef
	family string

	id   string
	text bytes.Buffer
	// reasoningText accumulates reasoning_content deltas into a Reasoning
	// part. Compat endpoints carry no signed/opaque reasoning payload the
	// way Anthropic's thinking blocks or OpenAI Responses' encrypted
	// reasoning items do, so this part never gets a ProviderData entry —
	// there is no reasoning replay on this wire, ever.
	reasoningText bytes.Buffer
	haveReasoning bool
	haveText      bool

	toolOrder []int
	toolCalls map[int]*assembledToolCall
	emitted   bool // tool call events already queued

	usage      provider.Usage
	haveUsage  bool
	stopReason provider.StopReason
	haveFinish bool

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
		line, err := s.readDataLine()
		if err != nil {
			return provider.Event{}, err
		}
		if err := s.handle(line); err != nil {
			return provider.Event{}, err
		}
	}
}

// readDataLine reads lines until it finds a non-empty "data: ..." line,
// returning its payload. Blank lines (event separators) and any other
// field lines are skipped, per SSE.
func (s *stream) readDataLine() ([]byte, error) {
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				line = trimEOL(line)
				if payload, ok := dataPayload(line); ok {
					return payload, nil
				}
				return nil, io.EOF
			}
			return nil, err
		}
		line = trimEOL(line)
		if payload, ok := dataPayload(line); ok {
			return payload, nil
		}
		// Blank lines and non-"data:" fields (comments, "event:", ...) are
		// ignored: this wire never sends anything but data lines in
		// practice, but skipping keeps the reader spec-compliant.
	}
}

func dataPayload(line string) ([]byte, bool) {
	if len(line) < 5 || line[:5] != "data:" {
		return nil, false
	}
	return []byte(trimSpaceLeft(line[5:])), true
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

// wireChunk is one "data: {...}" SSE payload.
type wireChunk struct {
	ID string `json:"id"`
	// Error is the OpenAI-compat mid-stream error shape ({"error":{...}}
	// as an SSE data payload) — how OpenRouter and friends report a
	// failure that begins after response headers were already sent.
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		// Code is an int on some wires (OpenRouter proxies upstream HTTP
		// statuses: 429, 529, 502...) and a string on others (OpenAI's
		// "rate_limit_exceeded", "server_error"), or null — RawMessage so
		// neither shape can fail the chunk unmarshal and resurrect the
		// silent-swallow this field exists to close.
		Code json.RawMessage `json:"code"`
	} `json:"error"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (s *stream) handle(data []byte) error {
	if string(data) == "[DONE]" {
		s.finish()
		return nil
	}

	var chunk wireChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return fmt.Errorf("openaicompat(%s): bad chunk: %w", s.family, err)
	}
	if chunk.Error != nil {
		// Never swallow a mid-stream error: before this branch existed the
		// chunk fell through the no-choices return below and the turn
		// ended silently, indistinguishable from success — so a mid-stream
		// overload never engaged the goal loop's retryable backoff.
		err := fmt.Errorf("openaicompat(%s): stream error: %s (type=%q code=%s)",
			s.family, chunk.Error.Message, chunk.Error.Type, chunk.Error.Code)
		if class, ok := classifyWireCode(chunk.Error.Code); ok {
			return provider.MarkRetryable(err, class)
		}
		return err
	}
	if chunk.ID != "" {
		s.id = chunk.ID
	}
	if chunk.Usage != nil {
		s.usage.InputTokens = chunk.Usage.PromptTokens
		s.usage.OutputTokens = chunk.Usage.CompletionTokens
		s.haveUsage = true
	}
	if len(chunk.Choices) == 0 {
		// A terminal usage chunk carries no choices.
		return nil
	}

	choice := chunk.Choices[0]
	if choice.Delta.Content != "" {
		s.haveText = true
		s.text.WriteString(choice.Delta.Content)
		s.queue = append(s.queue, provider.Event{Type: provider.EventTextDelta, Text: choice.Delta.Content})
	}
	if choice.Delta.ReasoningContent != "" {
		s.haveReasoning = true
		s.reasoningText.WriteString(choice.Delta.ReasoningContent)
		s.queue = append(s.queue, provider.Event{Type: provider.EventReasoningDelta, Text: choice.Delta.ReasoningContent})
	}
	for _, tc := range choice.Delta.ToolCalls {
		if s.toolCalls == nil {
			s.toolCalls = make(map[int]*assembledToolCall)
		}
		cur, ok := s.toolCalls[tc.Index]
		if !ok {
			cur = &assembledToolCall{}
			s.toolCalls[tc.Index] = cur
			s.toolOrder = append(s.toolOrder, tc.Index)
		}
		if tc.ID != "" {
			cur.id = tc.ID
		}
		if tc.Function.Name != "" {
			cur.name = tc.Function.Name
		}
		cur.args.WriteString(tc.Function.Arguments)
	}
	if choice.FinishReason != "" {
		s.stopReason = mapFinishReason(choice.FinishReason)
		s.haveFinish = true
		s.emitToolCalls()
	}
	return nil
}

// emitToolCalls queues an EventToolCall for each accumulated tool call, in
// the order each first appeared, if not already emitted.
func (s *stream) emitToolCalls() {
	if s.emitted {
		return
	}
	s.emitted = true
	for _, idx := range s.toolOrder {
		s.queue = append(s.queue, provider.Event{Type: provider.EventToolCall, ToolCall: s.toolCalls[idx].toolCall()})
	}
}

// finish is called on [DONE]: it emits any tool calls a server that omitted
// finish_reason never surfaced, then queues the terminal EventDone.
func (s *stream) finish() {
	s.emitToolCalls()
	stop := s.stopReason
	if !s.haveFinish {
		if len(s.toolOrder) > 0 {
			stop = provider.StopToolUse
		} else {
			stop = provider.StopEndTurn
		}
	}
	s.queue = append(s.queue, provider.Event{
		Type:       provider.EventDone,
		Message:    s.assemble(),
		StopReason: stop,
		Usage:      s.usage,
	})
	s.done = true
}

// assemble builds the canonical assistant message from accumulated state.
// Ordering mirrors the sibling adapters: reasoning, then text, then tool
// calls.
func (s *stream) assemble() *message.Message {
	msg := &message.Message{
		ID:        s.id,
		Role:      message.RoleAssistant,
		Model:     s.model,
		CreatedAt: time.Now().UTC(),
	}
	if s.haveReasoning {
		msg.Parts = append(msg.Parts, &message.Reasoning{Text: s.reasoningText.String()})
	}
	if s.haveText {
		msg.Parts = append(msg.Parts, &message.Text{Text: s.text.String()})
	}
	for _, idx := range s.toolOrder {
		msg.Parts = append(msg.Parts, s.toolCalls[idx].toolCall())
	}
	return msg
}

func mapFinishReason(reason string) provider.StopReason {
	switch reason {
	case "stop":
		return provider.StopEndTurn
	case "tool_calls":
		return provider.StopToolUse
	case "length":
		return provider.StopMaxTokens
	case "content_filter":
		return provider.StopRefusal
	default:
		return provider.StopOther
	}
}
