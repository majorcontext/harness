package harnesstest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/mcp"
)

// MCPSpecEnv is the environment variable that carries a JSON MCPSpec to a
// stdio stub (see StdioEnv).
const MCPSpecEnv = "HARNESSTEST_MCP_SPEC"

const mcpSessionID = "harnesstest-session"

// MCPSpec scripts a fake MCP server: what it says at initialize and which
// tools and resources it serves.
type MCPSpec struct {
	Name         string
	Instructions string
	Tools        []MCPTool
	Resources    []MCPResource
	PageSize     int // when positive, tools/list and resources/list answer in pages of this size
}

// MCPTool is one scripted tool. A tools/call answers with the first of
// RPCError, Echo, Cwd, and Result that applies.
type MCPTool struct {
	Def      mcp.Tool
	Result   mcp.CallToolResult
	Echo     bool          // answer with the call arguments as one text item
	Cwd      bool          // answer with the base name of the server's working directory
	RPCError *mcp.RPCError // answer with a JSON-RPC error
}

// MCPResource is one scripted resource. A non-empty Blob (base64) is served
// instead of Text.
type MCPResource struct {
	Resource mcp.Resource
	Text     string
	Blob     string
}

// MCPCall is a tools/call or resources/read the server received. Name is
// the tool name or the resource URI.
type MCPCall struct {
	Method        string
	Name          string
	Args          any
	Authorization string
}

// StdioEnv is the environment for a stdio stub that serves s.
func (s MCPSpec) StdioEnv() []string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("harnesstest: marshal MCPSpec: %v", err))
	}
	return []string{MCPSpecEnv + "=" + string(b)}
}

type mcpHandler struct {
	spec  MCPSpec
	mu    sync.Mutex
	calls []MCPCall
}

type mcpRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcp.RPCError   `json:"error,omitempty"`
}

// handle answers one JSON-RPC message. It returns nil for a notification.
func (h *mcpHandler) handle(raw []byte, auth string) []byte {
	var req mcpRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return encodeMCP(mcpResponse{ID: json.RawMessage("null"), Error: &mcp.RPCError{Code: -32700, Message: "parse error"}})
	}
	if len(req.ID) == 0 {
		return nil
	}
	result, rpcErr := h.dispatch(req, auth)
	return encodeMCP(mcpResponse{ID: req.ID, Result: result, Error: rpcErr})
}

func encodeMCP(r mcpResponse) []byte {
	r.JSONRPC = "2.0"
	b, err := json.Marshal(r)
	if err != nil {
		panic(fmt.Sprintf("harnesstest: marshal MCP response: %v", err))
	}
	return b
}

func (h *mcpHandler) dispatch(req mcpRequest, auth string) (any, *mcp.RPCError) {
	switch req.Method {
	case "initialize":
		return h.initialize(), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return h.listTools(req.Params), nil
	case "tools/call":
		return h.callTool(req.Params, auth)
	case "resources/list":
		return h.listResources(req.Params), nil
	case "resources/read":
		return h.readResource(req.Params, auth)
	}
	return nil, &mcp.RPCError{Code: -32601, Message: "method not found: " + req.Method}
}

func (h *mcpHandler) initialize() any {
	caps := map[string]any{"tools": map[string]any{}}
	if len(h.spec.Resources) > 0 {
		caps["resources"] = map[string]any{}
	}
	return map[string]any{
		"protocolVersion": mcp.LatestProtocolVersion,
		"capabilities":    caps,
		"serverInfo":      map[string]any{"name": h.spec.Name, "version": "1"},
		"instructions":    h.spec.Instructions,
	}
}

// page returns the items of the page that params' cursor names, and the
// cursor of the next page ("" on the last). The cursor is the start index.
func (h *mcpHandler) page(params json.RawMessage, n int) (start, end int, next string) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(params, &p)
	start, _ = strconv.Atoi(p.Cursor)
	start = max(0, min(start, n))
	end = n
	if h.spec.PageSize > 0 && start+h.spec.PageSize < n {
		end = start + h.spec.PageSize
		next = strconv.Itoa(end)
	}
	return start, end, next
}

func (h *mcpHandler) listTools(params json.RawMessage) any {
	start, end, next := h.page(params, len(h.spec.Tools))
	tools := make([]mcp.Tool, 0, end-start)
	for _, t := range h.spec.Tools[start:end] {
		def := t.Def
		if len(def.InputSchema) == 0 {
			def.InputSchema = json.RawMessage(`{"type":"object"}`)
		}
		tools = append(tools, def)
	}
	return mcp.ListToolsResult{Tools: tools, NextCursor: next}
}

func (h *mcpHandler) record(method, name string, args any, auth string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, MCPCall{Method: method, Name: name, Args: args, Authorization: auth})
}

func (h *mcpHandler) callTool(params json.RawMessage, auth string) (any, *mcp.RPCError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &mcp.RPCError{Code: -32602, Message: "invalid params"}
	}
	var args any
	if len(p.Arguments) > 0 {
		_ = json.Unmarshal(p.Arguments, &args)
	}
	h.record("tools/call", p.Name, args, auth)
	for _, t := range h.spec.Tools {
		if t.Def.Name != p.Name {
			continue
		}
		switch {
		case t.RPCError != nil:
			return nil, t.RPCError
		case t.Echo:
			text, _ := json.Marshal(args)
			return mcp.CallToolResult{Content: []mcp.Content{{Type: mcp.ContentTypeText, Text: string(text)}}}, nil
		case t.Cwd:
			wd, _ := os.Getwd()
			return mcp.CallToolResult{Content: []mcp.Content{{Type: mcp.ContentTypeText, Text: filepath.Base(wd)}}}, nil
		}
		return t.Result, nil
	}
	return nil, &mcp.RPCError{Code: -32602, Message: "unknown tool: " + p.Name}
}

