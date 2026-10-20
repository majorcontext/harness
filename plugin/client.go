package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"sync/atomic"

	"github.com/majorcontext/harness/internal/plugin/wire"
	"github.com/majorcontext/harness/protocol"
)

// Client is the handle of a plugin to the harness. Its calls to the host run
// no hook, and fail when the connection ends or the runtime closes.
type Client struct {
	conn *wire.Conn
	init atomic.Pointer[InitializeParams]
}

func (c *Client) params() InitializeParams {
	if p := c.init.Load(); p != nil {
		return *p
	}
	return InitializeParams{}
}

// WorkspaceDir is the work directory of the runtime, or "" when it has none.
func (c *Client) WorkspaceDir() string { return c.params().WorkspaceDir }

// HarnessVersion is the version of the harness.
func (c *Client) HarnessVersion() string { return c.params().HarnessVersion }

// Config is the "config" value of the plugin entry in the harness config, or
// nil when it has none, as for an in-process plugin.
func (c *Client) Config() json.RawMessage { return bytes.Clone(c.params().Config) }

// HTTPHeaders returns a copy of plugin_http_headers from the harness config.
func (c *Client) HTTPHeaders() map[string]string { return maps.Clone(c.params().HTTPHeaders) }

// HTTPClient returns an HTTP client that adds HTTPHeaders to each request. A
// header that the request sets wins. A redirect hop gets the headers only while
// every hop so far has the scheme and host of the first request.
func (c *Client) HTTPClient() *http.Client {
	return &http.Client{Transport: headerTransport{headers: c.HTTPHeaders()}}
}

type headerTransport struct{ headers map[string]string }

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	sameOrigin := true
	for r := req; r.Response != nil && r.Response.Request != nil; r = r.Response.Request {
		prev := r.Response.Request.URL
		sameOrigin = sameOrigin && prev.Scheme == req.URL.Scheme && prev.Host == req.URL.Host
	}
	req = req.Clone(req.Context())
	if !sameOrigin {
		return http.DefaultTransport.RoundTrip(req)
	}
	for k, v := range t.headers {
		if _, ok := req.Header[http.CanonicalHeaderKey(k)]; !ok {
			req.Header.Set(k, v)
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

// ServeURL is the base URL of the harness HTTP API, or "" when the host serves
// none.
func (c *Client) ServeURL() string { return c.params().ServeURL }

// RunToken is the bearer token for ServeURL.
func (c *Client) RunToken() string { return c.params().RunToken }

// SessionMessages returns the messages of a session in log order, with each
// attachment as a PartBlob part.
func (c *Client) SessionMessages(ctx context.Context, sessionID string) ([]Message, error) {
	var resp SessionMessagesResponse
	if err := c.conn.Call(ctx, wire.MethodSessionMessages, &SessionMessagesRequest{SessionID: sessionID}, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// MCPCallRequest is the wire record of Client.MCPCall.
type MCPCallRequest struct {
	Server string          `json:"server"` // name in the harness config
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args,omitempty"`
}

// MCPCallResult is the result of an MCP tool call.
type MCPCallResult struct {
	Content []Part `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// MCPCall calls a tool of a server in mcp_servers. A tool error sets IsError.
// An unknown server, a server that is down, retrying, or parked, or a failed
// transport returns an error. MCPCall never connects a server again.
func (c *Client) MCPCall(ctx context.Context, server, tool string, args json.RawMessage) (*MCPCallResult, error) {
	var out MCPCallResult
	if err := c.conn.Call(ctx, wire.MethodMCPCall, &MCPCallRequest{Server: server, Tool: tool, Args: args}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GenerateRequest is a model call through the harness provider layer.
type GenerateRequest struct {
	Model     string    `json:"model"` // provider/model reference or config alias; empty: the default model
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens,omitempty"` // 0 keeps the default; negative fails
}

// Usage counts the tokens of a model call.
type Usage = protocol.Usage

// GenerateResponse is the reply of a model call.
type GenerateResponse struct {
	Message Message `json:"message"`
	Usage   *Usage  `json:"usage,omitempty"` // zero counts when the model reports none
}

// Generate makes one model call with no tools, with the retry limits of the
// runtime. No session records it. A model whose backend runs its own agent
// loop, such as Claude Code, answers with no tool and no session.
func (c *Client) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	var resp GenerateResponse
	if err := c.conn.Call(ctx, wire.MethodGenerate, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
