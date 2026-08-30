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
	"strings"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

const defaultBaseURL = "https://api.openai.com"

// defaultResponsesPath is the request path the OpenAI Responses API
// documents, and the only path this adapter could reach before
// Client.ResponsesPath existed. An empty ResponsesPath resolves to it, so
// every pre-existing caller's wire is unchanged.
const defaultResponsesPath = "/v1/responses"

// Client is a provider.Provider for the OpenAI Responses API. The zero value
// plus APIKey is usable; nothing touches the network until Stream.
type Client struct {
	APIKey  string
	BaseURL string // defaults to https://api.openai.com
	// ResponsesPath is the request path appended to BaseURL, defaulting to
	// defaultResponsesPath. It is configurable because the Responses wire
	// format is spoken by endpoints that do not serve it at OpenAI's own
	// path: a vendor may expose an equivalent endpoint under a path of its
	// own, which "<base>/v1/responses" cannot reach no matter how BaseURL
	// is written.
	ResponsesPath string
	// Family overrides the provider family key this client reports from
	// Name() and uses as its ProviderData tag. Empty (the default) means
	// the package Family constant, so every existing caller is unchanged.
	//
	// It is configurable for the same reason provider/openaicompat's is:
	// more than one distinct endpoint can speak this wire, and two of them
	// may be configured at once under different providers-map keys. The tag
	// matters beyond routing. Reasoning items on this API are opaque,
	// typically ENCRYPTED, endpoint-scoped state replayed verbatim on every
	// later request. Tagging both clients "openai" would make the canonical
	// format's family match succeed across two endpoints that do not share
	// a key, so a session that switched between them would replay one
	// endpoint's ciphertext to the other. Per-client families make that a
	// cross-family drop instead — the canonical crossing rule, which costs
	// a turn of reasoning continuity and nothing else.
	Family     string
	HTTPClient *http.Client // defaults to http.DefaultClient
	// OmitResponseParams names optional Responses request params this
	// client must NOT send on the wire, e.g. "max_output_tokens",
	// "temperature", "top_p", "metadata" — see config.Provider's field of
	// the same name for the full rationale (some Responses-API-compatible
	// endpoints, e.g. the ChatGPT Codex backend, reject params the OpenAI
	// API itself accepts). This is wire-only: it never affects the
	// canonical provider.Request this client is handed, only the JSON body
	// transcodeRequestFamily builds from it.
	OmitResponseParams []string
	// SanitizeToolSchemas rewrites every tool's JSON Schema parameters
	// through sanitizeToolParameterSchema before this client sends a
	// request — see config.Provider's field of the same name for the full
	// rationale (the ChatGPT Codex backend's tool-schema validator rejects
	// keywords the OpenAI platform API accepts, e.g. a regex `pattern`
	// using lookaround). This is wire-only: it never affects the canonical
	// provider.Request this client is handed, only the JSON body
	// transcodeRequestFamily builds from it. Default false leaves every
	// tool schema byte-identical to req.Tools.
	SanitizeToolSchemas bool
}

// familyOrDefault resolves a configured family override to the family key
// actually used on the wire and in ProviderData: an empty override means
// the package Family constant. It is the single place that default lives,
// shared by Client (which configures it) and stream (which is also built
// directly, without a Client, by the fuzz harness).
func familyOrDefault(family string) string {
	if family != "" {
		return family
	}
	return Family
}

// family resolves the provider family key for this client: the configured
// override, or the package constant when unset.
func (c *Client) family() string { return familyOrDefault(c.Family) }

func (c *Client) Name() string { return c.family() }

func (c *Client) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("openai: no API key configured (set OPENAI_API_KEY)")
	}
	wire, err := transcodeRequestFamily(req, c.family(), c.OmitResponseParams, c.SanitizeToolSchemas)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesURL(c.BaseURL, c.ResponsesPath), bytes.NewReader(body))
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
		body:   resp.Body,
		r:      bufio.NewReader(resp.Body),
		model:  req.Model,
		family: c.family(),
	}, nil
}

// responsesURL joins a base URL and a request path, applying each field's
// default and normalizing the separator between them to exactly one slash.
//
// Both halves are caller-supplied configuration now, so both of the obvious
// typos have to be absorbed. A path missing its leading slash is the
// dangerous one: "https://host" + "backend/responses" is not a wrong path,
// it is a request aimed at the host "hostbackend" — a different server
// entirely, and one an attacker could conceivably register. A trailing
// slash on the base is the harmless mirror image, normalized here for the
// same reason.
//
// The join is deliberately string-level rather than url.JoinPath: JoinPath
// re-encodes path segments, and this adapter must keep the default path
// byte-identical to the string it has always sent. Trimming every leading
// slash before re-adding one also rules out a "//..." path, which a URL
// parser reads as the start of an authority rather than as a path.
func responsesURL(base, path string) string {
	if base == "" {
		base = defaultBaseURL
	}
	if path == "" {
		path = defaultResponsesPath
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
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
		err = fmt.Errorf("openai: %s (%s, HTTP %d)", body.Error.Message, body.Error.Type, resp.StatusCode)
	} else {
		err = fmt.Errorf("openai: HTTP %d", resp.StatusCode)
	}
	if class, ok := classifyStatus(resp.StatusCode); ok {
		return provider.MarkRetryable(err, class)
	}
	return err
}