func (h *mcpHandler) listResources(params json.RawMessage) any {
	start, end, next := h.page(params, len(h.spec.Resources))
	list := make([]mcp.Resource, 0, end-start)
	for _, r := range h.spec.Resources[start:end] {
		list = append(list, r.Resource)
	}
	return mcp.ListResourcesResult{Resources: list, NextCursor: next}
}

func (h *mcpHandler) readResource(params json.RawMessage, auth string) (any, *mcp.RPCError) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &mcp.RPCError{Code: -32602, Message: "invalid params"}
	}
	h.record("resources/read", p.URI, nil, auth)
	for _, r := range h.spec.Resources {
		if r.Resource.URI != p.URI {
			continue
		}
		c := mcp.ResourceContents{URI: r.Resource.URI, MimeType: r.Resource.MimeType, Text: r.Text}
		if r.Blob != "" {
			c.Text, c.Blob = "", &r.Blob
		}
		return mcp.ReadResourceResult{Contents: []mcp.ResourceContents{c}}, nil
	}
	return nil, &mcp.RPCError{Code: -32002, Message: "resource not found: " + p.URI}
}

// ServeMCPStdio serves spec over newline-delimited JSON-RPC until r ends.
func ServeMCPStdio(r io.Reader, w io.Writer, spec MCPSpec) error {
	h := &mcpHandler{spec: spec}
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if resp := h.handle(line, ""); resp != nil {
				if _, werr := w.Write(append(resp, '\n')); werr != nil {
					return werr
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// MCPServer is a fake MCP server over Streamable HTTP. Like a real server
// it requires an Accept header that names both response media types, and,
// after initialize, the Mcp-Session-Id it issued, the Mcp-Protocol-Version
// it negotiated, and a prior notifications/initialized.
type MCPServer struct {
	srv *httptest.Server
	h   *mcpHandler

	mu       sync.Mutex
	down     bool
	failInit int
	sse      bool
	wantAuth string
	ready    bool
}

// NewMCPServer starts a server that serves spec until t ends.
func NewMCPServer(t testing.TB, spec MCPSpec) *MCPServer {
	t.Helper()
	s := &MCPServer{h: &mcpHandler{spec: spec}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the MCP endpoint.
func (s *MCPServer) URL() string { return s.srv.URL }

// Close stops the listener, so a later request is refused at the socket.
func (s *MCPServer) Close() { s.srv.Close() }

// SetAvailable makes every request fail with HTTP 503 while up is false.
func (s *MCPServer) SetAvailable(up bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = !up
}

// FailInitialize makes the next n initialize requests fail with HTTP 503.
func (s *MCPServer) FailInitialize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failInit = n
}

// UseSSE answers each request as a text/event-stream instead of JSON.
func (s *MCPServer) UseSSE() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sse = true
}

// RequireAuthorization rejects a request whose Authorization header is not v.
func (s *MCPServer) RequireAuthorization(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wantAuth = v
}

// Calls returns every tools/call and resources/read received, in order.
func (s *MCPServer) Calls() []MCPCall {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	return append([]MCPCall(nil), s.h.calls...)
}

func (s *MCPServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req mcpRequest
	_ = json.Unmarshal(body, &req)
	if status, msg := s.reject(r, req.Method); status != 0 {
		http.Error(w, msg, status)
		return
	}
	resp := s.h.handle(body, r.Header.Get("Authorization"))
	if req.Method == "initialize" {
		w.Header().Set("Mcp-Session-Id", mcpSessionID)
	}
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	s.mu.Lock()
	sse := s.sse
	s.mu.Unlock()
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// reject returns the HTTP status and message that refuse a request, or a
// zero status when the request is served.
func (s *MCPServer) reject(r *http.Request, method string) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return http.StatusServiceUnavailable, "harnesstest: server unavailable"
	}
	if s.wantAuth != "" && r.Header.Get("Authorization") != s.wantAuth {
		return http.StatusUnauthorized, "harnesstest: bad authorization"
	}
	if !acceptsMCPResponses(r.Header.Get("Accept")) {
		return http.StatusNotAcceptable, "harnesstest: Accept must name application/json and text/event-stream"
	}
	if method == "initialize" {
		if s.failInit > 0 {
			s.failInit--
			return http.StatusServiceUnavailable, "harnesstest: initialize unavailable"
		}
		s.ready = false
		return 0, ""
	}
	if r.Header.Get("Mcp-Session-Id") != mcpSessionID {
		return http.StatusBadRequest, "harnesstest: missing Mcp-Session-Id"
	}
	if r.Header.Get("Mcp-Protocol-Version") != mcp.LatestProtocolVersion {
		return http.StatusBadRequest, "harnesstest: missing Mcp-Protocol-Version"
	}
	if method == "notifications/initialized" {
		s.ready = true
	} else if !s.ready {
		return http.StatusBadRequest, "harnesstest: request before notifications/initialized"
	}
	return 0, ""
}

func acceptsMCPResponses(accept string) bool {
	return strings.Contains(accept, "application/json") && strings.Contains(accept, "text/event-stream")
}
