package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// Transport selects and configures how a Client connects to an MCP server.
// The two implementations this package provides are StdioTransport and
// HTTPTransport; the interface is otherwise unexported (sealed) since the
// protocol defines exactly these two standard transports.
type Transport interface {
	open(onNotify notificationHandler) (transport, error)
}

// Options configures a Client.
type Options struct {
	// ClientInfo identifies this client to the server during initialize.
	// Defaults to {Name: "harness-mcp-client"} if Name is empty.
	ClientInfo Implementation
	// RequestTimeout bounds every request (initialize, tools/list,
	// tools/call). Defaults to 30s. Callers can additionally scope a
	// shorter deadline via the ctx passed to each call; whichever is
	// tighter wins.
	RequestTimeout time.Duration
	// OnNotification observes any notification (or, over the stdio
	// transport's duplex stream, request) from the server this client
	// does not implement — e.g. notifications/message (logging),
	// notifications/tools/list_changed, or roots/sampling requests. The
	// default logs via Logger and continues; it never causes a request to
	// fail.
	OnNotification func(method string, params json.RawMessage)
	// Logger is used by the default OnNotification. Defaults to
	// log.Default().
	Logger *log.Logger
}

// Client is an MCP client. It is safe for concurrent use after Initialize
// completes, matching the Streamable HTTP transport's expectation that a
// single session may see interleaved requests.
type Client struct {
	tr   transport
	opts Options

	mu              sync.RWMutex
	initialized     bool
	serverInfo      Implementation
	protocolVersion string
	serverCaps      ServerCapabilities
}

// NewClient opens the given transport (spawning a child process for
// StdioTransport, or preparing an HTTP client for HTTPTransport) and
// returns a Client ready for Initialize. It does not perform the
// initialize handshake itself — callers must call Initialize before using
// any other method, per the spec's lifecycle rules.
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

// Initialize performs the initialize/initialized lifecycle handshake:
// protocol version negotiation, capability exchange, and client info,
// followed by the initialized notification once the server has responded.
// It MUST be called exactly once before any other Client method.
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
	// The negotiated version, once known, rides on every subsequent HTTP
	// request as MCP-Protocol-Version; the stdio transport has no
	// equivalent header requirement.
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

// ListAllTools drains every page of tools/list into a single slice. It
// exists for convenience; callers that want to react to a large tool list
// incrementally should call ListTools directly.
//
// A server is expected to eventually return an empty NextCursor, but a
// buggy or hostile one might not. ListAllTools guards against that two
// ways: it errors immediately if a page's NextCursor repeats a cursor
// already seen (the common "stuck" case), and it errors if pagination
// still hasn't terminated after maxListAllToolsPages pages.
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

// Close shuts down the connection: for stdio, this closes the child
// process's stdin and waits for it to exit (escalating to SIGTERM/SIGKILL
// if it doesn't); for Streamable HTTP, this best-effort DELETEs the
// session if one was established. Per the spec, shutdown has no dedicated
// protocol message on either transport.
func (c *Client) Close() error {
	return c.tr.close()
}

// request wraps a call with the client's configured RequestTimeout: a
// child context is created so a hung server can't wedge the caller past
// that bound, without weakening any tighter deadline/cancellation the
// caller's ctx already carries. Context cancellation and timeout both
// unblock the call immediately.
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
