package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// restoreBody replaces an already-drained request body with a fresh reader
// over the bytes read from it, so a wrapper handler can peek at the body
// (to decide how to route the request) and then hand off to a fake server
// that also needs to read it.
func restoreBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
}

// fakeHTTPServer implements just enough of the Streamable HTTP transport
// (https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#streamable-http)
// to exercise the client: single POST endpoint, optional session ID
// issuance/enforcement, and either a plain JSON or an SSE response per
// request, at the test's discretion.
type fakeHTTPServer struct {
	protocolVersion string // reported by initialize; defaults to LatestProtocolVersion
	sessionID       string // "" disables session enforcement
	requireAuth     string // if non-empty, the exact Authorization header value required
	tools           []Tool
	pageSize        int
	callTool        func(name string, arguments json.RawMessage) (*CallToolResult, error)
	streamToolsCall bool // answer tools/call over SSE with a leading unrelated notification

	mu          sync.Mutex
	seenAuth    []string
	seenSession []string
}

func (s *fakeHTTPServer) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(srv.Close)
	return srv
}

func (s *fakeHTTPServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.seenAuth = append(s.seenAuth, r.Header.Get("Authorization"))
	s.seenSession = append(s.seenSession, r.Header.Get(sessionIDHeader))
	s.mu.Unlock()

	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.requireAuth != "" && r.Header.Get("Authorization") != s.requireAuth {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var msg message
	if json.Unmarshal(body, &msg) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if s.sessionID != "" && msg.Method != methodInitialize {
		if r.Header.Get(sessionIDHeader) != s.sessionID {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	if msg.isNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rerr := s.handle(msg.Method, msg.Params)
	if msg.Method == methodInitialize && s.sessionID != "" {
		w.Header().Set(sessionIDHeader, s.sessionID)
	}

	if s.streamToolsCall && msg.Method == methodToolsCall {
		s.writeSSE(w, msg.ID, result, rerr)
		return
	}
	s.writeJSON(w, msg.ID, result, rerr)
}

func (s *fakeHTTPServer) handle(method string, params json.RawMessage) (any, *RPCError) {
	switch method {
	case methodInitialize:
		v := s.protocolVersion
		if v == "" {
			v = LatestProtocolVersion
		}
		return InitializeResult{
			ProtocolVersion: v,
			ServerInfo:      Implementation{Name: "fake-http-server", Version: "0.0.1"},
			Capabilities:    ServerCapabilities{Tools: &ToolsCapability{}},
		}, nil

	case methodToolsList:
		var req listToolsParams
		_ = json.Unmarshal(params, &req)
		res, err := pageTools(s.tools, s.pageSize, req.Cursor)
		if err != nil {
			var rerr *RPCError
			errors.As(err, &rerr)
			return nil, rerr
		}
		return res, nil

	case methodToolsCall:
		var req callToolParams
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}
		}
		if s.callTool != nil {
			argRaw, _ := json.Marshal(req.Arguments)
			res, err := s.callTool(req.Name, argRaw)
			if err != nil {
				var rerr *RPCError
				if errors.As(err, &rerr) {
					return nil, rerr
				}
				return nil, &RPCError{Code: codeInternalError, Message: err.Error()}
			}
			return res, nil
		}
		return &CallToolResult{Content: []Content{{Type: ContentTypeText, Text: "ok"}}}, nil

	default:
		return nil, &RPCError{Code: codeMethodNotFound, Message: fmt.Sprintf("unknown method %q", method)}
	}
}

func (s *fakeHTTPServer) writeJSON(w http.ResponseWriter, id json.RawMessage, result any, rerr *RPCError) {
	msg := message{JSONRPC: "2.0", ID: id}
	if rerr != nil {
		msg.Error = rerr
	} else {
		raw, _ := json.Marshal(result)
		msg.Result = raw
	}
	body, _ := json.Marshal(msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *fakeHTTPServer) writeSSE(w http.ResponseWriter, id json.RawMessage, result any, rerr *RPCError) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	// An unrelated server notification, interleaved before the response,
	// per the transport's allowance that "[t]he server MAY send JSON-RPC
	// requests and notifications before sending the JSON-RPC response."
	note, _ := json.Marshal(message{JSONRPC: "2.0", Method: "notifications/message", Params: json.RawMessage(`{"level":"info","data":"working"}`)})
	fmt.Fprintf(w, "data: %s\n\n", note)
	flusher.Flush()

	msg := message{JSONRPC: "2.0", ID: id}
	if rerr != nil {
		msg.Error = rerr
	} else {
		raw, _ := json.Marshal(result)
		msg.Result = raw
	}
	body, _ := json.Marshal(msg)
	fmt.Fprintf(w, "data: %s\n\n", body)
	flusher.Flush()
}

func newHTTPTestClient(t *testing.T, srv *fakeHTTPServer, headers map[string]string) *Client {
	t.Helper()
	httpSrv := srv.server(t)
	tr := &HTTPTransport{Endpoint: httpSrv.URL, Headers: headers}
	return newTestClient(t, tr, Options{})
}

func TestHTTPInitializeHandshakeAndSession(t *testing.T) {
	srv := &fakeHTTPServer{sessionID: "sess-123"}
	c := newHTTPTestClient(t, srv, nil)

	res := mustInitialize(t, c)
	if res.ProtocolVersion != LatestProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want %q", res.ProtocolVersion, LatestProtocolVersion)
	}

	// tools/list must carry the session ID the server issued during
	// initialize on every subsequent request.
	if _, err := c.ListTools(context.Background(), ""); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.seenSession) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(srv.seenSession))
	}
	for i, id := range srv.seenSession[1:] {
		if id != "sess-123" {
			t.Errorf("request %d: Mcp-Session-Id = %q, want sess-123", i+1, id)
		}
	}
}

