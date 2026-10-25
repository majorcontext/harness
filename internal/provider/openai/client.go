// Package openai is the provider adapter for the OpenAI Responses API.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/provider"
)

const defaultBaseURL = "https://api.openai.com"

// defaultResponsesPath is the documented path. An empty ResponsesPath uses it.
const defaultResponsesPath = "/v1/responses"

// ChatGPTCacheTTL is the idle prompt-prefix lifetime that the ChatGPT Codex backend keeps. The backend rejects every retention field, so this is a measured lifetime.
const ChatGPTCacheTTL = 30 * time.Minute

// Client is a provider.Provider for the OpenAI Responses API. The zero value
// plus APIKey is usable; nothing touches the network until Stream.
type Client struct {
	live       atomic.Value
	liveShared *atomic.Value
	// LiteUntilListed makes every model count as Responses Lite until a model list names it.
	LiteUntilListed bool
	// APIKey may be empty when HTTPClient sets the Authorization header.
	APIKey string
	// NoKeyReason says why APIKey is empty. The error of an empty APIKey names it.
	NoKeyReason string
	// NoKey says that an empty APIKey is no error: the entry is keyless or its headers carry the credential.
	NoKey   bool
	BaseURL string // defaults to https://api.openai.com
	// ExtraHeaders are sent verbatim on every request, first, so the fixed
	// Authorization, Content-Type and Accept headers overwrite them.
	ExtraHeaders map[string]string
	// ResponsesPath is appended to BaseURL, defaulting to defaultResponsesPath,
	// for endpoints that serve the Responses wire at another path.
	ResponsesPath string
	// Family overrides the family key reported from Name() and used as the
	// ProviderData tag. Two endpoints on this wire need distinct families so
	// encrypted reasoning items are never replayed across endpoints.
	Family     string
	HTTPClient *http.Client // defaults to http.DefaultClient
	// Log receives the debug lines of the client. nil: slog.Default().
	Log *slog.Logger
	// OmitResponseParams names optional request params that are never sent,
	// for endpoints that reject params the OpenAI API accepts. Wire-only.
	OmitResponseParams []string
	// SanitizeToolSchemas rewrites every tool's JSON Schema parameters through
	// sanitizeToolParameterSchema before sending. Wire-only; default false
	// leaves schemas byte-identical to req.Tools.
	SanitizeToolSchemas bool
	// UseWebSocketTransport routes calls over a pooled wss:// connection
	// instead of HTTP POST + SSE. Any failure falls back to HTTP for that
	// request. The websocket path sends the already-transcoded body.
	UseWebSocketTransport bool
	// HashCacheKey sends a stable hash of the session key as prompt_cache_key, so the raw key never leaves. The websocket pool keeps the raw key.
	HashCacheKey bool
	// SessionHeaders sends the session key, or its cache key hash when HashCacheKey is set, as the session_id and x-client-request-id headers, on which the ChatGPT backend routes the prompt cache.
	SessionHeaders bool
	// CacheTTL is how long the endpoint keeps an idle prompt prefix. 0: unknown.
	CacheTTL       time.Duration
	RequestRetries int // see provider.Do

	wsPoolOnce sync.Once
	wsPoolVal  *wsPool
}

// wsPoolFor lazily builds this client's websocket pool, one per Client.
func (c *Client) wsPoolFor() *wsPool {
	c.wsPoolOnce.Do(func() { c.wsPoolVal = newWSPool() })
	return c.wsPoolVal
}

// familyOrDefault resolves a family override to the key used on the wire and
// in ProviderData. Stream also calls it without a Client.
func familyOrDefault(family string) string {
	if family != "" {
		return family
	}
	return Family
}

// family resolves the provider family key for this client: the configured
// override, or the package constant when unset.
func (c *Client) family() string { return familyOrDefault(c.Family) }

// Name returns the provider family key of this client.
func (c *Client) Name() string { return c.family() }

// ShareLiveCatalog makes this client read catalog facts from source.
func (c *Client) ShareLiveCatalog(source *Client) {
	if source != nil {
		c.liveShared = source.liveCatalogValue()
	}
}

func (c *Client) liveCatalogValue() *atomic.Value {
	if c.liveShared != nil {
		return c.liveShared
	}
	return &c.live
}

