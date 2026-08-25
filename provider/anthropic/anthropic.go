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
	"strings"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
	// extendedCacheTTLBeta is the documented gate for the 1-hour
	// cache_control TTL, sent with — and only with — a CacheTTL1h request.
	// Some endpoints no longer enforce the gate and accept the TTL without
	// this header. The adapter sends it regardless: the documented contract
	// asks for it, and an endpoint that does enforce it must not fail. The
	// 5m TTL sends no beta at all, which is the escape hatch for a gateway
	// that rejects an unknown beta.
	extendedCacheTTLBeta = "extended-cache-ttl-2025-04-11"
)

// Client is a provider.Provider for the Anthropic Messages API. The zero
// value plus APIKey is usable; nothing touches the network until Stream.
type Client struct {
	APIKey     string
	BaseURL    string       // defaults to https://api.anthropic.com
	HTTPClient *http.Client // defaults to http.DefaultClient
	// CacheTTL is the prompt-cache breakpoint lifetime: CacheTTL5m (the
	// API default) or CacheTTL1h (the extended TTL). Empty resolves to
	// DefaultCacheTTL — see that constant for why the default is 1h. Any
	// other value fails Stream, like a missing API key.
	CacheTTL string
}

func (c *Client) Name() string { return Family }

func (c *Client) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("anthropic: no API key configured (set ANTHROPIC_API_KEY)")
	}
	ttl, err := resolveCacheTTL(c.CacheTTL)
	if err != nil {
		return nil, err
	}
	wire, err := transcodeRequest(req, ttl)
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
	if ttl == CacheTTL1h {
		httpReq.Header.Set("Anthropic-Beta", extendedCacheTTLBeta)
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
	var err error
	if json.Unmarshal(raw, &body) == nil && body.Error.Message != "" {
		msg := fmt.Sprintf("anthropic: %s (%s, HTTP %d)", body.Error.Message, body.Error.Type, resp.StatusCode)
		// Context overflow is deterministic (400 invalid_request_error) and
		// disjoint from both the retryable statuses below and the permanent
		// mark just below it — classify it first so it never reaches either.
		if promptTokens, limit, ok := parseContextOverflow(body.Error.Type, resp.StatusCode, body.Error.Message); ok {
			return &provider.Error{
				Kind:         provider.ErrKindContextOverflow,
				Raw:          msg,
				PromptTokens: promptTokens,
				TokenLimit:   limit,
			}
		}
		// Every OTHER 400 invalid_request_error is a permanent, deterministic
		// failure (NEP-5272): a malformed request — most notably a message
		// array with an orphaned tool_use, the incident that motivated this —
		// fails identically no matter how many times it is retried, unlike
		// the transient weather classifyStatus recognizes below. Checked
		// only once context overflow is ruled out (the two are disjoint:
		// overflow always returns above), and only for this one status/type
		// combination — classifyStatus already reports 400 as not
		// retryable, so this mark and that one can never collide on the
		// same error.
		// An ACCOUNT-level supply wall (usage limit, quota, credit
		// balance, spend cap) is classified before both the generic
		// permanent branch below and classifyStatus's retryable weather:
		// Anthropic delivers it as an ordinary 400 invalid_request_error
		// or 429 rate_limit_error, so without this check the same wall
		// reads as "malformed request" on one status and "retry in a
		// moment" on the other — neither of which tells a supervising
		// parent that every sibling on this key is walled too. Wrapped
		// permanent for the same reason the branch below is: no backoff
		// schedule outlives a spent quota, so retrying only burns turns.
		if hint, ok := parseUsageExhaustion(resp.StatusCode, body.Error.Message); ok {
			return provider.MarkPermanent(&provider.Error{
				Kind:        provider.ErrKindProviderExhausted,
				Raw:         msg,
				RecoverHint: hint,
			})
		}
		if resp.StatusCode == http.StatusBadRequest && body.Error.Type == "invalid_request_error" {
			return provider.MarkPermanent(errors.New(msg))
		}
		err = errors.New(msg)
	} else {
		err = fmt.Errorf("anthropic: HTTP %d", resp.StatusCode)
	}
	if class, ok := classifyStatus(resp.StatusCode); ok {
		return provider.MarkRetryable(err, class)
	}
	return err
}

// classifyStatus classifies an HTTP response status into a
// provider.RetryableClass (see GitHub issue #61): 529 is Anthropic's
// dedicated "overloaded" status, 429 is a rate limit, and any other 5xx is
// a generic server error — all transient provider weather worth the goal
// loop's long backoff (engine/goal.go). Every other status (400s, auth)
// reports ok=false and stays a deterministic, fail-fast error exactly as
// before.
func classifyStatus(status int) (provider.RetryableClass, bool) {
	switch {
	case status == 529:
		return provider.RetryableOverloaded, true
	case status == http.StatusTooManyRequests:
		return provider.RetryableRateLimited, true
	case status >= 500 && status <= 599:
		return provider.RetryableServerError, true
	default:
		return "", false
	}
}