func TestHTTPInitializeOlderServerVersion(t *testing.T) {
	srv := &fakeHTTPServer{protocolVersion: "2025-03-26"}
	c := newHTTPTestClient(t, srv, nil)

	res := mustInitialize(t, c)
	if res.ProtocolVersion != "2025-03-26" {
		t.Errorf("ProtocolVersion = %q, want 2025-03-26", res.ProtocolVersion)
	}
	if c.ProtocolVersion() != "2025-03-26" {
		t.Errorf("Client.ProtocolVersion() = %q", c.ProtocolVersion())
	}
}

func TestHTTPInitializeUnsupportedServerVersion(t *testing.T) {
	srv := &fakeHTTPServer{protocolVersion: "0000-00-00"}
	c := newHTTPTestClient(t, srv, nil)

	if _, err := c.Initialize(context.Background()); err == nil {
		t.Fatal("expected error for unsupported protocol version")
	}
}

func TestHTTPProtocolVersionHeaderSentAfterInitialize(t *testing.T) {
	srv := &fakeHTTPServer{}

	var gotHeader string
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(protocolVersionHeader) != "" {
			gotHeader = r.Header.Get(protocolVersionHeader)
		}
		srv.serveHTTP(w, r)
	})
	httpSrv := httptest.NewServer(wrapped)
	t.Cleanup(httpSrv.Close)

	tr := &HTTPTransport{Endpoint: httpSrv.URL}
	c := newTestClient(t, tr, Options{})
	mustInitialize(t, c)
	if _, err := c.ListTools(context.Background(), ""); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if gotHeader != LatestProtocolVersion {
		t.Errorf("MCP-Protocol-Version header = %q, want %q", gotHeader, LatestProtocolVersion)
	}
}

