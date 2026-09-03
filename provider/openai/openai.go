// Package openai is the provider adapter for the OpenAI Responses API.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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
	// UseWebSocketTransport routes this client's calls over a pooled
	// wss:// connection (see ws.go/ws_pool.go) instead of HTTP POST + SSE
	// — see config.Provider's field of the same name for the full
	// rationale. Any failure along the way falls back to the HTTP path
	// below for that request, so this can only add a transport, never
	// remove the working one. Default false is byte-identical to this
	// client's behavior before the field existed. Note this client
	// already sanitizes tool schemas (SanitizeToolSchemas) and omits
	// params (OmitResponseParams) BEFORE the wire body is built, so the
	// websocket path — which sends that same already-transcoded body —
	// inherits both with no duplicated logic.
	UseWebSocketTransport bool

	wsPoolOnce sync.Once
	wsPoolVal  *wsPool
}

// wsPoolFor lazily builds this client's websocket pool on first use, one
// per Client instance (not global) — a Client is already the unit main.go
// registers one per provider entry, so this scopes pooled connections to
// the same boundary API keys and base URLs are already scoped to.
func (c *Client) wsPoolFor() *wsPool {
	c.wsPoolOnce.Do(func() { c.wsPoolVal = newWSPool() })
	return c.wsPoolVal
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

// StartupPrewarmEnabled reports whether engine startup assembly can produce
// transport state for this client without relying on request data.
func (c *Client) StartupPrewarmEnabled() bool {
	return c.family() == CodexFamily && c.UseWebSocketTransport
}

// httpClient returns the *http.Client this adapter makes every request
// with — c.HTTPClient, or http.DefaultClient when unset. Both the HTTP POST
// path and the websocket dial (see Stream, wsPoolFor) use this exact value,
// which is what makes the two transports' proxy/TLS behavior identical by
// construction rather than by two configurations kept in sync by hand.
func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

type preparedRequest struct {
	body    []byte
	url     string
	headers http.Header
	client  *http.Client
}

func (c *Client) prepareRequest(req *provider.Request, allowEmptyInput bool) (*preparedRequest, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("openai: no API key configured (set OPENAI_API_KEY)")
	}
	wire, err := transcodeRequestFamilyWithOptions(req, c.family(), c.OmitResponseParams, c.SanitizeToolSchemas, transcodeRequestOptions{
		allowEmptyInput: allowEmptyInput,
	})
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	return &preparedRequest{
		body: body,
		url:  responsesURL(c.BaseURL, c.ResponsesPath),
		headers: http.Header{
			"Content-Type":  []string{"application/json"},
			"Accept":        []string{"text/event-stream"},
			"Authorization": []string{"Bearer " + c.APIKey},
		},
		client: c.httpClient(),
	}, nil
}

func (c *Client) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	prepared, err := c.prepareRequest(req, false)
	if err != nil {
		return nil, err
	}
	body := prepared.body
	url := prepared.url
	headers := prepared.headers
	hc := prepared.client

	// The websocket transport sends this SAME url/headers/body — the
	// Authorization header included — so whatever credential injection
	// applies to the HTTP path below (a box's gatekeeper proxy swapping
	// this dummy bearer for a real OAuth one) applies identically to the
	// ws dial, since both go through hc's own Transport (proxy + TLS
	// trust store), and no separate code path could plausibly relay
	// different credentials for the same client. Any failure at all —
	// no SessionKey, a busy or previously-broken session, dial/send/
	// first-frame failure — falls through to the semantically identical HTTP
	// POST below. Codex HTTP changes only its wire encoding to zstd.
	if c.UseWebSocketTransport && req.SessionKey != "" {
		if st, ok := c.wsPoolFor().stream(ctx, wsStreamRequest{
			SessionKey: req.SessionKey,
			URL:        url,
			Headers:    headers,
			Body:       body,
			Model:      req.Model,
			Family:     c.family(),
			HTTPClient: hc,
		}); ok {
			return st, nil
		}
	}

	httpBody := body
	if c.family() == CodexFamily {
		httpBody, err = compressCodexHTTPRequest(ctx, body)
		if err != nil {
			return nil, err
		}
		headers = headers.Clone()
		headers.Set("Content-Encoding", "zstd")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(httpBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header = headers.Clone()

	resp, err := hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, apiError(resp)
	}
	return &stream{
		body:     resp.Body,
		r:        bufio.NewReader(resp.Body),
		model:    req.Model,
		family:   c.family(),
		subUsage: c.codexSubscriptionUsage(resp.Header),
	}, nil
}

