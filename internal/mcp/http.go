package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type requestHeadersKey struct{}

// WithRequestHeader adds an HTTP header to requests made with ctx.
func WithRequestHeader(ctx context.Context, name, value string) context.Context {
	headers, _ := ctx.Value(requestHeadersKey{}).(map[string]string)
	copy := make(map[string]string, len(headers)+1)
	for k, v := range headers {
		copy[k] = v
	}
	copy[name] = value
	return context.WithValue(ctx, requestHeadersKey{}, copy)
}

// RequestHeader returns an HTTP header carried by ctx.
func RequestHeader(ctx context.Context, name string) string {
	return requestHeaders(ctx)[name]
}

func requestHeaders(ctx context.Context) map[string]string {
	headers, _ := ctx.Value(requestHeadersKey{}).(map[string]string)
	return headers
}

// sessionIDHeader and protocolVersionHeader are the Streamable HTTP
// transport's well-known headers (https://modelcontextprotocol.org/specification/2025-11-25/basic/transports#session-management,
// #protocol-version-header). net/http canonicalizes header names, so any
// casing works; these are written in their canonical form.
const (
	// HarnessSessionIDHeader carries the calling Harness session ID when a server opts in.
	HarnessSessionIDHeader = "Harness-Session-Id"
	sessionIDHeader        = "Mcp-Session-Id"
	protocolVersionHeader  = "Mcp-Protocol-Version"
)

// HTTPTransport configures a client that speaks the Streamable HTTP
// transport: every JSON-RPC message is POSTed to a single MCP endpoint,
// which responds with either a single JSON object or a `text/event-stream`
// carrying zero or more server-initiated messages followed by the
// response.
type HTTPTransport struct {
	// Endpoint is the MCP endpoint URL (supports both POST and, in a full
	// implementation, GET — this client does not open an independent GET
	// listening stream).
	Endpoint string
	// Headers are static headers sent on every request, e.g.
	// {"Authorization": "Bearer <token>"} for a pre-obtained OAuth/PAT
	// token. This client does not implement the OAuth 2.1 authorization
	// flow itself.
	Headers map[string]string
	// Client is the underlying HTTP client. http.DefaultClient is used if
	// nil. Per-request timeouts are enforced via context, not by the
	// http.Client itself, so callers generally don't need a Timeout here.
	Client *http.Client
	// MaxResponseBytes bounds the body of each response that the client decodes. Zero or less is no bound. A larger
	// body fails the request with ErrResponseTooLarge. WithMaxResponse overrides it for one request.
	MaxResponseBytes int64
}

type maxResponseKey struct{}

// WithMaxResponse returns a context under which an HTTP request reads at most n response bytes, whatever
// HTTPTransport.MaxResponseBytes says.
func WithMaxResponse(ctx context.Context, n int64) context.Context {
	return context.WithValue(ctx, maxResponseKey{}, n)
}

// ErrResponseTooLarge is the error of a response whose body passed its bound.
var ErrResponseTooLarge = errors.New("mcp: response too large")

// boundedBody reads the body of a response and fails with ErrResponseTooLarge after max bytes.
type boundedBody struct {
	r    io.Reader
	left int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.left < 0 {
		return 0, ErrResponseTooLarge
	}
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return max(0, n+int(b.left)), ErrResponseTooLarge
	}
	return n, err
}

// body returns the body of resp under the bound of ctx or of the transport.
func (t *httpTransport) body(ctx context.Context, resp *http.Response) io.Reader {
	limit, ok := ctx.Value(maxResponseKey{}).(int64)
	if !ok {
		limit = t.maxBody
	}
	if limit <= 0 {
		return resp.Body
	}
	return &boundedBody{r: resp.Body, left: limit}
}

func (t *HTTPTransport) open(handler handlerFunc) (transport, error) {
	if t.Endpoint == "" {
		return nil, fmt.Errorf("mcp: http transport: no endpoint configured")
	}
	client := originBound(t.Client)
	return &httpTransport{
		endpoint: t.Endpoint,
		headers:  t.Headers,
		client:   client,
		handler:  handler,
		maxBody:  t.MaxResponseBytes,
	}, nil
}

