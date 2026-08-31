package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// wsProtocolHeader is the "openai-beta" header value this transport sends
// on the dial handshake unless the caller already set one. Ported verbatim
// from opencode's ws.ts PROTOCOL_HEADER — the Codex backend's websocket
// endpoint uses it to negotiate the response.create wire shape this file
// speaks.
const wsProtocolHeader = "responses_websockets=2026-02-06"

// wsMessageTooBigCode is the WebSocket close code (RFC 6449) the Codex
// backend sends when a request or response frame exceeds its size limit —
// a permanent, this-session-can-never-work-over-ws condition, not a
// transient one. Ported from opencode's ws.ts MESSAGE_TOO_BIG_CLOSE_CODE.
const wsMessageTooBigCode = websocket.StatusMessageTooBig

// toWebSocketURL rewrites an http(s):// Responses URL to its ws(s)://
// equivalent. Ported from opencode's ws.ts toWebSocketUrl.
func toWebSocketURL(rawURL string) string {
	switch {
	case strings.HasPrefix(rawURL, "https://"):
		return "wss://" + strings.TrimPrefix(rawURL, "https://")
	case strings.HasPrefix(rawURL, "http://"):
		return "ws://" + strings.TrimPrefix(rawURL, "http://")
	default:
		return rawURL
	}
}

// dialResponsesWebSocket opens the persistent Codex Responses websocket:
// same URL/headers as the HTTP path (Authorization included), converted to
// ws(s)://, bounded by timeout. The dial goes through httpClient's own
// Transport — see the DialOptions.HTTPClient doc comment on
// github.com/coder/websocket — so it inherits whatever proxy (HTTPS_PROXY)
// and TLS trust store (SSL_CERT_FILE/SSL_CERT_DIR, the system pool) that
// client is already configured with, identically to every HTTP request
// this adapter makes. There is no separate proxy/CA plumbing to add here;
// reusing httpClient IS the proxy/CA support.
// dialResponsesWebSocket's second return value is the raw HTTP upgrade
// response — coder/websocket's own Dial return, non-nil on a successful
// upgrade (a normal 101 Switching Protocols) and often non-nil even on a
// failed one (a rejected upgrade the server answered with an ordinary HTTP
// error). wsPool.stream reads its Header off this for the x-codex-*
// subscription-usage headers (see codexSubscriptionUsageFromHeaders) —
// the Codex backend sends them on this same upgrade response, not inside
// any websocket frame, so there is no other point in this transport where
// they are ever visible.
func dialResponsesWebSocket(ctx context.Context, url string, headers http.Header, httpClient *http.Client, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	dialCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	hdr := headers.Clone()
	if hdr == nil {
		hdr = http.Header{}
	}
	if hdr.Get("openai-beta") == "" {
		hdr.Set("openai-beta", wsProtocolHeader)
	}
	// Content-Length describes the (nonexistent) body of the HTTP POST this
	// adapter would otherwise send; it has no meaning on a GET-style
	// Upgrade handshake and coder/websocket's Dial already ignores it, but
	// stripping it keeps the outgoing header set honest.
	hdr.Del("Content-Length")

	conn, resp, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{
		HTTPClient: httpClient,
		HTTPHeader: hdr,
	})
	if err != nil {
		return nil, resp, fmt.Errorf("openai: websocket dial: %w", err)
	}
	// A single oversized frame (tool output, a huge pasted file) must not
	// silently kill the connection: without raising this, coder/websocket's
	// 32KiB default read limit would surface as an ordinary close, which
	// this transport's caller (wsPool) cannot tell apart from the server's
	// own MESSAGE_TOO_BIG close. 64MiB matches the Responses API's own
	// documented per-request body cap, so nothing legitimate is still cut
	// short.
	conn.SetReadLimit(64 << 20)
	return conn, resp, nil
}

// sendResponseCreate frames body (the same JSON the HTTP path POSTs,
// {"model":..., "input":..., "stream":true, ...}) as a Codex websocket
// request: {"type":"response.create", ...body-minus-stream}. "stream" is
// dropped because it is meaningless once the transport itself IS the
// streaming channel — ported from opencode's ws.ts streamResponsesWebSocket
// (the payload destructure that drops "stream"/"background").
func sendResponseCreate(ctx context.Context, conn *websocket.Conn, body []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return fmt.Errorf("openai: websocket request.create: decoding request body: %w", err)
	}
	delete(fields, "stream")
	fields["type"] = json.RawMessage(`"response.create"`)
	payload, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("openai: websocket response.create: encoding request: %w", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		return fmt.Errorf("openai: websocket write response.create: %w", err)
	}
	return nil
}

// wsFrameEnvelope reads only the "type" discriminator out of one websocket
// frame — every other field is left as raw JSON for stream.handle to decode
// itself, exactly as it already does for an SSE "data:" payload.
type wsFrameEnvelope struct {
	Type string `json:"type"`
}

// readResponsesFrame reads one text frame from conn and returns its "type"
// field (the ws-frame analog of an SSE "event:" line — see stream.handle,
// which this transport reuses unmodified) alongside the raw frame bytes. A
// binary frame is a protocol violation the Codex backend never sends in
// practice (ported from opencode's ws.ts onMessage, which invalidates the
// connection on isBinary).
func readResponsesFrame(ctx context.Context, conn *websocket.Conn) (name string, data []byte, err error) {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return "", nil, err
	}
	if typ == websocket.MessageBinary {
		return "", nil, errors.New("openai: unexpected binary websocket frame")
	}
	var env wsFrameEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return "", nil, fmt.Errorf("openai: decoding websocket frame: %w", err)
	}
	return env.Type, data, nil
}

// wsTerminalEventTypes are the Responses websocket event "type" values that
// end a response.create's stream — mirrors the case list in stream.handle
// (response.completed/response.incomplete/response.failed/error) plus
// opencode's defensive extra "response.done", which harness's SSE path has
// never seen from the real API but ws.ts treats as terminal too.
func isWSTerminalEvent(name string) bool {
	switch name {
	case "response.completed", "response.done", "response.incomplete", "response.failed", "error":
		return true
	default:
		return false
	}
}

// isWSCleanTerminalEvent reports whether name is the terminal event kind
// that leaves the underlying connection reusable for the pool's next turn.
// Every other terminal (a model-level failure, not a transport failure)
// still ends the stream normally but the pool drops the socket rather than
// risk replaying against server-side state left by an abnormal end — ported
// from opencode's ws-pool.ts onTerminal.
func isWSCleanTerminalEvent(name string) bool {
	return name == "response.completed" || name == "response.done"
}
