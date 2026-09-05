package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// Transport configures how a Client connects to an MCP server.
type Transport interface {
	open(onNotify notificationHandler) (transport, error)
}

// Options configures a Client.
type Options struct {
	// ClientInfo identifies this client to the server during initialize.
	// Defaults to {Name: "harness-mcp-client"} if Name is empty.
	ClientInfo Implementation
	// RequestTimeout bounds every request. It defaults to 30 seconds.
	RequestTimeout time.Duration
	// OnNotification observes unsupported server notifications and requests.
	// The default logs them and continues.
	OnNotification func(method string, params json.RawMessage)
	// Logger is used by the default OnNotification. Defaults to
	// log.Default().
	Logger *log.Logger
}

// Client is an MCP client. It is safe for concurrent use after Initialize.
type Client struct {
	tr   transport
	opts Options

	mu              sync.RWMutex
	initialized     bool
	serverInfo      Implementation
	protocolVersion string
	serverCaps      ServerCapabilities
}

// NewClient opens t and returns a client ready for Initialize.
func NewClient(t Transport, opts Options) (*Client, error) {
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 30 * time.Second
	}
	if opts.ClientInfo.Name == "" {
		opts.ClientInfo.Name = "harness-mcp-client"
	}
	onNotify := opts.OnNotification
	if onNotify == nil {
		logger := opts.Logger
		if logger == nil {
			logger = log.Default()
		}
		onNotify = func(method string, params json.RawMessage) {
			logger.Printf("mcp: unhandled server notification %q: %s", method, string(params))
		}
	}
	tr, err := t.open(onNotify)
	if err != nil {
		return nil, err
	}
	return &Client{tr: tr, opts: opts}, nil
}

// Initialize completes the MCP initialization handshake.
// Call Initialize once before other Client methods.
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	params := initializeParams{
		ProtocolVersion: LatestProtocolVersion,
		Capabilities:    ClientCapabilities{},
		ClientInfo:      c.opts.ClientInfo,
	}
	var result InitializeResult
	if err := c.request(ctx, methodInitialize, params, &result); err != nil {
		return nil, fmt.Errorf("mcp: initialize: %w", err)
	}
	if !isSupportedProtocolVersion(result.ProtocolVersion) {
		return nil, fmt.Errorf("mcp: server negotiated unsupported protocol version %q", result.ProtocolVersion)
	}
	// HTTP sends the negotiated version on later requests.
	if ht, ok := c.tr.(*httpTransport); ok {
		ht.setProtocolVersion(result.ProtocolVersion)
	}
	if err := c.notify(ctx, notificationInitialized, struct{}{}); err != nil {
		return nil, fmt.Errorf("mcp: initialized notification: %w", err)
	}

	c.mu.Lock()
	c.initialized = true
	c.serverInfo = result.ServerInfo
	c.protocolVersion = result.ProtocolVersion
	c.serverCaps = result.Capabilities
	c.mu.Unlock()
	return &result, nil
}

// ServerInfo returns the server's implementation info from initialize.
// Only meaningful after Initialize has returned successfully.
func (c *Client) ServerInfo() Implementation {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serverInfo
}

// ProtocolVersion returns the negotiated protocol version. Only meaningful
// after Initialize has returned successfully.
func (c *Client) ProtocolVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.protocolVersion
}

// ServerCapabilities returns the server's declared capabilities. Only
// meaningful after Initialize has returned successfully.
func (c *Client) ServerCapabilities() ServerCapabilities {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serverCaps
}

// ListTools requests one page of the server's tool list. Pass the previous
// result's NextCursor to fetch the next page; an empty cursor requests the
// first page. A response with an empty NextCursor means there are no more
// pages.
func (c *Client) ListTools(ctx context.Context, cursor string) (*ListToolsResult, error) {
	var result ListToolsResult
	if err := c.request(ctx, methodToolsList, listToolsParams{Cursor: cursor}, &result); err != nil {
		return nil, fmt.Errorf("mcp: tools/list: %w", err)
	}
	return &result, nil
}

// maxListAllToolsPages caps the number of tools/list pages ListAllTools
// will fetch, as a backstop against a server that never terminates
// pagination (e.g. always minting a fresh cursor).
const maxListAllToolsPages = 1000

// ListAllTools returns every tools/list page in one slice.
// It rejects repeated cursors and caps the number of pages.
func (c *Client) ListAllTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	seen := make(map[string]struct{})
	for pages := 0; ; pages++ {
		if pages >= maxListAllToolsPages {
			return nil, fmt.Errorf("mcp: tools/list: exceeded %d pages without terminating", maxListAllToolsPages)
		}
		page, err := c.ListTools(ctx, cursor)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			return all, nil
		}
		if _, dup := seen[page.NextCursor]; dup {
			return nil, fmt.Errorf("mcp: tools/list: server returned non-advancing cursor %q", page.NextCursor)
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
}

// CallTool invokes a tool by name with the given arguments (typically a
// map[string]any or a struct that marshals to a JSON object). A non-nil
// error means the call failed at the protocol level (e.g. unknown tool
// name); a successful call with a tool-level failure is reported via
// CallToolResult.IsError, not an error.
func (c *Client) CallTool(ctx context.Context, name string, arguments any) (*CallToolResult, error) {
	var result CallToolResult
	if err := c.request(ctx, methodToolsCall, callToolParams{Name: name, Arguments: arguments}, &result); err != nil {
		return nil, fmt.Errorf("mcp: tools/call %s: %w", name, err)
	}
	return &result, nil
}

// Close shuts down the connection and removes an HTTP session when present.
func (c *Client) Close() error {
	return c.tr.close()
}

// request applies the configured timeout without extending ctx's deadline.
func (c *Client) request(ctx context.Context, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
	defer cancel()
	return c.tr.call(ctx, method, params, result)
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	ctx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
	defer cancel()
	return c.tr.notify(ctx, method, params)
}
