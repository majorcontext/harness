package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// rpcMessage is a minimal JSON-RPC 2.0 envelope, hand-rolled the same way
// package mcp hand-rolls its own (see mcp/protocol.go) — this test fake
// speaks the wire format directly rather than importing mcp's unexported
// message type.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// fakeMCPTool describes one tool a fakeMCPHTTPServer exposes plus its
// canned tools/call response.
type fakeMCPTool struct {
	name        string
	description string
	content     []map[string]any
	isError     bool
}

// fakeMCPHTTPServer implements just enough of the Streamable HTTP MCP
// transport (single POST endpoint, plain JSON responses, no SSE) to
// exercise engine-side connect/list/call wiring end to end, following the
// same "fake server over the real transport, no scripted client
// internals" approach as mcp/http_test.go's fakeHTTPServer.
type fakeMCPHTTPServer struct {
	tools []fakeMCPTool
	// blockUntil, if non-nil, is closed to unblock every request (used to
	// simulate a hung server that never responds, per AGENTS.md's
	// channel-closed-in-Cleanup pattern for hang simulation).
	blockUntil chan struct{}

	calls []string // tool names actually invoked, in order
}

func (s *fakeMCPHTTPServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (s *fakeMCPHTTPServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if s.blockUntil != nil {
		<-s.blockUntil
	}
	var in rpcMessage
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if in.Method != "" && len(in.ID) == 0 {
		// A notification (e.g. notifications/initialized): no response body.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch in.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fake-mcp-http", "version": "0.0.1"},
		}
	case "tools/list":
		var tools []map[string]any
		for _, tool := range s.tools {
			tools = append(tools, map[string]any{
				"name":        tool.name,
				"description": tool.description,
				"inputSchema": map[string]any{"type": "object"},
			})
		}
		result = map[string]any{"tools": tools}
	case "tools/call":
		var params struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(in.Params, &params)
		s.calls = append(s.calls, params.Name)
		for _, tool := range s.tools {
			if tool.name == params.Name {
				result = map[string]any{"content": tool.content, "isError": tool.isError}
				break
			}
		}
		if result == nil {
			w.Header().Set("Content-Type", "application/json")
			resp := rpcMessage{JSONRPC: "2.0", ID: in.ID, Error: json.RawMessage(`{"code":-32601,"message":"unknown tool"}`)}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	raw, _ := json.Marshal(result)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcMessage{JSONRPC: "2.0", ID: in.ID, Result: raw})
}

func textContent(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}

// TestMCPToolName covers the namespacing convention:
// mcp__<server>__<tool> (the Claude Code convention).
func TestMCPToolName(t *testing.T) {
	got := mcpToolName("weather", "get_forecast")
	want := "mcp__weather__get_forecast"
	if got != want {
		t.Errorf("mcpToolName() = %q, want %q", got, want)
	}
}

func TestMCPManagerRegistersNamespacedTools(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{
		{name: "get_forecast", description: "Get the weather forecast", content: []map[string]any{textContent("sunny")}},
	}}
	url := srv.start(t)

	mgr := NewMCPManager(map[string]MCPServerConfig{
		"weather": {URL: url},
	})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	defs := mgr.Tools(context.Background())
	if len(defs) != 1 {
		t.Fatalf("Tools() = %+v, want 1 entry", defs)
	}
	if defs[0].Name != "mcp__weather__get_forecast" {
		t.Errorf("tool name = %q, want mcp__weather__get_forecast", defs[0].Name)
	}
	if defs[0].Description != "Get the weather forecast" {
		t.Errorf("tool description = %q", defs[0].Description)
	}
}