// Prewarm prepares a Codex websocket session without generating assistant
// output. Other families and transports do not have startup state to prepare.
func (c *Client) Prewarm(ctx context.Context, req *provider.Request) error {
	if c.family() != CodexFamily || !c.UseWebSocketTransport || req.SessionKey == "" {
		return nil
	}
	prepared, err := c.prepareRequest(req, true)
	if err != nil {
		return err
	}
	st, ok := c.wsPoolFor().stream(ctx, wsStreamRequest{
		SessionKey: req.SessionKey,
		URL:        prepared.url,
		Headers:    prepared.headers,
		Body:       prepared.body,
		Model:      req.Model,
		Family:     c.family(),
		HTTPClient: prepared.client,
		Prewarm:    true,
	})
	if !ok {
		return errors.New("openai: websocket prewarm failed")
	}
	defer st.Close()
	for {
		_, err := st.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// codexSubscriptionUsage reads h for the x-codex-* subscription-usage
// headers (see codexSubscriptionUsageFromHeaders), but only for a client
// configured under CodexFamily — see that constant's own doc comment for
// why family is the gate: an ordinary "openai" entry never even looks at
// these headers, whether or not a proxy in front of it happens to echo
// some of the same header names.
func (c *Client) codexSubscriptionUsage(h http.Header) *message.SubscriptionUsage {
	if c.family() != CodexFamily {
		return nil
	}
	return codexSubscriptionUsageFromHeaders(h)
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
	body io.Closer
	r    *bufio.Reader
	// wsConn is non-nil for a websocket-delivered response (see wsPool.
	// stream) and nil for the HTTP+SSE path — exactly one of {r, wsConn}
	// is set. Next/Close dispatch on it instead of duplicating the SSE
	// decode loop and event mapping for the ws case: stream.handle below
	// is shared, unmodified, between both transports.
	wsConn *wsFrameSource
	model  message.ModelRef
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
	// subUsage is this response's captured subscription-usage snapshot —
	// set only for a CodexFamily client (see Client.codexSubscriptionUsage
	// and wsPool.stream, the two sources), nil otherwise. Carried onto the
	// EventDone event queued in the "response.completed"/"response.
	// incomplete" case below.
	subUsage *message.SubscriptionUsage

	// onComplete publishes transport-local response lineage after stream.handle
	// has assembled a clean response.completed message. It is nil for HTTP.
	onComplete func(responseID string, assistant *message.Message)

	requestMetadata *provider.RequestMetadata
	// recoverChainMiss retries one immediate incremental chain miss as the
	// immutable complete request on the same socket. It is nil for HTTP.
	recoverChainMiss func(first bool, visible bool, chainErr error) (*wsFrameSource, *provider.RequestMetadata, error)
	visibleOutput    bool
	responseFrames   int

	queue []provider.Event
	done  bool
}

// Close releases this stream's transport. For a websocket-delivered
// response, whether the underlying connection is actually torn down or
// kept pooled for the session's next turn was already decided when the
// terminal event was read (see wsFrameSource.close) — Close here just
// tells that decision whether it is being reached cleanly (s.done, the
// same flag Next uses to return io.EOF) or because the caller is
// abandoning the stream early.
func (s *stream) Close() error {
	if s.wsConn != nil {
		return s.wsConn.close(s.done)
	}
	return s.body.Close()
}

// readEvent returns the next (name, data) pair from whichever transport
// this stream is reading — the ws frame source's buffered/live frames, or
// readSSE's HTTP+SSE decode — so handle below never needs to know which.
func (s *stream) readEvent() (string, []byte, error) {
	if s.wsConn != nil {
		return s.wsConn.next()
	}
	return s.readSSE()
}

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
		name, data, err := s.readEvent()
		if err != nil {
			// Reaching this read at all means response.completed has not
			// been seen (s.done, checked above, would have returned the
			// normal end-of-iteration io.EOF) — so any read failure here
			// is the stream dying mid-response: transient, classified
			// retryable.
			return provider.Event{}, provider.MarkStreamTruncated(err)
		}
		s.responseFrames++
		if err := s.handle(name, data); err != nil {
			var miss *previousResponseNotFoundError
			if errors.As(err, &miss) && s.recoverChainMiss != nil {
				source, metadata, recoverErr := s.recoverChainMiss(s.responseFrames == 1, s.visibleOutput, err)
				s.recoverChainMiss = nil
				if recoverErr != nil {
					return provider.Event{}, recoverErr
				}
				s.wsConn = source
				s.requestMetadata = metadata
				s.respID = ""
				s.items = nil
				s.usage = provider.Usage{}
				s.hasToolCall = false
				s.responseFrames = 0
				s.queue = nil
				continue
			}
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

type previousResponseNotFoundError struct {
	message string
}

func (e *previousResponseNotFoundError) Error() string {
	return fmt.Sprintf("openai: %s (previous_response_not_found)", e.message)
}

func streamError(code, message string) error {
	if code == "previous_response_not_found" {
		if message == "" {
			message = "previous response not found"
		}
		return &previousResponseNotFoundError{message: message}
	}
	if code == "" {
		return fmt.Errorf("openai: %s", message)
	}
	err := fmt.Errorf("openai: %s (%s)", message, code)
	if class, ok := classifyErrorCode(code); ok {
		return provider.MarkRetryable(err, class)
	}
	return err
}

func isPreviousResponseNotFoundFrame(name string, data []byte) bool {
	if name != "response.failed" && name != "error" {
		return false
	}
	var ev struct {
		Code     string `json:"code"`
		Response struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"response"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal(data, &ev) == nil &&
		(ev.Code == "previous_response_not_found" || ev.Response.Error.Code == "previous_response_not_found" || ev.Error.Code == "previous_response_not_found")
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
		s.visibleOutput = true
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
		s.visibleOutput = true
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
			s.visibleOutput = true
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
				ID                string `json:"id"`
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
		if ev.Response.ID != "" {
			s.respID = ev.Response.ID
		}
		assistant := s.assemble()
		if name == "response.completed" && s.onComplete != nil {
			s.onComplete(ev.Response.ID, assistant)
		}
		s.queue = append(s.queue, provider.Event{
			Type:              provider.EventDone,
			Message:           assistant,
			StopReason:        stop,
			Usage:             s.usage,
			SubscriptionUsage: s.subUsage,
			RequestMetadata:   s.requestMetadata,
		})
		s.done = true

	case "response.failed", "error":
		var ev struct {
			Code     string `json:"code"`
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
		case ev.Response.Error.Code == "previous_response_not_found":
			return streamError(ev.Response.Error.Code, ev.Response.Error.Message)
		case ev.Error.Code == "previous_response_not_found":
			return streamError(ev.Error.Code, ev.Error.Message)
		case ev.Code == "previous_response_not_found":
			return streamError(ev.Code, ev.Message)
		case ev.Response.Error.Message != "":
			return streamError(ev.Response.Error.Code, ev.Response.Error.Message)
		case ev.Error.Message != "":
			return streamError(ev.Error.Code, ev.Error.Message)
		case ev.Message != "":
			return streamError(ev.Code, ev.Message)
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