// ErrRedirectRefused is the error of a request that the server answered with a redirect to another origin.
var ErrRedirectRefused = errors.New("redirect to another origin refused")

// originBound returns a shallow copy of c, or of the default client when c is nil, that refuses a redirect to
// another origin. A client with its own CheckRedirect keeps it.
func originBound(c *http.Client) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	if c.CheckRedirect != nil {
		return c
	}
	bound := *c
	bound.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if o := via[0].URL; req.URL.Scheme != o.Scheme || req.URL.Host != o.Host {
			return ErrRedirectRefused
		}
		return nil
	}
	return &bound
}

// doRequest sends req and strips the URL, which may hold a credential, from a transport error.
func (t *httpTransport) doRequest(req *http.Request) (*http.Response, error) {
	resp, err := t.client.Do(req)
	var ue *url.Error
	if err != nil && errors.As(err, &ue) {
		err = ue.Err
	}
	return resp, err
}

type httpTransport struct {
	endpoint string
	headers  map[string]string
	client   *http.Client
	handler  handlerFunc
	maxBody  int64

	nextID atomic.Int64

	mu              sync.Mutex
	sessionID       string
	protocolVersion string // set after a successful initialize; sent on all subsequent requests
}

func (t *httpTransport) setProtocolVersion(v string) {
	t.mu.Lock()
	t.protocolVersion = v
	t.mu.Unlock()
}

func (t *httpTransport) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	for k, v := range requestHeaders(ctx) {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	sessionID := t.sessionID
	protocolVersion := t.protocolVersion
	t.mu.Unlock()
	if sessionID != "" {
		req.Header.Set(sessionIDHeader, sessionID)
	}
	if protocolVersion != "" {
		req.Header.Set(protocolVersionHeader, protocolVersion)
	}
	return req, nil
}

func (t *httpTransport) recordSession(resp *http.Response) {
	if id := resp.Header.Get(sessionIDHeader); id != "" {
		t.mu.Lock()
		t.sessionID = id
		t.mu.Unlock()
	}
}

func (t *httpTransport) call(ctx context.Context, method string, params, result any) error {
	id := t.nextID.Add(1)
	t.mu.Lock()
	sessionID := t.sessionID
	t.mu.Unlock()
	annotateCall(ctx, method, id, sessionID)
	idJSON, err := json.Marshal(id)
	if err != nil {
		return err
	}
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	body, err := json.Marshal(message{JSONRPC: "2.0", ID: idJSON, Method: method, Params: raw})
	if err != nil {
		return err
	}

	req, err := t.newRequest(ctx, body)
	if err != nil {
		return err
	}
	resp, err := t.doRequest(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("mcp: http request: %w", err)
	}
	defer resp.Body.Close()
	t.recordSession(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpStatusError(resp)
	}

	// RFC 9110 §8.3.1: media types (and their parameters' names) are
	// case-insensitive, so a server sending "Application/JSON" or
	// "Text/Event-Stream" is conformant and must be treated the same as
	// the canonical lowercase form.
	contentType := strings.ToLower(stripParams(resp.Header.Get("Content-Type")))
	switch contentType {
	case "application/json":
		respBody, err := io.ReadAll(t.body(ctx, resp))
		if err != nil {
			return fmt.Errorf("mcp: reading response: %w", err)
		}
		var msg message
		if err := json.Unmarshal(respBody, &msg); err != nil {
			return fmt.Errorf("mcp: malformed response: %w", err)
		}
		return decodeResult(msg, idToken(idJSON), result)

	case "text/event-stream":
		var found *message
		err := readSSE(t.body(ctx, resp), func(ev sseEvent) (bool, error) {
			if len(ev.data) == 0 {
				return false, nil // priming event
			}
			var msg message
			if err := json.Unmarshal(ev.data, &msg); err != nil {
				return false, fmt.Errorf("mcp: malformed SSE event: %w", err)
			}
			if msg.isResponse() {
				if idToken(msg.ID) == idToken(idJSON) {
					m := msg
					found = &m
					return true, nil
				}
				return false, nil // response to some other in-flight request; not ours
			}
			// A server request arriving inline with our response stream is
			// answered with a POST of its response.
			result, herr := t.handler(ctx, msg.Method, msg.Params)
			if len(msg.ID) != 0 {
				if err := t.postResponse(ctx, replyTo(msg, result, herr)); err != nil {
					log.Printf("mcp: answer to server request %q was not delivered: %v", msg.Method, err)
				}
			}
			return false, nil
		})
		if err != nil {
			return err
		}
		if found == nil {
			return fmt.Errorf("mcp: SSE stream ended without a response to request %s", method)
		}
		return decodeResult(*found, idToken(idJSON), result)

	default:
		return fmt.Errorf("mcp: unexpected response Content-Type %q", resp.Header.Get("Content-Type"))
	}
}