func TestMCPManagerCallToolRouting(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{
		{name: "get_forecast", content: []map[string]any{textContent("sunny and 75F")}},
	}}
	url := srv.start(t)

	mgr := NewMCPManager(map[string]MCPServerConfig{"weather": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	// Connect first via Tools(), then call.
	mgr.Tools(context.Background())
	out, isErr, err := mgr.CallTool(context.Background(), "mcp__weather__get_forecast", json.RawMessage(`{"city":"nyc"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if isErr {
		t.Fatalf("isErr = true, want false")
	}
	if out.Text() != "sunny and 75F" {
		t.Errorf("output = %q", out.Text())
	}
	if len(srv.calls) != 1 || srv.calls[0] != "get_forecast" {
		t.Errorf("server saw calls = %v, want [get_forecast]", srv.calls)
	}
}

func TestMCPManagerCallToolIsError(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{
		{name: "flaky", content: []map[string]any{textContent("boom: rate limited")}, isError: true},
	}}
	url := srv.start(t)

	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	out, isErr, err := mgr.CallTool(context.Background(), "mcp__svc__flaky", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !isErr {
		t.Fatal("isErr = false, want true")
	}
	if out.Text() != "boom: rate limited" {
		t.Errorf("output = %q", out.Text())
	}
}

func TestMCPManagerCallServerTool(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{
		{name: "echo", content: []map[string]any{textContent("hi")}},
	}}
	url := srv.start(t)

	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	out, isErr, err := mgr.CallServerTool(context.Background(), "svc", "echo", nil)
	if err != nil {
		t.Fatalf("CallServerTool: %v", err)
	}
	if isErr {
		t.Fatal("isErr = true, want false")
	}
	if out.Text() != "hi" {
		t.Errorf("output = %q", out.Text())
	}
}

func TestMCPManagerCallServerToolUnknownServer(t *testing.T) {
	mgr := NewMCPManager(map[string]MCPServerConfig{})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })
	_, _, err := mgr.CallServerTool(context.Background(), "nope", "whatever", nil)
	if err == nil {
		t.Fatal("want an error for an unconfigured server, got nil")
	}
}

// TestMCPManagerConnectionFailureFailsOpen verifies a server that cannot be
// reached at all does not prevent Tools() from returning the other,
// reachable servers' tools — connection/listTools failure for one server
// must not kill anything else (fail-open, matching the plugin crash
// philosophy).
func TestMCPManagerConnectionFailureFailsOpen(t *testing.T) {
	good := &fakeMCPHTTPServer{tools: []fakeMCPTool{{name: "ok", content: []map[string]any{textContent("fine")}}}}
	goodURL := good.start(t)

	mgr := NewMCPManager(map[string]MCPServerConfig{
		"good": {URL: goodURL},
		// Nothing listens here: connection refused immediately.
		"bad": {URL: "http://127.0.0.1:1"},
	})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	defs := mgr.Tools(context.Background())
	if len(defs) != 1 {
		t.Fatalf("Tools() = %+v, want exactly the good server's 1 tool", defs)
	}
	if defs[0].Name != "mcp__good__ok" {
		t.Errorf("tool name = %q", defs[0].Name)
	}

	_, _, err := mgr.CallServerTool(context.Background(), "bad", "whatever", nil)
	if err == nil {
		t.Fatal("want an error calling a tool on a server that failed to connect")
	}
}

// TestMCPManagerConnectTimeoutFailsOpen verifies a server that connects but
// never responds is bounded by ConnectTimeout, per-server, and does not
// block or fail the rest of Tools(). It blocks on a channel closed in
// Cleanup rather than using a real hang, per AGENTS.md's guidance for
// simulating a hung component; ConnectTimeout is set small so the test
// stays fast in real wall-clock time.
func TestMCPManagerConnectTimeoutFailsOpen(t *testing.T) {
	block := make(chan struct{})
	hung := &fakeMCPHTTPServer{blockUntil: block}
	hungURL := hung.start(t)
	// Registered after start(t)'s own t.Cleanup(srv.Close): cleanups run
	// LIFO, so this closes the block channel (unblocking the stuck
	// handler) before srv.Close blocks waiting for it to return.
	t.Cleanup(func() { close(block) })

	good := &fakeMCPHTTPServer{tools: []fakeMCPTool{{name: "ok", content: []map[string]any{textContent("fine")}}}}
	goodURL := good.start(t)

	mgr := NewMCPManager(map[string]MCPServerConfig{
		"hung": {URL: hungURL, ConnectTimeout: 20 * time.Millisecond},
		"good": {URL: goodURL},
	})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	start := time.Now()
	defs := mgr.Tools(context.Background())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Tools() took %s, want bounded by ConnectTimeout", elapsed)
	}
	if len(defs) != 1 || defs[0].Name != "mcp__good__ok" {
		t.Fatalf("Tools() = %+v, want exactly the good server's tool", defs)
	}
}

// TestMCPManagerConnectsOnce verifies the connect-and-list step happens
// exactly once per server even across repeated Tools()/CallTool() calls —
// it is cached, not re-attempted every call.
func TestMCPManagerConnectsOnce(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{{name: "ok", content: []map[string]any{textContent("fine")}}}}
	var listCalls int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if containsMethod(body, "tools/list") {
			listCalls++
		}
		srv.serveHTTP(w, r)
	})
	hs := httptest.NewServer(handler)
	t.Cleanup(hs.Close)

	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: hs.URL}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	mgr.Tools(context.Background())
	mgr.Tools(context.Background())
	mgr.Tools(context.Background())

	if listCalls != 1 {
		t.Errorf("tools/list called %d times, want exactly 1 (connect-once, cached)", listCalls)
	}
}

// TestMCPManagerConnectSurvivesFirstCallerCancellation is the regression
// test for the "first caller's ctx poisons the cached connect forever" bug:
// ensureConnected runs exactly once (sync.Once) and, before the fix,
// threaded the very first caller's ctx straight into connectMCPServer. In
// serve mode that ctx is a per-request context — if the request that
// happens to trigger the first connect is aborted/disconnected during the
// connect window, every server's connect observes an already-canceled
// context, fails immediately, is logged-and-skipped, and (because the
// outcome is cached by sync.Once) is never retried: one transient
// cancellation permanently strips MCP tools from the whole process.
//
// The connect step must be detached from whichever caller happens to
// trigger it — only the per-server ConnectTimeout should bound it — so a
// reachable server still connects successfully even when the first caller
// arrives with an already-canceled context.
func TestMCPManagerConnectSurvivesFirstCallerCancellation(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{
		{name: "get_forecast", content: []map[string]any{textContent("sunny")}},
	}}
	url := srv.start(t)

	mgr := NewMCPManager(map[string]MCPServerConfig{"weather": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before the first caller even arrives

	defs := mgr.Tools(canceledCtx)
	if len(defs) != 1 || defs[0].Name != "mcp__weather__get_forecast" {
		t.Fatalf("Tools(canceled ctx) = %+v, want the weather server's tool still connected", defs)
	}

	// A later, healthy caller must see the same cached result — the
	// connect must not have been permanently poisoned.
	defs2 := mgr.Tools(context.Background())
	if len(defs2) != 1 || defs2[0].Name != "mcp__weather__get_forecast" {
		t.Fatalf("Tools(background ctx) after canceled-ctx first call = %+v", defs2)
	}
}

func containsMethod(body []byte, method string) bool {
	var msg rpcMessage
	if json.Unmarshal(body, &msg) != nil {
		return false
	}
	return msg.Method == method
}

// TestSessionRegistersMCPTools exercises the full engine path: a Session
// configured with an MCP server sees the namespaced tool in its assembled
// request, and a model-issued call to it round-trips through the session
// history as an ordinary tool result.
func TestSessionRegistersMCPTools(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{
		{name: "get_forecast", description: "weather", content: []map[string]any{textContent("cloudy")}},
	}}
	url := srv.start(t)

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse,
			&message.Text{Text: "checking"},
			toolCall("tc1", "mcp__weather__get_forecast", `{"city":"nyc"}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "it's cloudy"}),
	}}

	mgr := NewMCPManager(map[string]MCPServerConfig{"weather": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		MCP:       mgr,
	})

	final, err := s.Prompt(context.Background(), "what's the weather")
	if err != nil {
		t.Fatal(err)
	}
	if final.Parts.Text() != "it's cloudy" {
		t.Errorf("final = %q", final.Parts.Text())
	}

	// The first request's tool list must advertise the namespaced tool.
	var found bool
	for _, td := range prov.requests[0].Tools {
		if td.Name == "mcp__weather__get_forecast" {
			found = true
		}
	}
	if !found {
		t.Errorf("request tools = %+v, want mcp__weather__get_forecast", prov.requests[0].Tools)
	}

	h := s.History()
	tr, ok := h[2].Parts[0].(*message.ToolResult)
	if !ok || tr.CallID != "tc1" || tr.IsError {
		t.Fatalf("tool result = %+v", h[2].Parts[0])
	}
	if tr.Content.Text() != "cloudy" {
		t.Errorf("tool result content = %q", tr.Content.Text())
	}
}