// classifyErrorType classifies the Anthropic wire error "type" carried by a
// mid-stream "error" SSE event (see the "error" case in stream.handle
// below) — the same three retryable categories as classifyStatus, keyed on
// the wire vocabulary instead of an HTTP status because a stream error
// carries no status code of its own.
func classifyErrorType(errType string) (provider.RetryableClass, bool) {
	switch errType {
	case "overloaded_error":
		return provider.RetryableOverloaded, true
	case "rate_limit_error":
		return provider.RetryableRateLimited, true
	case "api_error":
		return provider.RetryableServerError, true
	default:
		return "", false
	}
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

// usageExhaustionPatterns matches the message shapes Anthropic uses for an
// ACCOUNT-level supply wall. Like contextOverflowPattern above, Anthropic
// gives these no distinct error type or code — a spent usage limit arrives
// as a plain invalid_request_error and a spent quota as a plain
// rate_limit_error — so this is the second place message matching is
// tolerated, under the identical rules: scoped to this adapter, never the
// engine, and gated on a structural signal (see parseUsageExhaustion)
// before the message is ever inspected.
//
// The list is meant to GROW. Each entry is one observed wall shape, kept
// separate and narrow rather than folded into one clever alternation, so
// adding a newly observed wording is a one-line change with an obvious
// test row (TestUsageLimitShapes). Every entry must name a SUPPLY that is
// spent — a limit, quota, balance, or spend cap — never a per-minute or
// per-token THROTTLE, which is ordinary weather classifyStatus already
// handles correctly (TestPlainRateLimitStaysRetryable guards that line).
var usageExhaustionPatterns = []*regexp.Regexp{
	// "You have reached your specified API usage limits." — the live
	// 2026-08-25 incident's own message, an HTTP 400.
	regexp.MustCompile(`(?i)reached your specified API usage limits?`),
	// "Your credit balance is too low to access the Anthropic API"
	regexp.MustCompile(`(?i)credit balance is too low`),
	// "You have exceeded your monthly quota" / "Organization quota exceeded"
	regexp.MustCompile(`(?i)quota (?:has been )?exceeded`),
	regexp.MustCompile(`(?i)exceeded your (?:\w+ )?quota`),
	// "...would exceed your organization's monthly spend limit"
	regexp.MustCompile(`(?i)spend limit`),
	// "usage limit reached" / "usage limit exceeded" phrasings
	regexp.MustCompile(`(?i)usage limit (?:reached|exceeded)`),
}

// recoverHintPattern extracts the provider's own statement of when access
// returns, e.g. "You will regain access on 2026-09-01 at 00:00 UTC." The
// hint is optional detail: parseUsageExhaustion classifies with or without
// it (see provider.Error.RecoverHint).
var recoverHintPattern = regexp.MustCompile(`(?i)regain access on ([^.\n"]+)`)

// exhaustionStatuses are the HTTP statuses an account-level wall can
// arrive on: 400 (usage limit, credit balance), 402 (payment required),
// 403 (some organization-level refusals), 429 (quota). The status gate
// runs BEFORE any message match, so a 500 that happens to quote a quota
// message stays server weather.
var exhaustionStatuses = map[int]bool{
	http.StatusBadRequest:      true,
	http.StatusPaymentRequired: true,
	http.StatusForbidden:       true,
	http.StatusTooManyRequests: true,
}

// parseUsageExhaustion classifies an Anthropic error message as an
// account-level supply wall, returning the recover-at hint when the
// message carries one (empty string otherwise — never a reason to refuse
// the classification). status <= 0 skips the status gate: a mid-stream
// "error" SSE event carries no status of its own, exactly as
// classifyErrorType has to work from the wire type alone.
func parseUsageExhaustion(status int, message string) (recoverHint string, ok bool) {
	if status > 0 && !exhaustionStatuses[status] {
		return "", false
	}
	matched := false
	for _, pat := range usageExhaustionPatterns {
		if pat.MatchString(message) {
			matched = true
			break
		}
	}
	if !matched {
		return "", false
	}
	if m := recoverHintPattern.FindStringSubmatch(message); m != nil {
		return strings.TrimSpace(m[1]), true
	}
	return "", true
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
			// Reaching this read at all means message_stop has not been
			// seen (s.done, checked above, would have returned the normal
			// end-of-iteration io.EOF) — so any read failure here is the
			// stream dying mid-response: transient, classified retryable.
			return provider.Event{}, provider.MarkStreamTruncated(err)
		}
		if err := s.handle(name, data); err != nil {
			return provider.Event{}, err
		}
		if len(s.queue) == 0 && !s.done {
			// The wire event was handled but queued nothing consumer-
			// visible (ping, input_json_delta, message_start, ...).
			// Surface it as activity instead of looping into another
			// blocking read, so a consumer timing Next returns (the
			// engine's idle-stream watchdog) sees the wire is alive — a
			// large tool-argument block otherwise streams for minutes
			// with zero events. See provider.EventActivity.
			return provider.Event{Type: provider.EventActivity}, nil
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
			// An event whose blank-line terminator never arrived is
			// DISCARDED, per the SSE spec — never handed up as if
			// complete. The cut can land anywhere (TCP fragmentation makes
			// the boundary a coin flip), and parsing a mid-JSON fragment
			// here used to surface a raw, deterministic-looking decode
			// error that dodged Next's truncation classification.
			return "", nil, err
		}
		line = trimEOL(line)
		switch {
		case line == "":
			if name != "" || buf.Len() > 0 {
				return name, buf.Bytes(), nil
			}
		case line[0] == ':':
			// A comment line is a keepalive heartbeat (bifrost sends
			// ": heartbeat" every second on idle streams —
			// maximhq/bifrost#5010). It carries no event, but it IS wire
			// activity: hand it up between events so Next surfaces
			// EventActivity and the engine's idle watchdog sees the
			// stream is alive. A comment INSIDE a partially-read event
			// (name or data already buffered) stays skipped — the event's
			// own arrival is the activity signal there.
			if name == "" && buf.Len() == 0 {
				return "", nil, nil
			}
		case len(line) > 6 && line[:6] == "event:":
			name = trimSpaceLeft(line[6:])
		case len(line) > 5 && line[:5] == "data:":
			buf.WriteString(trimSpaceLeft(line[5:]))
		}
		// Unknown fields are ignored per the SSE spec.
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
				InputTokens              int `json:"input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				OutputTokens             int `json:"output_tokens"`
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
		// Input/cache counts normally arrive in message_start and are zero
		// here (real Anthropic omits them from message_delta) — but a
		// Bedrock-translating gateway (bifrost's /anthropic route, captured
		// live 2026-08-06) emits message_start with ALL usage fields zero
		// and delivers the real input/cache counts in message_delta
		// instead, the Bedrock convention of usage-in-final-metadata.
		// Nonzero values here win; zeros never clobber message_start's.
		// Without this, every turn on such a route reported input_tokens=0,
		// silently disarming auto-compaction (its threshold sums exactly
		// these components).
		if ev.Usage.InputTokens > 0 {
			s.usage.InputTokens = ev.Usage.InputTokens
		}
		if ev.Usage.CacheCreationInputTokens > 0 {
			s.usage.CacheWriteTokens = ev.Usage.CacheCreationInputTokens
		}
		if ev.Usage.CacheReadInputTokens > 0 {
			s.usage.CacheReadTokens = ev.Usage.CacheReadInputTokens
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
		err := fmt.Errorf("anthropic: %s (%s)", ev.Error.Message, ev.Error.Type)
		// Mirror apiError's HTTP-path classification (NEP-5272): an
		// invalid_request_error naming a structurally malformed request —
		// most notably an orphaned tool_use — fails identically on every
		// retry, whether it arrives as an HTTP 400 before the stream ever
		// starts or, as here, as a mid-stream "error" SSE event. Without
		// this, a mid-stream invalid_request_error burned the full
		// deterministic retry budget instead of failing fast on attempt 1.
		// No context-overflow carve-out is needed here: an oversized
		// request is rejected before any stream opens, never mid-stream.
		//
		// An account-level supply wall CAN arrive here, though: a long
		// stream can outlive the moment the key's limit is reached.
		// Classified first, on the message alone — a stream event carries
		// no HTTP status, the same constraint classifyErrorType works
		// under — so a mid-stream wall reads identically to an HTTP one.
		if hint, ok := parseUsageExhaustion(0, ev.Error.Message); ok {
			return provider.MarkPermanent(&provider.Error{
				Kind:        provider.ErrKindProviderExhausted,
				Raw:         err.Error(),
				RecoverHint: hint,
			})
		}
		if ev.Error.Type == "invalid_request_error" {
			return provider.MarkPermanent(err)
		}
		if class, ok := classifyErrorType(ev.Error.Type); ok {
			return provider.MarkRetryable(err, class)
		}
		return err

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