// CacheLifetime is how long the endpoint keeps an idle prompt prefix, or 0 when unknown.
func (c *Client) CacheLifetime() time.Duration { return c.CacheTTL }

// NativeWebSearch reports whether model takes the hosted web search of the Responses API.
func (c *Client) NativeWebSearch(model string) bool {
	live, _ := c.liveCatalogValue().Load().(*liveCatalog)
	if live == nil {
		return !c.LiteUntilListed
	}
	lite, listed := live.lite[model]
	return !lite && (listed || !c.LiteUntilListed)
}

// EffortControl returns the effort control of a model id.
func (c *Client) EffortControl(model string) (modelmeta.EffortControl, bool) {
	ctl, ok := modelmeta.OpenAIEfforts(model)
	if live, _ := c.liveCatalogValue().Load().(*liveCatalog); live != nil {
		if l, found := live.efforts[model]; found {
			ctl.Levels, ctl.Field, ok = slices.Clone(l), true, true
		}
	}
	return ctl, ok
}

// StartupPrewarmEnabled reports whether engine startup assembly can produce
// transport state for this client without relying on request data.
func (c *Client) StartupPrewarmEnabled() bool {
	return socketAvailable && c.family() == CodexFamily && c.UseWebSocketTransport
}

// httpClient returns c.HTTPClient, or http.DefaultClient when unset. HTTP and
// the websocket dial share it, so both get the same proxy and TLS behavior.
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
	sizes   wireSizes
}