// classifyStatus classifies an HTTP response status into a
// provider.RetryableClass (see GitHub issue #79, mirroring provider/
// anthropic's classifyStatus for issue #61): 429 is a rate limit, any other
// 5xx is a generic server error — both transient provider weather worth the
// goal loop's long backoff (engine/goal.go). Every other status (400s,
// auth) reports ok=false and stays a deterministic, fail-fast error exactly
// as before. Unlike Anthropic, the Responses API has no dedicated
// "overloaded" status distinct from a plain 5xx, so there is no analog of
// RetryableOverloaded here.
func classifyStatus(status int) (provider.RetryableClass, bool) {
	switch {
	case status == http.StatusTooManyRequests:
		return provider.RetryableRateLimited, true
	case status >= 500 && status <= 599:
		return provider.RetryableServerError, true
	default:
		return "", false
	}
}

// classifyErrorCode classifies the Responses API's mid-stream "response.
// failed"/"error" event error "code" (see the corresponding case in
// stream.handle below) — the same two retryable categories as
// classifyStatus, keyed on the wire code vocabulary instead of an HTTP
// status because a stream error carries no status code of its own. Mirrors
// provider/anthropic's classifyErrorType.
func classifyErrorCode(code string) (provider.RetryableClass, bool) {
	switch code {
	case "rate_limit_exceeded":
		return provider.RetryableRateLimited, true
	case "server_error":
		return provider.RetryableServerError, true
	default:
		return "", false
	}
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
	// family is the client's resolved provider family: the ProviderData tag
	// this stream writes reasoning attachments under. Empty means the
	// package Family constant (see familyOrDefault), which keeps a stream
	// built directly in a test — the fuzz harness does exactly that —
	// behaving as it always has.
	family string

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
			// Reaching this read at all means response.completed has not
			// been seen (s.done, checked above, would have returned the
			// normal end-of-iteration io.EOF) — so any read failure here
			// is the stream dying mid-response: transient, classified
			// retryable.
			return provider.Event{}, provider.MarkStreamTruncated(err)
		}
		if err := s.handle(name, data); err != nil {
			return provider.Event{}, err
		}
		if len(s.queue) == 0 && !s.done {
			// Handled but queued nothing consumer-visible: surface as
			// activity so idle-watchdog consumers see the wire is alive —
			// see provider.EventActivity and provider/anthropic's Next.
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
			// DISCARDED, per the SSE spec — see provider/anthropic's
			// readSSE for why handing the fragment up dodged truncation
			// classification.
			return "", nil, err
		}
		line = trimEOL(line)
		switch {
		case line == "":
			if name != "" || buf.Len() > 0 {
				return name, buf.Bytes(), nil
			}
		case line[0] == ':':
			// Keepalive heartbeat comment — see provider/anthropic's
			// readSSE: surface it between events so Next emits
			// EventActivity and idle watchdogs see the wire is alive.
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

// maxOutputIndex bounds the output_index values itemAt accepts: the slice
// grows to the index named on the wire, so an unbounded value is an
// attacker/corruption-controlled allocation. FuzzStreamDecode found
// output_index 177777777 forcing a ~1.4GB slice (the nightly fuzz worker
// died of resource exhaustion), and a negative index panicked. Real
// responses carry at most a few dozen output items; 10000 is generous
// beyond any legitimate stream.
const maxOutputIndex = 10000

// itemAt returns the assembled item at output_index idx, growing the slice
// as needed, or an error for an index no legitimate stream produces.
func (s *stream) itemAt(idx int) (*assembledItem, error) {
	if idx < 0 || idx > maxOutputIndex {
		return nil, fmt.Errorf("openai: output_index %d out of range [0, %d]", idx, maxOutputIndex)
	}
	for len(s.items) <= idx {
		s.items = append(s.items, nil)
	}
	if s.items[idx] == nil {
		s.items[idx] = &assembledItem{}
	}
	return s.items[idx], nil
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
		it, err := s.itemAt(ev.OutputIndex)
		if err != nil {
			return err
		}
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
		it, err := s.itemAt(ev.OutputIndex)
		if err != nil {
			return err
		}
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
		it, err := s.itemAt(ev.OutputIndex)
		if err != nil {
			return err
		}
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
			err := fmt.Errorf("openai: %s (%s)", ev.Response.Error.Message, ev.Response.Error.Code)
			if class, ok := classifyErrorCode(ev.Response.Error.Code); ok {
				return provider.MarkRetryable(err, class)
			}
			return err
		case ev.Error.Message != "":
			err := fmt.Errorf("openai: %s (%s)", ev.Error.Message, ev.Error.Code)
			if class, ok := classifyErrorCode(ev.Error.Code); ok {
				return provider.MarkRetryable(err, class)
			}
			return err
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
				ProviderData: message.ProviderData{familyOrDefault(s.family): it.raw},
			})
		case "function_call":
			msg.Parts = append(msg.Parts, it.toolCall())
		}
	}
	return msg
}
