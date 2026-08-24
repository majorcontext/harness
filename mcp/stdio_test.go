package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// fakeStdioServer is an in-process MCP server speaking newline-delimited
// JSON-RPC over an io.ReadWriteCloser, following the same pattern
// plugin/plugin_test.go uses (testPlugin + serve) for its fake plugins:
// tests dial into a net.Pipe instead of spawning a real subprocess.
type fakeStdioServer struct {
	protocolVersion string // version to report from initialize; defaults to LatestProtocolVersion
	tools           []Tool
	pageSize        int // 0 = no pagination
	callTool        func(name string, arguments json.RawMessage) (*CallToolResult, error)
	malformedCall   bool // tools/call returns an invalid JSON-RPC line instead of a normal response
	notify          func(c *conn)
	stuckCursor     bool // tools/list always returns the same non-advancing NextCursor
}

func (s *fakeStdioServer) handle(_ context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case methodInitialize:
		v := s.protocolVersion
		if v == "" {
			v = LatestProtocolVersion
		}
		return InitializeResult{
			ProtocolVersion: v,
			ServerInfo:      Implementation{Name: "fake-server", Version: "0.0.1"},
			Capabilities:    ServerCapabilities{Tools: &ToolsCapability{}},
		}, nil

	case methodToolsList:
		var req listToolsParams
		_ = json.Unmarshal(params, &req)
		return s.listTools(req.Cursor)

	case methodToolsCall:
		var req callToolParams
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, err
		}
		if s.callTool != nil {
			argRaw, _ := json.Marshal(req.Arguments)
			return s.callTool(req.Name, argRaw)
		}
		return &CallToolResult{Content: []Content{{Type: ContentTypeText, Text: "ok"}}}, nil

	default:
		return nil, &RPCError{Code: codeMethodNotFound, Message: fmt.Sprintf("unknown method %q", method)}
	}
}

func (s *fakeStdioServer) listTools(cursor string) (*ListToolsResult, error) {
	if s.stuckCursor {
		return &ListToolsResult{Tools: s.tools, NextCursor: "stuck-cursor"}, nil
	}
	return pageTools(s.tools, s.pageSize, cursor)
}

// pageTools implements opaque cursor-based pagination
// (https://modelcontextprotocol.io/specification/2025-11-25/server/utilities/pagination)
// for the fake servers shared by the stdio and HTTP conformance tests.
// pageSize <= 0 means unpaginated (return everything, no nextCursor).
func pageTools(tools []Tool, pageSize int, cursor string) (*ListToolsResult, error) {
	if pageSize <= 0 {
		return &ListToolsResult{Tools: tools}, nil
	}
	start := 0
	if cursor != "" {
		var err error
		start, err = decodeCursor(cursor)
		if err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: "invalid cursor"}
		}
	}
	end := start + pageSize
	if end > len(tools) {
		end = len(tools)
	}
	result := &ListToolsResult{Tools: tools[start:end]}
	if end < len(tools) {
		result.NextCursor = encodeCursor(end)
	}
	return result, nil
}