func (c *Client) prepareRequest(req *provider.Request, allowEmptyInput bool) (*preparedRequest, error) {
	req = c.withCacheKey(req)
	if c.APIKey == "" && c.HTTPClient == nil && !c.NoKey {
		return nil, provider.NoKeyError("openai", c.NoKeyReason)
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
	headers := http.Header{}
	for k, v := range c.ExtraHeaders {
		headers.Set(k, v)
	}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		headers.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.SessionHeaders && req.SessionKey != "" {
		id := req.SessionKey
		if c.HashCacheKey {
			id = req.CacheKey
		}
		headers["session_id"] = []string{id}
		headers.Set("x-client-request-id", id)
	}
	return &preparedRequest{
		body:    body,
		url:     responsesURL(c.BaseURL, c.ResponsesPath),
		headers: headers,
		client:  c.httpClient(),
		sizes:   measureBody(body, wire.MaxOutputTokens),
	}, nil
}

// withCacheKey returns req with the hash of its session key as the cache key when HashCacheKey is set.
func (c *Client) withCacheKey(req *provider.Request) *provider.Request {
	if !c.HashCacheKey || req.SessionKey == "" {
		return req
	}
	sum := sha256.Sum256([]byte(req.SessionKey))
	out := *req
	out.CacheKey = hex.EncodeToString(sum[:])[:32]
	return &out
}

// Stream sends req and returns the response stream, over a websocket when
// configured and over HTTP + SSE otherwise.
func (c *Client) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	prepared, err := c.prepareRequest(req, false)
	if err != nil {
		return nil, err
	}
	body := prepared.body
	url := prepared.url
	headers := prepared.headers
	hc := prepared.client

	// The websocket transport sends the same url, headers and body as the HTTP
	// path, so credential injection applies to both. Any failure falls through
	// to the HTTP POST below. Codex HTTP changes only its wire encoding to zstd.
	if c.UseWebSocketTransport && req.SessionKey != "" && !req.OneShot {
		var permanent error
		if st, ok := c.wsPoolFor().stream(ctx, wsStreamRequest{
			Permanent:  &permanent,
			SessionKey: req.SessionKey,
			URL:        url,
			Headers:    headers,
			Body:       body,
			Sizes:      prepared.sizes,
			Model:      req.Model,
			Family:     c.family(),
			HTTPClient: hc,
			Log:        c.Log,
		}); ok {
			return st, nil
		} else if permanent != nil {
			return nil, provider.MarkPermanent(permanent)
		}
	}

	httpBody := body
	zstdEncoded := c.family() == CodexFamily
	if zstdEncoded {
		httpBody, err = compressCodexHTTPRequest(ctx, body, c.Log)
		if err != nil {
			return nil, err
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(httpBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header = headers.Clone()
	if zstdEncoded {
		httpReq.Header.Set("Content-Encoding", "zstd")
	}

	resp, err := provider.Do(ctx, hc, httpReq, c.RequestRetries)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, apiError(resp)
	}
	return &stream{
		body:     resp.Body,
		r:        bufio.NewReader(resp.Body),
		model:    req.Model,
		family:   c.family(),
		subUsage: c.codexSubscriptionUsage(resp.Header),

		requestMetadata: prepared.sizes.metadata(),
	}, nil
}

// Warm prepares a Codex websocket session without generating assistant
// output. Other families and transports do not have startup state to prepare.
func (c *Client) Warm(ctx context.Context, req *provider.Request) error {
	if c.family() != CodexFamily || !c.UseWebSocketTransport || req.SessionKey == "" {
		return nil
	}
	prepared, err := c.prepareRequest(req, true)
	if err != nil {
		return err
	}
	var permanent error
	return c.wsPoolFor().warm(ctx, wsStreamRequest{
		Permanent:  &permanent,
		SessionKey: req.SessionKey,
		URL:        prepared.url,
		Headers:    prepared.headers,
		Body:       prepared.body,
		Sizes:      prepared.sizes,
		Model:      req.Model,
		Family:     c.family(),
		HTTPClient: prepared.client,
		Log:        c.Log,
		Prewarm:    true,
	}, func(st provider.Stream) error {
		for {
			if _, err := st.Next(); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
		}
	})
}

// codexSubscriptionUsage reads the x-codex-* subscription-usage headers, only
// for a client configured under CodexFamily.
// codexRateLimitsEventType names the Codex websocket telemetry frame
// carrying a rate-limit snapshot. It carries no response content, so it
// never counts toward wsFrameSource.framesRead.
const codexRateLimitsEventType = "codex.rate_limits"

func (c *Client) codexSubscriptionUsage(h http.Header) *message.SubscriptionUsage {
	if c.family() != CodexFamily {
		return nil
	}
	return codexSubscriptionUsageFromHeaders(h)
}

// ResponsesURL is the URL of the Responses request of an entry with the base URL and the path.
func ResponsesURL(base, path string) string { return responsesURL(base, path) }

// responsesURL joins a base URL and a request path with exactly one slash.
// The join is string-level, not url.JoinPath, which re-encodes segments. A
// path without a leading slash would otherwise aim at a different host.
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
			Type     string `json:"type"`
			Code     string `json:"code"`
			Message  string `json:"message"`
			ResetsAt int64  `json:"resets_at"`
		} `json:"error"`
	}
	var err error
	decoded := json.Unmarshal(raw, &body) == nil
	if decoded && (body.Error.Message != "" || body.Error.Type != "" || body.Error.Code != "") {
		kind := body.Error.Type
		switch {
		case kind == "":
			kind = body.Error.Code
		case body.Error.Code != "" && body.Error.Code != kind:
			kind = kind + "/" + body.Error.Code
		}
		msg := fmt.Sprintf("openai: %s (%s, HTTP %d)", body.Error.Message, kind, resp.StatusCode)
		if body.Error.Message == "" {
			msg = fmt.Sprintf("openai: %s (HTTP %d)", kind, resp.StatusCode)
		}
		if pe, ok := contextOverflow(body.Error.Code, msg); ok {
			return pe
		}
		if hint, ok := parseUsageExhaustion(resp.StatusCode, body.Error.Message, body.Error.Type, body.Error.Code); ok {
			return withReset(provider.MarkPermanent(&provider.Error{
				Kind:        provider.ErrKindProviderExhausted,
				Raw:         msg,
				RecoverHint: hint,
			}), body.Error.ResetsAt)
		}
		err = errors.New(msg)
	} else {
		err = fmt.Errorf("openai: HTTP %d", resp.StatusCode)
	}
	if class, ok := classifyStatus(resp.StatusCode); ok {
		return provider.MarkRetryableHTTP(err, class, resp)
	}
	return err
}

func contextOverflow(code, raw string) (*provider.Error, bool) {
	if code != "context_length_exceeded" {
		return nil, false
	}
	return &provider.Error{Kind: provider.ErrKindContextOverflow, Raw: raw}, true
}

// classifyStatus maps an HTTP status to a retryable class: 429 is a rate
// limit, any other 5xx a server error. Every other status is not retryable.
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

// classifyErrorCode maps a mid-stream error event code to the same classes as
// classifyStatus.
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
