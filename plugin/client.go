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

// Client is the handle of a plugin to the harness.
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

// WorkspaceDir is the project directory of the harness, or "" when it has none.
func (c *Client) WorkspaceDir() string { return c.params().WorkspaceDir }

// HarnessVersion is the version of the harness.
func (c *Client) HarnessVersion() string { return c.params().HarnessVersion }

// Config is the config block of this plugin as JSON, or nil when it has none.
func (c *Client) Config() json.RawMessage { return bytes.Clone(c.params().Config) }

// HTTPHeaders returns a copy of the headers that the harness wants on outbound HTTP traffic.
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

// ServeURL is the base URL of the `harness serve` HTTP API, or "" in run mode.
func (c *Client) ServeURL() string { return c.params().ServeURL }

// RunToken is the bearer token for ServeURL.
func (c *Client) RunToken() string { return c.params().RunToken }

// SessionMessages returns the messages of a session.
func (c *Client) SessionMessages(ctx context.Context, sessionID string) ([]Message, error) {
	var resp SessionMessagesResponse
	if err := c.conn.Call(ctx, wire.MethodSessionMessages, &SessionMessagesRequest{SessionID: sessionID}, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// MCPCallRequest is the request of the client/mcp.call method.
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

// MCPCall calls a tool of an MCP server of the harness config. A tool error
// sets IsError; an unknown server or a failed connect returns an error.
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
	MaxTokens int       `json:"max_tokens,omitempty"` // 0 keeps the default
}

// Usage counts the tokens of a model call.
type Usage = protocol.Usage

// GenerateResponse is the reply of a model call.
type GenerateResponse struct {
	Message Message `json:"message"`
	Usage   *Usage  `json:"usage,omitempty"` // zero counts when the model reports none
}

// Generate makes one tool-less model call through the harness provider layer.
func (c *Client) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	var resp GenerateResponse
	if err := c.conn.Call(ctx, wire.MethodGenerate, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