func (t *httpTransport) postResponse(ctx context.Context, resp message) error {
	resp.JSONRPC = "2.0"
	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	req, err := t.newRequest(ctx, body)
	if err != nil {
		return err
	}
	hr, err := t.doRequest(req)
	if err != nil {
		return fmt.Errorf("mcp: http response post: %w", err)
	}
	defer func() { _ = hr.Body.Close() }()
	if hr.StatusCode < 200 || hr.StatusCode >= 300 {
		return httpStatusError(hr)
	}
	return nil
}

func decodeResult(msg message, wantID string, result any) error {
	if idToken(msg.ID) != wantID {
		return fmt.Errorf("mcp: response id %s does not match request id %s", idToken(msg.ID), wantID)
	}
	if msg.Error != nil {
		return msg.Error
	}
	if result != nil && len(msg.Result) > 0 {
		return json.Unmarshal(msg.Result, result)
	}
	return nil
}

func (t *httpTransport) notify(ctx context.Context, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	body, err := json.Marshal(message{JSONRPC: "2.0", Method: method, Params: raw})
	if err != nil {
		return err
	}
	req, err := t.newRequest(ctx, body)
	if err != nil {
		return err
	}
	resp, err := t.doRequest(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("mcp: http notify: %w", err)
	}
	defer resp.Body.Close()
	t.recordSession(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpStatusError(resp)
	}
	return nil
}

// closeSessionDeleteTimeout bounds the best-effort session termination
// DELETE issued by close. It is deliberately independent of the caller's
// context (close's signature takes none) and of t.client's Timeout (per
// HTTPTransport.Client's doc, callers generally don't set one): without
// its own deadline, a server that accepts the DELETE but never responds
// would wedge close, and therefore Client.Close, forever.
const closeSessionDeleteTimeout = 2 * time.Second

func (t *httpTransport) close() error {
	// The transport itself holds no persistent connection to release
	// (each call is an independent HTTP request); per spec, session
	// termination is a courtesy DELETE, not a requirement. It is
	// best-effort in both directions: the server may ignore or reject it
	// (see the nilerr suppressions below), and this side bounds its own
	// wait rather than trust the server to answer promptly.
	t.mu.Lock()
	sessionID := t.sessionID
	t.mu.Unlock()
	if sessionID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeSessionDeleteTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.endpoint, nil)
	if err != nil {
		return nil //nolint:nilerr // best-effort
	}
	req.Header.Set(sessionIDHeader, sessionID)
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	resp, err := t.doRequest(req)
	if err != nil {
		return nil //nolint:nilerr // best-effort; server may not support DELETE, or may never respond (bounded by closeSessionDeleteTimeout above)
	}
	defer resp.Body.Close()
	return nil
}

// HTTPStatusError is a non-2xx answer of an HTTP server. It holds the status and never the body.
type HTTPStatusError struct{ Status int }

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("mcp: http %d", e.Status) }

// httpStatusError returns the RPC error of a body that holds one, except for a 401 or 403, which is always an
// HTTPStatusError.
func httpStatusError(resp *http.Response) error {
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &HTTPStatusError{Status: resp.StatusCode}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var msg message
	if json.Unmarshal(body, &msg) == nil && msg.Error != nil {
		return msg.Error
	}
	return &HTTPStatusError{Status: resp.StatusCode}
}

// stripParams removes any ";charset=..." style parameters from a
// Content-Type header value for comparison.
func stripParams(contentType string) string {
	if i := indexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return contentType
}