// TestSessionPromptSurvivesMCPServerFailure verifies the session-level
// fail-open guarantee: one configured MCP server that cannot be reached at
// all must not kill the session's Prompt — it simply runs with the other,
// reachable server's tool available and the unreachable one absent.
func TestSessionPromptSurvivesMCPServerFailure(t *testing.T) {
	good := &fakeMCPHTTPServer{tools: []fakeMCPTool{
		{name: "ok", content: []map[string]any{textContent("still works")}},
	}}
	goodURL := good.start(t)

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse,
			&message.Text{Text: "trying"},
			toolCall("tc1", "mcp__good__ok", `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}

	mgr := NewMCPManager(map[string]MCPServerConfig{
		"good": {URL: goodURL},
		"down": {URL: "http://127.0.0.1:1"}, // nothing listens: connection refused
	})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		MCP:       mgr,
	})

	final, err := s.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt failed because of an unreachable MCP server: %v", err)
	}
	if final.Parts.Text() != "done" {
		t.Errorf("final = %q", final.Parts.Text())
	}

	var names []string
	for _, td := range prov.requests[0].Tools {
		names = append(names, td.Name)
	}
	if !containsName(names, "mcp__good__ok") {
		t.Errorf("tools = %v, want mcp__good__ok present", names)
	}
	if containsName(names, "mcp__down__") {
		t.Errorf("tools = %v, want no tool from the unreachable server", names)
	}

	h := s.History()
	tr, ok := h[2].Parts[0].(*message.ToolResult)
	if !ok || tr.IsError {
		t.Fatalf("tool result = %+v, want a successful call to the reachable server's tool", h[2].Parts[0])
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}

// TestSessionMCPCallRoutesThroughSameClients exercises the plugin-facing
// path (Session.MCPCall, used by plugin.ClientAPI.MCPCall) and confirms it
// reaches the exact same connected MCP clients a namespaced tool call would
// — an explicit server+tool call by name, not a namespaced tool-def lookup.
func TestSessionMCPCallRoutesThroughSameClients(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{
		{name: "echo", content: []map[string]any{textContent("plugin says hi")}},
	}}
	url := srv.start(t)

	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: url}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	s := NewSession(Config{
		Providers: provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		MCP:       mgr,
	})

	out, isErr, err := s.MCPCall(context.Background(), "svc", "echo", nil)
	if err != nil {
		t.Fatalf("MCPCall: %v", err)
	}
	if isErr {
		t.Fatal("isErr = true, want false")
	}
	if out.Text() != "plugin says hi" {
		t.Errorf("output = %q", out.Text())
	}
}

func TestSessionMCPCallNoManagerConfigured(t *testing.T) {
	s := NewSession(Config{
		Providers: provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	_, _, err := s.MCPCall(context.Background(), "svc", "echo", nil)
	if err == nil {
		t.Fatal("want an error when no MCP registry is configured, got nil")
	}
}

// TestMCPManagerCloseBounded verifies Close returns promptly even when one
// server would otherwise hang, since each underlying mcp.Client.Close
// already self-bounds (see mcp.Client.Close's doc comment); Close on the
// manager must not add its own unbounded wait on top.
// TestDecodeMCPBase64MalformedLogsWarning covers finding 3 from PR #51's
// review: malformed base64 in an MCP content block used to yield a nil
// Blob.Data completely silently. It must now log a slog warning naming
// the offending server and tool — but never the payload bytes themselves,
// which could be arbitrarily large and are not diagnostic.
func TestDecodeMCPBase64MalformedLogsWarning(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const payload = "not valid base64!!!"
	data := decodeMCPBase64("weather", "get_forecast", payload)
	if data != nil {
		t.Errorf("decodeMCPBase64() = %v, want nil for malformed input", data)
	}

	out := buf.String()
	if !strings.Contains(out, "weather") {
		t.Errorf("log output = %q, want it to name the server %q", out, "weather")
	}
	if !strings.Contains(out, "get_forecast") {
		t.Errorf("log output = %q, want it to name the tool %q", out, "get_forecast")
	}
	if strings.Contains(out, payload) {
		t.Errorf("log output = %q, must never contain the raw payload bytes", out)
	}
}

// TestMCPManagerCloseWaitsForInFlightConnect is the regression test for
// finding 2 from PR #51's review: Close used to read m.clients directly,
// without any interaction with connectOnce, so a Close racing a caller's
// very first Tools()/CallTool() (still connecting) would see the
// zero-value nil clients map — connectMCPServer populates m.clients only
// at the very end of the one-time connect step — return immediately having
// closed nothing, and then the connect would finish moments later and
// populate m.clients with a client nobody will ever close: a leaked
// connection (or, for a stdio server, a leaked child process), and silent,
// since connectOnce never retries or revisits it.
//
// The server here gates its initialize response on a channel the test
// controls, so the connect step is deterministically still in flight (past
// dial, mid-handshake) when Close is invoked; the fake session header lets
// the test observe, via the DELETE the client's Close then issues, whether
// the client that connect is about to create actually got closed.
func TestMCPManagerCloseWaitsForInFlightConnect(t *testing.T) {
	const wantSession = "race-session-1"
	gate := make(chan struct{})
	entered := make(chan struct{})
	var enteredOnce sync.Once

	var mu sync.Mutex
	var deleteSessions []string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleteSessions = append(deleteSessions, r.Header.Get("Mcp-Session-Id"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var in rpcMessage
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if in.Method == "initialize" {
			enteredOnce.Do(func() { close(entered) })
			<-gate // held open until the test has raced Close in
		}
		if in.Method != "" && len(in.ID) == 0 {
			w.WriteHeader(http.StatusAccepted) // notification, e.g. notifications/initialized
			return
		}
		var result any
		switch in.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-mcp-http", "version": "0.0.1"},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{}}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := json.Marshal(result)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", wantSession)
		_ = json.NewEncoder(w).Encode(rpcMessage{JSONRPC: "2.0", ID: in.ID, Result: raw})
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: srv.URL}})

	toolsDone := make(chan struct{})
	go func() {
		mgr.Tools(context.Background())
		close(toolsDone)
	}()

	<-entered // the first connect is in flight, blocked mid-initialize

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- mgr.Close(context.Background())
	}()

	close(gate) // let the racing connect (and Close's interlock) proceed

	<-toolsDone
	if err := <-closeDone; err != nil {
		t.Errorf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deleteSessions) != 1 || deleteSessions[0] != wantSession {
		t.Fatalf("server saw DELETE sessions = %v, want exactly [%q]: Close must close the client the racing first connect created, not leak it", deleteSessions, wantSession)
	}
}

func TestMCPManagerCloseBounded(t *testing.T) {
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{{name: "ok", content: []map[string]any{textContent("fine")}}}}
	url := srv.start(t)

	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: url}})
	mgr.Tools(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mgr.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}