func TestHTTPListToolsPagination(t *testing.T) {
	var tools []Tool
	for i := 0; i < 5; i++ {
		tools = append(tools, Tool{Name: fmt.Sprintf("tool-%d", i)})
	}
	srv := &fakeHTTPServer{tools: tools, pageSize: 2}
	c := newHTTPTestClient(t, srv, nil)
	mustInitialize(t, c)

	all, err := c.ListAllTools(context.Background())
	if err != nil {
		t.Fatalf("ListAllTools: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("got %d tools, want 5", len(all))
	}
}

func TestHTTPCallToolSuccess(t *testing.T) {
	srv := &fakeHTTPServer{
		callTool: func(name string, _ json.RawMessage) (*CallToolResult, error) {
			return &CallToolResult{Content: []Content{
				{Type: ContentTypeText, Text: "hi"},
				{Type: ContentTypeImage, Data: "aGVsbG8=", MimeType: "image/png"},
			}}, nil
		},
	}
	c := newHTTPTestClient(t, srv, nil)
	mustInitialize(t, c)

	res, err := c.CallTool(context.Background(), "greet", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Error("IsError = true, want false")
	}
	if len(res.Content) != 2 || res.Content[1].Type != ContentTypeImage {
		t.Fatalf("Content = %+v", res.Content)
	}
}

func TestHTTPCallToolIsError(t *testing.T) {
	srv := &fakeHTTPServer{
		callTool: func(string, json.RawMessage) (*CallToolResult, error) {
			return &CallToolResult{
				Content: []Content{{Type: ContentTypeText, Text: "failed to divide by zero"}},
				IsError: true,
			}, nil
		},
	}
	c := newHTTPTestClient(t, srv, nil)
	mustInitialize(t, c)

	res, err := c.CallTool(context.Background(), "divide", nil)
	if err != nil {
		t.Fatalf("CallTool returned protocol error for tool-level failure: %v", err)
	}
	if !res.IsError {
		t.Fatal("IsError = false, want true")
	}
}

func TestHTTPCallToolOverSSEWithInterleavedNotification(t *testing.T) {
	got := make(chan string, 1)
	srv := &fakeHTTPServer{
		streamToolsCall: true,
		callTool: func(string, json.RawMessage) (*CallToolResult, error) {
			return &CallToolResult{Content: []Content{{Type: ContentTypeText, Text: "done"}}}, nil
		},
	}
	httpSrv := srv.server(t)
	tr := &HTTPTransport{Endpoint: httpSrv.URL}
	c := newTestClient(t, tr, Options{
		OnNotification: func(method string, _ json.RawMessage) { got <- method },
	})
	mustInitialize(t, c)

	res, err := c.CallTool(context.Background(), "slow", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Content[0].Text != "done" {
		t.Errorf("Content = %+v", res.Content)
	}
	select {
	case method := <-got:
		if method != "notifications/message" {
			t.Errorf("method = %q", method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for interleaved notification")
	}
}

func TestHTTPMalformedResponse(t *testing.T) {
	srv := &fakeHTTPServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg message
		_ = json.Unmarshal(body, &msg)
		if msg.Method == methodInitialize {
			restoreBody(r, body)
			srv.serveHTTP(w, r)
			return
		}
		// A 200 OK response claiming application/json but whose result
		// isn't a valid CallToolResult shape at all.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"result":"not an object"}`))
	})
	httpSrv := httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)

	tr := &HTTPTransport{Endpoint: httpSrv.URL}
	c := newTestClient(t, tr, Options{})
	mustInitialize(t, c)

	_, err := c.CallTool(context.Background(), "anything", nil)
	if err == nil {
		t.Fatal("expected error for malformed tools/call response")
	}
}

func TestHTTPStaticAuthHeader(t *testing.T) {
	srv := &fakeHTTPServer{requireAuth: "Bearer secret-token"}

	t.Run("with header", func(t *testing.T) {
		c := newHTTPTestClient(t, srv, map[string]string{"Authorization": "Bearer secret-token"})
		if _, err := c.Initialize(context.Background()); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
	})

	t.Run("without header", func(t *testing.T) {
		c := newHTTPTestClient(t, srv, nil)
		if _, err := c.Initialize(context.Background()); err == nil {
			t.Fatal("expected error without Authorization header")
		}
	})
}

func TestHTTPRequestTimeout(t *testing.T) {
	// httptest.Server uses real sockets, which testing/synctest's fake
	// clock does not govern (per AGENTS.md: "real network and file I/O do
	// not" work in a synctest bubble) — so this test uses a real, small
	// timeout instead. The server handler blocks forever on a channel
	// closed at cleanup, so the timeout is what ends the request; there is
	// no arbitrary failsafe layered on top of it.
	release := make(chan struct{})
	started := make(chan struct{})

	mux := http.NewServeMux()
	srv := &fakeHTTPServer{}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg message
		_ = json.Unmarshal(body, &msg)
		if msg.Method == methodInitialize || msg.isNotification() {
			restoreBody(r, body)
			srv.serveHTTP(w, r)
			return
		}
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	})
	httpSrv := httptest.NewServer(mux)
	// Cleanup order is LIFO: release the blocked handler goroutine before
	// httpSrv.Close() (which waits for outstanding requests) tries to
	// shut the server down, or Close would deadlock waiting on it.
	t.Cleanup(httpSrv.Close)
	t.Cleanup(func() { close(release) })

	tr := &HTTPTransport{Endpoint: httpSrv.URL}
	c := newTestClient(t, tr, Options{RequestTimeout: 30 * time.Millisecond})
	mustInitialize(t, c)

	_, err := c.CallTool(context.Background(), "slow", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	<-started
}

func TestHTTPContextCancellation(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	mux := http.NewServeMux()
	srv := &fakeHTTPServer{}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg message
		_ = json.Unmarshal(body, &msg)
		if msg.Method == methodInitialize || msg.isNotification() {
			restoreBody(r, body)
			srv.serveHTTP(w, r)
			return
		}
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	})
	httpSrv := httptest.NewServer(mux)
	// See the ordering note in TestHTTPRequestTimeout above.
	t.Cleanup(httpSrv.Close)
	t.Cleanup(func() { close(release) })

	tr := &HTTPTransport{Endpoint: httpSrv.URL}
	c := newTestClient(t, tr, Options{RequestTimeout: time.Hour})
	mustInitialize(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.CallTool(ctx, "slow", nil)
		done <- err
	}()

	<-started
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
