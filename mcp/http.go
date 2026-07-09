package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// sessionIDHeader and protocolVersionHeader are the Streamable HTTP
// transport's well-known headers
// (https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#session-management,
// #protocol-version-header). net/http canonicalizes header names, so any
// casing works; these are written in their canonical form.
const (
	sessionIDHeader       = "Mcp-Session-Id"
	protocolVersionHeader = "Mcp-Protocol-Version"
)

// HTTPTransport configures a client that speaks the Streamable HTTP
// transport: every JSON-RPC message is POSTed to a single MCP endpoint,
// which responds with either a single JSON object or a `text/event-stream`
// carrying zero or more server-initiated messages followed by the
// response.
type HTTPTransport struct {
	// Endpoint is the MCP endpoint URL (supports both POST and, in a full
	// implementation, GET — this client does not open an independent GET
	// listening stream; see the package doc's deferred-features list).
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
}

func (t *HTTPTransport) open(onNotify notificationHandler) (transport, error) {
	if t.Endpoint == "" {
		return nil, fmt.Errorf("mcp: http transport: no endpoint configured")
	}
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	return &httpTransport{
		endpoint: t.Endpoint,
		headers:  t.Headers,
		client:   client,
		onNotify: onNotify,
	}, nil
}

type httpTransport struct {
	endpoint string
	headers  map[string]string
	client   *http.Client
	onNotify notificationHandler

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
	resp, err := t.client.Do(req)
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
		respBody, err := io.ReadAll(resp.Body)
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
		err := readSSE(resp.Body, func(ev sseEvent) (bool, error) {
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
			// Server-initiated request or notification arriving inline
			// with our response stream: log-and-continue. This client
			// does not implement any server-initiated methods (no roots,
			// sampling, or elicitation), so requests go unanswered here
			// rather than over a duplex channel — see the package doc's
			// deferred-features list.
			t.onNotify(msg.Method, msg.Params)
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
	resp, err := t.client.Do(req)
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
	resp, err := t.client.Do(req)
	if err != nil {
		return nil //nolint:nilerr // best-effort; server may not support DELETE, or may never respond (bounded by closeSessionDeleteTimeout above)
	}
	defer resp.Body.Close()
	return nil
}

func httpStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var msg message
	if json.Unmarshal(body, &msg) == nil && msg.Error != nil {
		return msg.Error
	}
	return fmt.Errorf("mcp: http %d: %s", resp.StatusCode, string(body))
}

// stripParams removes any ";charset=..." style parameters from a
// Content-Type header value for comparison.
func stripParams(contentType string) string {
	if i := indexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return contentType
}