func encodeCursor(n int) string { return fmt.Sprintf("page:%d", n) }
func decodeCursor(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "page:%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

// dial returns a StdioTransport wired to this fake server over a net.Pipe.
func (s *fakeStdioServer) dial(t *testing.T) *StdioTransport {
	t.Helper()
	return &StdioTransport{
		dial: func() (io.ReadWriteCloser, error) {
			clientSide, serverSide := net.Pipe()
			go func() {
				c := newConn(serverSide, s.handle)
				if s.notify != nil {
					s.notify(c)
				}
				_ = c.run()
			}()
			return clientSide, nil
		},
	}
}

func newTestClient(t *testing.T, tr Transport, opts Options) *Client {
	t.Helper()
	c, err := NewClient(tr, opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func mustInitialize(t *testing.T, c *Client) *InitializeResult {
	t.Helper()
	res, err := c.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return res
}

func TestStdioInitializeHandshake(t *testing.T) {
	srv := &fakeStdioServer{}
	c := newTestClient(t, srv.dial(t), Options{ClientInfo: Implementation{Name: "test-client", Version: "1.0.0"}})

	res := mustInitialize(t, c)
	if res.ProtocolVersion != LatestProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want %q", res.ProtocolVersion, LatestProtocolVersion)
	}
	if res.ServerInfo.Name != "fake-server" {
		t.Errorf("ServerInfo.Name = %q", res.ServerInfo.Name)
	}
	if c.ProtocolVersion() != LatestProtocolVersion {
		t.Errorf("Client.ProtocolVersion() = %q", c.ProtocolVersion())
	}
	if c.ServerInfo().Name != "fake-server" {
		t.Errorf("Client.ServerInfo().Name = %q", c.ServerInfo().Name)
	}
}

func TestStdioInitializeOlderServerVersion(t *testing.T) {
	// A server offering an older, still-supported protocol version must be
	// accepted (per spec version negotiation: the client MUST use the
	// server's response version).
	srv := &fakeStdioServer{protocolVersion: "2024-11-05"}
	c := newTestClient(t, srv.dial(t), Options{})

	res := mustInitialize(t, c)
	if res.ProtocolVersion != "2024-11-05" {
		t.Errorf("ProtocolVersion = %q, want 2024-11-05", res.ProtocolVersion)
	}
	if c.ProtocolVersion() != "2024-11-05" {
		t.Errorf("Client.ProtocolVersion() = %q", c.ProtocolVersion())
	}
}

func TestStdioInitializeUnsupportedServerVersion(t *testing.T) {
	srv := &fakeStdioServer{protocolVersion: "1999-01-01"}
	c := newTestClient(t, srv.dial(t), Options{})

	_, err := c.Initialize(context.Background())
	if err == nil {
		t.Fatal("expected error for unsupported protocol version")
	}
}

func TestStdioListToolsPagination(t *testing.T) {
	var tools []Tool
	for i := 0; i < 5; i++ {
		tools = append(tools, Tool{Name: fmt.Sprintf("tool-%d", i)})
	}
	srv := &fakeStdioServer{tools: tools, pageSize: 2}
	c := newTestClient(t, srv.dial(t), Options{})
	mustInitialize(t, c)

	var got []Tool
	cursor := ""
	pages := 0
	for {
		page, err := c.ListTools(context.Background(), cursor)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		got = append(got, page.Tools...)
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3", pages)
	}
	if len(got) != 5 {
		t.Fatalf("got %d tools, want 5", len(got))
	}
	for i, tool := range got {
		if tool.Name != fmt.Sprintf("tool-%d", i) {
			t.Errorf("tools[%d].Name = %q", i, tool.Name)
		}
	}
}

func TestStdioListAllTools(t *testing.T) {
	srv := &fakeStdioServer{tools: []Tool{{Name: "a"}, {Name: "b"}, {Name: "c"}}, pageSize: 1}
	c := newTestClient(t, srv.dial(t), Options{})
	mustInitialize(t, c)

	all, err := c.ListAllTools(context.Background())
	if err != nil {
		t.Fatalf("ListAllTools: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d tools, want 3", len(all))
	}
}

// TestStdioListAllToolsNonAdvancingCursor guards against a server bug (or
// malicious server) that keeps returning the same NextCursor forever:
// ListAllTools must error instead of looping without bound.
func TestStdioListAllToolsNonAdvancingCursor(t *testing.T) {
	srv := &fakeStdioServer{
		tools:       []Tool{{Name: "a"}},
		pageSize:    1,
		stuckCursor: true,
	}
	c := newTestClient(t, srv.dial(t), Options{})
	mustInitialize(t, c)

	done := make(chan error, 1)
	go func() {
		_, err := c.ListAllTools(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error for a non-advancing cursor, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListAllTools did not terminate on a repeated cursor")
	}
}

func TestStdioCallToolSuccess(t *testing.T) {
	srv := &fakeStdioServer{
		callTool: func(name string, arguments json.RawMessage) (*CallToolResult, error) {
			if name != "echo" {
				t.Errorf("name = %q", name)
			}
			return &CallToolResult{Content: []Content{
				{Type: ContentTypeText, Text: "hello"},
				{Type: ContentTypeImage, Data: "YmFzZTY0", MimeType: "image/png"},
			}}, nil
		},
	}
	c := newTestClient(t, srv.dial(t), Options{})
	mustInitialize(t, c)

	res, err := c.CallTool(context.Background(), "echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Errorf("IsError = true, want false")
	}
	if len(res.Content) != 2 {
		t.Fatalf("got %d content items, want 2", len(res.Content))
	}
	if res.Content[0].Type != ContentTypeText || res.Content[0].Text != "hello" {
		t.Errorf("content[0] = %+v", res.Content[0])
	}
	if res.Content[1].Type != ContentTypeImage || res.Content[1].MimeType != "image/png" {
		t.Errorf("content[1] = %+v", res.Content[1])
	}
}

func TestStdioCallToolIsError(t *testing.T) {
	srv := &fakeStdioServer{
		callTool: func(name string, arguments json.RawMessage) (*CallToolResult, error) {
			return &CallToolResult{
				Content: []Content{{Type: ContentTypeText, Text: "boom: division by zero"}},
				IsError: true,
			}, nil
		},
	}
	c := newTestClient(t, srv.dial(t), Options{})
	mustInitialize(t, c)

	res, err := c.CallTool(context.Background(), "divide", nil)
	if err != nil {
		t.Fatalf("CallTool returned protocol error for a tool-level failure: %v", err)
	}
	if !res.IsError {
		t.Fatal("IsError = false, want true")
	}
	if res.Content[0].Text == "" {
		t.Error("expected error text in content")
	}
}

func TestStdioCallToolUnknownName(t *testing.T) {
	srv := &fakeStdioServer{
		callTool: func(name string, arguments json.RawMessage) (*CallToolResult, error) {
			return nil, &RPCError{Code: codeInvalidParams, Message: "unknown tool"}
		},
	}
	c := newTestClient(t, srv.dial(t), Options{})
	mustInitialize(t, c)

	_, err := c.CallTool(context.Background(), "nope", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var rerr *RPCError
	if !errors.As(err, &rerr) {
		t.Fatalf("error = %v, want *RPCError", err)
	}
	if rerr.Code != codeInvalidParams {
		t.Errorf("code = %d, want %d", rerr.Code, codeInvalidParams)
	}
}

func TestStdioMalformedResponse(t *testing.T) {
	// A response that is valid JSON-RPC (so it correlates to our request)
	// but whose "result" doesn't have the shape CallToolResult expects —
	// this must fail fast with a decode error, not hang until timeout.
	srv := &fakeStdioServer{}
	tr := &StdioTransport{
		dial: func() (io.ReadWriteCloser, error) {
			clientSide, serverSide := net.Pipe()
			handler := func(ctx context.Context, method string, params json.RawMessage) (any, error) {
				if method == methodToolsCall {
					// Valid envelope, but "result" is a bare string
					// rather than the {content, isError} object the
					// client expects.
					return "this is not a CallToolResult", nil
				}
				return srv.handle(ctx, method, params)
			}
			c := newConn(serverSide, handler)
			go c.run() //nolint:errcheck
			return clientSide, nil
		},
	}
	c := newTestClient(t, tr, Options{RequestTimeout: 5 * time.Second})
	mustInitialize(t, c)

	_, err := c.CallTool(context.Background(), "anything", nil)
	if err == nil {
		t.Fatal("expected an error for a malformed tools/call response")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("malformed response should fail fast, not time out")
	}
}

func TestStdioServerInitiatedNotification(t *testing.T) {
	got := make(chan string, 1)
	release := make(chan *conn, 1)
	srv := &fakeStdioServer{
		notify: func(c *conn) { release <- c },
	}
	c := newTestClient(t, srv.dial(t), Options{
		OnNotification: func(method string, _ json.RawMessage) { got <- method },
	})
	mustInitialize(t, c)

	serverConn := <-release
	if err := serverConn.notify(context.Background(), "notifications/message", map[string]any{"level": "info"}); err != nil {
		t.Fatalf("server notify: %v", err)
	}

	select {
	case method := <-got:
		if method != "notifications/message" {
			t.Errorf("method = %q", method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OnNotification")
	}
}

func TestStdioRequestTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })

		srv := &fakeStdioServer{
			callTool: func(name string, _ json.RawMessage) (*CallToolResult, error) {
				<-release // hang until cleanup; the timeout must fire first
				return &CallToolResult{}, nil
			},
		}
		c := newTestClient(t, srv.dial(t), Options{RequestTimeout: 50 * time.Millisecond})
		mustInitialize(t, c)

		_, err := c.CallTool(context.Background(), "slow", nil)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	})
}

func TestStdioContextCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })

		srv := &fakeStdioServer{
			callTool: func(name string, _ json.RawMessage) (*CallToolResult, error) {
				<-release
				return &CallToolResult{}, nil
			},
		}
		c := newTestClient(t, srv.dial(t), Options{RequestTimeout: time.Hour})
		mustInitialize(t, c)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := c.CallTool(ctx, "slow", nil)
			done <- err
		}()

		synctest.Wait()
		cancel()

		err := <-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}
