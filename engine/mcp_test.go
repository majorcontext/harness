package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/mcp"
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

// TestMCPManagerCallToolTransportErrorNeverLeaksURL is the runtime-call
// sibling of TestMCPManagerCallServerToolRetryingErrorNeverLeaksURL: a
// server that connected FINE and then drops before answering a tools/call
// request yields a real *url.Error (net/http's shape for a failed dial),
// which stringifies as `Post "<full-endpoint-URL>": <cause>` — the same
// secret-in-URL class classifyMCPConnectError guards against on the
// connect surface, but here on client.CallTool's error path in callTool
// (engine/mcp.go). The endpoint URL below carries a fake secret in its
// query string, mirroring a real MCP server auth pattern.
func TestMCPManagerCallToolTransportErrorNeverLeaksURL(t *testing.T) {
	const secret = "SUPERSECRET789"
	srv := &fakeMCPHTTPServer{tools: []fakeMCPTool{
		{name: "echo", content: []map[string]any{textContent("hi")}},
	}}
	hs := httptest.NewServer(http.HandlerFunc(srv.serveHTTP))
	endpoint := hs.URL + "/mcp?token=" + secret

	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: endpoint}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	// Connect succeeds first — the server is healthy at this point.
	mgr.Tools(context.Background())

	// The server now drops: nothing is listening on this port anymore, so
	// the NEXT call's dial fails with a real connection-refused error
	// wrapping the full (secret-bearing) endpoint URL.
	hs.Close()

	_, _, err := mgr.CallServerTool(context.Background(), "svc", "echo", nil)
	if err == nil {
		t.Fatal("want an error once the server has gone away mid-call")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("CallServerTool error = %q, leaked the fake secret", err)
	}
	if strings.Contains(err.Error(), "token=") || strings.Contains(err.Error(), endpoint) {
		t.Fatalf("CallServerTool error = %q, leaked the endpoint URL", err)
	}
	if !strings.Contains(err.Error(), "connection refused") && !strings.Contains(err.Error(), "connection failed") {
		t.Errorf("CallServerTool error = %q, want a classified connection-failure reason", err)
	}
}

// TestMCPManagerCallToolContextCancellationPassesThroughCleanly pins the
// ctx-cancellation decision documented on sanitizeMCPCallError: both HTTP
// and stdio transports return ctx.Err() bare (never wrapped in a
// *url.Error) on cancellation, so there is nothing to sanitize and the
// call error should be exactly context.Canceled — not rewritten into a
// misleading "call failed". This is also inert for turn-abort semantics:
// runToolCall (engine.go) always turns a tool error into an ordinary
// ToolResult regardless of its text, so sanitization here cannot itself
// change abort behavior — the enclosing turn's own abort happens
// separately, the next time Session.Prompt's loop makes a provider request
// on the same cancelled ctx.
//
// The fake handler answers initialize/tools-list immediately (so the
// server connects normally) and blocks only the tools/call request, so the
// call is cancelled genuinely mid-flight rather than never reaching the
// server at all.
func TestMCPManagerCallToolContextCancellationPassesThroughCleanly(t *testing.T) {
	callStarted := make(chan struct{})
	block := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"tools/call"`) {
			close(callStarted)
			<-block
		}
		(&fakeMCPHTTPServer{tools: []fakeMCPTool{
			{name: "echo", content: []map[string]any{textContent("hi")}},
		}}).serveHTTP(w, httptest.NewRequest(r.Method, r.URL.String(), bytes.NewReader(body)))
	})
	hs := httptest.NewServer(handler)
	t.Cleanup(hs.Close)
	// Registered AFTER hs's own t.Cleanup(hs.Close): cleanups run LIFO, so
	// this closes the block channel (releasing the handler goroutine
	// blocked on it) before hs.Close blocks waiting for that goroutine to
	// finish — same ordering rule TestMCPManagerConnectTimeoutFailsOpen
	// documents.
	t.Cleanup(func() { close(block) })

	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: hs.URL}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	mgr.Tools(context.Background()) // connect first, unblocked

	connCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := mgr.CallServerTool(connCtx, "svc", "echo", nil)
		done <- err
	}()
	<-callStarted
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CallServerTool error = %v, want context.Canceled somewhere in its chain (unwrapped by sanitizeMCPCallError, not rewritten)", err)
	}
	// The client wraps ctx.Err() with "mcp: tools/call <name>: %w" (see
	// mcp/client.go's CallTool) before it ever reaches sanitizeMCPCallError
	// — that wrapping is the mcp package's, not a rewrite this fix
	// performs, and it contains no endpoint URL either way. Pin the exact
	// unsanitized text so a future change accidentally rewriting it into a
	// generic "call failed" would be caught here.
	const want = `mcp: tools/call echo: context canceled`
	if err.Error() != want {
		t.Errorf("CallServerTool error text = %q, want %q (no sanitization applied)", err.Error(), want)
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
// exactly once for a server that connects successfully, even across
// repeated Tools()/CallTool() calls — it is cached, not re-attempted every
// call. REWRITTEN for the retry state machine (see
// docs/plans/2026-07-20-mcp-init-resilience.md invariant 3): this is no
// longer "every server connects exactly once, period" (a FAILED server now
// gets a bounded background retry — see
// TestMCPManagerFailedServerRetriesInBackgroundAndRecovers) but narrows to
// "a server that HAS connected is never re-probed," which is what this
// test actually exercises (its one server always succeeds).
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
//
// This race is about the FIRST-attempt/connectOnce interlock specifically
// (see engine/mcp.go's Close doc comment) and is unaffected by the retry
// state machine layered on top of it: the server here connects
// successfully, so no retryServer goroutine is ever spawned for it, and
// this test's assertions hold exactly as before.
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

// TestMCPManagerCloseOfNeverPromptedManagerNeverConnects is the regression
// test for the follow-up finding on PR #51's Close-vs-first-connect race
// fix: Close interlocked with an in-flight connect by calling
// ensureConnected itself, unconditionally — so a harness serve process
// configured with MCP servers that never received a single prompt before
// shutdown would still CONNECT every configured server just to
// immediately close it: up to ConnectTimeout of pure shutdown latency,
// bounded by the wrong timeout entirely (ConnectTimeout, not
// mcpCloseTimeout, since Close was the one *initiating* the connect
// rather than waiting on one already in flight).
//
// Close must never be what initiates a first connect — only ever interlock
// with one already started by a real caller. The server here gates every
// request (including the very first one) on a channel the test never
// releases during the assertions (closed only in Cleanup, per AGENTS.md's
// hang-simulation pattern), with ConnectTimeout set small so a
// wrongly-triggered connect attempt still resolves (as a failure) quickly
// rather than hanging the test — but any request reaching the server at
// all is the bug: fixed code must never even dial out.
//
// It also covers the ordering the fix must additionally guarantee: a
// Tools() call arriving after such a Close must not get to start a late
// connect either, since connectOnce is only ever consumed once.
//
// Like TestMCPManagerCloseWaitsForInFlightConnect, this is about the
// FIRST-attempt interlock and predates the retry state machine; since the
// manager here is closed before any attempt ever resolves, no
// retryServer goroutine is ever spawned (there is nothing to retry) —
// Close's added retryCancel()/retryWG.Wait() steps are both instant no-ops
// in this scenario, so the near-instant elapsed-time assertion below still
// holds.
func TestMCPManagerCloseOfNeverPromptedManagerNeverConnects(t *testing.T) {
	block := make(chan struct{})
	var requests int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		<-block // held open for the whole test; only Cleanup releases it
	})
	hs := httptest.NewServer(handler)
	t.Cleanup(hs.Close)
	// Registered after t.Cleanup(hs.Close): cleanups run LIFO, so this
	// closes the block channel (unblocking any stuck handler) before
	// hs.Close blocks waiting for it to return.
	t.Cleanup(func() { close(block) })

	mgr := NewMCPManager(map[string]MCPServerConfig{
		"svc": {URL: hs.URL, ConnectTimeout: 30 * time.Millisecond},
	})

	start := time.Now()
	if err := mgr.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Close of a never-prompted manager took %s, want near-instant (no connect attempt)", elapsed)
	}
	if n := atomic.LoadInt32(&requests); n != 0 {
		t.Fatalf("server saw %d request(s), want 0: Close of a never-prompted manager must never initiate a connect", n)
	}

	// A Tools() call arriving after such a Close must not start a late
	// connect either — connectOnce was already consumed by Close.
	defs := mgr.Tools(context.Background())
	if len(defs) != 0 {
		t.Errorf("Tools() after Close = %+v, want empty (no late connect)", defs)
	}
	if n := atomic.LoadInt32(&requests); n != 0 {
		t.Fatalf("server saw %d request(s) after a post-Close Tools() call, want 0: Close must permanently prevent any later first connect", n)
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

// # Retry state machine (docs/plans/2026-07-20-mcp-init-resilience.md)
//
// The tests below cover invariants 2-5 and 8 of that plan. Most run inside
// a testing/synctest bubble with mcpConnectFunc swapped for a network-free
// fake — AGENTS.md is explicit that real network I/O does not behave
// deterministically inside a synctest bubble, so a fake keeps the whole
// retry schedule bubble-native (fake timers only) and lets synctest's fake
// clock fast-forward through minutes of backoff instantly. The one
// exception (TestMCPManagerCallServerToolRetryingThenRecovers) drives a
// real mcp.Client round trip end to end and so runs outside a bubble, with
// real (bounded) backoff wall time and the mcpTestRetryCommitted hook for
// synchronization instead of a sleep or a poll loop.

// withMCPConnectFunc swaps mcpConnectFunc for fn for the duration of the
// test and restores the original in Cleanup — the package-var seam tests
// use to run the retry schedule without touching real network (see the
// section doc comment above).
func withMCPConnectFunc(t *testing.T, fn func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error)) {
	t.Helper()
	orig := mcpConnectFunc
	t.Cleanup(func() { mcpConnectFunc = orig })
	mcpConnectFunc = fn
}

// withZeroMCPJitter forces mcpJitterFunc to always return 0, making
// mcpRetryBackoff(attempt) exactly half of mcpRetryDelay(attempt) — the
// same "half fixed, half zeroed jitter" trick goal_test.go's
// TestGoalRetryableBackoffJitter callers use, so a schedule's total elapsed
// time is exactly assertable rather than merely bounded.
func withZeroMCPJitter(t *testing.T) {
	t.Helper()
	orig := mcpJitterFunc
	t.Cleanup(func() { mcpJitterFunc = orig })
	mcpJitterFunc = func(time.Duration) time.Duration { return 0 }
}

// TestMCPRetryDelaySchedule is the pure-function schedule test, mirroring
// goal.go's TestGoalRetryDelaySchedule: ~1s doubling to a 5-minute cap.
func TestMCPRetryDelaySchedule(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 32 * time.Second},
		{7, 64 * time.Second},
		{8, 128 * time.Second},
		{9, 256 * time.Second},
		{10, mcpRetryBackoffCap}, // 512s would exceed the 5-minute cap
		{11, mcpRetryBackoffCap},
	}
	for _, c := range cases {
		if got := mcpRetryDelay(c.attempt); got != c.want {
			t.Errorf("mcpRetryDelay(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

// TestMCPRetryBackoffJitter proves mcpRetryBackoff applies "equal jitter"
// (half the base delay fixed, half randomized in [0, half)) at both ends of
// the random range, via the mcpJitterFunc seam.
func TestMCPRetryBackoffJitter(t *testing.T) {
	orig := mcpJitterFunc
	t.Cleanup(func() { mcpJitterFunc = orig })

	mcpJitterFunc = func(time.Duration) time.Duration { return 0 }
	if got, want := mcpRetryBackoff(1), 500*time.Millisecond; got != want {
		t.Errorf("mcpRetryBackoff(1) with zero jitter = %v, want %v (half of the 1s base)", got, want)
	}

	mcpJitterFunc = func(max time.Duration) time.Duration { return max - 1 }
	if got, want := mcpRetryBackoff(1), 1*time.Second-1; got != want {
		t.Errorf("mcpRetryBackoff(1) with max jitter = %v, want %v (just under the full 1s base)", got, want)
	}
}

// TestMCPManagerFailedServerRetriesInBackgroundAndRecovers is invariant 2's
// headline test, red-verified against pre-Task-1 mcp.go (a throwaway
// httptest-based check proved a recovered server's tools never appear —
// see the commit message). A server whose first connect attempt fails
// gets a background retry on the capped schedule; once one succeeds, the
// tool appears in Tools() with no new session and no explicit trigger.
// The retry loop's commit is observed via the mcpTestRetryCommitted hook
// rather than synctest.Wait(): Wait() only blocks until the bubble reaches
// its NEXT quiescent point, which — for a goroutine sitting in a backoff
// timer wait — is immediately true (a goroutine durably blocked on an
// as-yet-unfired timer already satisfies "durably blocked", so Wait()
// returns before that timer ever fires; time only auto-advances while the
// CALLING goroutine is itself durably blocked too, e.g. on a channel
// receive, letting the bubble's clock fast-forward through the whole
// nested sequence, matching testing/synctest's own canonical example of a
// goroutine's timer resolving inside an enclosing blocked wait).
func TestMCPManagerFailedServerRetriesInBackgroundAndRecovers(t *testing.T) {
	withZeroMCPJitter(t)

	synctest.Test(t, func(t *testing.T) {
		var calls int32
		committed := make(chan bool, 8)
		orig := mcpTestRetryCommitted
		t.Cleanup(func() { mcpTestRetryCommitted = orig })
		mcpTestRetryCommitted = func(server string, connected bool) { committed <- connected }

		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			n := atomic.AddInt32(&calls, 1)
			if n < 4 {
				return nil, nil, errors.New("boom")
			}
			return nil, []mcp.Tool{{Name: "get_forecast", Description: "weather"}}, nil
		})

		mgr := NewMCPManager(map[string]MCPServerConfig{"weather": {URL: "http://unused"}})
		t.Cleanup(func() { _ = mgr.Close(context.Background()) })

		start := time.Now()

		defs := mgr.Tools(context.Background())
		if len(defs) != 0 {
			t.Fatalf("Tools() after the failed first attempt = %+v, want empty", defs)
		}

		// Block until a retry commits Connected == true — this receive is
		// what forces the bubble's fake clock through the whole backoff
		// schedule (two failed retries, then the third succeeding).
		for !<-committed {
		}

		defs = mgr.Tools(context.Background())
		if len(defs) != 1 || defs[0].Name != "mcp__weather__get_forecast" {
			t.Fatalf("Tools() after recovery = %+v, want the recovered server's tool", defs)
		}

		// Attempts 1-3 failed (the first attempt plus two retries);
		// attempt 4 succeeded. Elapsed (fake) time is the sum of the
		// backoff waited after each of the three failures.
		var want time.Duration
		for attempt := 1; attempt <= 3; attempt++ {
			want += mcpRetryDelay(attempt) / 2 // zero jitter above halves each delay
		}
		if elapsed := time.Since(start); elapsed != want {
			t.Errorf("elapsed = %v, want exactly %v (the retry backoff schedule for 3 failed attempts)", elapsed, want)
		}
		if n := atomic.LoadInt32(&calls); n != 4 {
			t.Errorf("connect attempts = %d, want exactly 4 (first + 3 retries, the last succeeding)", n)
		}
	})
}

// TestMCPManagerHealthyServerNeverReprobedWhileSiblingRetries covers
// invariant 3 (a healthy server is initialized exactly once, never
// re-probed) together with invariant 8 (independent per-server retry:
// one server's still-in-progress backoff must not delay another's
// recovery). "good" connects on its first attempt and must never be
// dialed again; "slow" fails twice, is deliberately held mid-attempt by
// the test so its recovery is observably LATER than good's, and only then
// allowed to succeed.
func TestMCPManagerHealthyServerNeverReprobedWhileSiblingRetries(t *testing.T) {
	withZeroMCPJitter(t)

	synctest.Test(t, func(t *testing.T) {
		var goodCalls, slowCalls int32
		gate := make(chan struct{})
		reachedGate := make(chan struct{})

		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			switch name {
			case "good":
				atomic.AddInt32(&goodCalls, 1)
				return nil, []mcp.Tool{{Name: "ping"}}, nil
			case "slow":
				n := atomic.AddInt32(&slowCalls, 1)
				if n == 1 {
					return nil, nil, errors.New("boom") // first attempt fails, spawning the retry
				}
				close(reachedGate) // signal BEFORE blocking, so the test can force fake time through the backoff wait that got us here
				<-gate             // held here until the test releases it
				return nil, []mcp.Tool{{Name: "pong"}}, nil
			default:
				t.Fatalf("unexpected server %q", name)
				return nil, nil, nil
			}
		})

		mgr := NewMCPManager(map[string]MCPServerConfig{
			"good": {URL: "http://unused"},
			"slow": {URL: "http://unused"},
		})
		t.Cleanup(func() { _ = mgr.Close(context.Background()) })

		defs := mgr.Tools(context.Background())
		if len(defs) != 1 || defs[0].Name != "mcp__good__ping" {
			t.Fatalf("Tools() after the first batch = %+v, want only good's tool", defs)
		}

		// Block until slow's retry goroutine has actually crossed its
		// backoff wait and reached its second attempt (now durably blocked
		// on gate) — a receive here, not synctest.Wait(), is what forces
		// the bubble's fake clock through that backoff timer (see the
		// comment on TestMCPManagerFailedServerRetriesInBackgroundAndRecovers).
		<-reachedGate

		defs = mgr.Tools(context.Background())
		if len(defs) != 1 || defs[0].Name != "mcp__good__ping" {
			t.Fatalf("Tools() while slow is still retrying = %+v, want only good's tool (slow must not be up yet)", defs)
		}
		if n := atomic.LoadInt32(&goodCalls); n != 1 {
			t.Errorf("good connect attempts = %d, want exactly 1 (never re-probed while slow keeps retrying)", n)
		}

		close(gate)     // let slow's held attempt complete
		synctest.Wait() // no further timer involved: slow just needs to run its commit and exit

		defs = mgr.Tools(context.Background())
		if len(defs) != 2 {
			t.Fatalf("Tools() after slow recovers = %+v, want both tools", defs)
		}
		if n := atomic.LoadInt32(&goodCalls); n != 1 {
			t.Errorf("good connect attempts = %d, want exactly 1 (still never re-probed)", n)
		}
	})
}

// mcpCommitEvent is one retryServer commit, as observed via the
// mcpTestRetryCommitted hook.
type mcpCommitEvent struct {
	server    string
	connected bool
}

// TestMCPManagerIndependentRetrySchedules is
// docs/plans/2026-07-20-mcp-init-resilience.md's invariant 8's dedicated
// test ("multiple servers failing simultaneously retry independently") —
// not to be confused with docs/plans/2026-07-20-mcp-bounded-retry.md's own,
// differently-numbered invariant 8 ("tool absent when no MCP servers
// configured"), which this test has nothing to do with: two servers that
// BOTH fail their first attempt and are both actively retrying must
// progress on independent schedules — "fast" (succeeds on its 2nd attempt)
// must recover without waiting for "slow" (succeeds only on its 4th, its
// LAST possible attempt now that background retries are bounded at
// mcpRetryMaxAttempts — deliberately at the boundary, see
// docs/plans/2026-07-20-mcp-bounded-retry.md Task 1), and Tools() must
// reflect that partial recovery immediately rather than waiting for every
// retrying server to settle. Adjusted from "5th" (pre-Task-1: retries were
// indefinite, so any attempt count demonstrated independence) down to "4th"
// because a 5th attempt would never fire under the new bound — slow would
// park after its 3rd background retry instead of ever recovering, which
// would defeat this test's own point.
func TestMCPManagerIndependentRetrySchedules(t *testing.T) {
	withZeroMCPJitter(t)

	synctest.Test(t, func(t *testing.T) {
		var fastCalls, slowCalls int32
		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			switch name {
			case "fast":
				if atomic.AddInt32(&fastCalls, 1) < 2 {
					return nil, nil, errors.New("boom")
				}
				return nil, []mcp.Tool{{Name: "go"}}, nil
			case "slow":
				if atomic.AddInt32(&slowCalls, 1) < 4 {
					return nil, nil, errors.New("boom")
				}
				return nil, []mcp.Tool{{Name: "go"}}, nil
			default:
				t.Fatalf("unexpected server %q", name)
				return nil, nil, nil
			}
		})

		committed := make(chan mcpCommitEvent, 32)
		orig := mcpTestRetryCommitted
		t.Cleanup(func() { mcpTestRetryCommitted = orig })
		mcpTestRetryCommitted = func(server string, connected bool) { committed <- mcpCommitEvent{server, connected} }

		mgr := NewMCPManager(map[string]MCPServerConfig{
			"fast": {URL: "http://unused"},
			"slow": {URL: "http://unused"},
		})
		t.Cleanup(func() { _ = mgr.Close(context.Background()) })

		mgr.Tools(context.Background()) // both fail their first attempt, spawning independent retries

		// Drain commits until fast succeeds. If slow succeeds FIRST, the
		// schedules are not actually independent (fast got delayed behind
		// slow's much longer one).
		for {
			ev := <-committed
			if ev.server == "slow" && ev.connected {
				t.Fatal("slow connected before fast — one server's retry blocked the other's")
			}
			if ev.server == "fast" && ev.connected {
				break
			}
		}

		defs := mgr.Tools(context.Background())
		if len(defs) != 1 || defs[0].Name != "mcp__fast__go" {
			t.Fatalf("Tools() right after fast recovers = %+v, want only fast's tool (slow must still be down)", defs)
		}

		for {
			ev := <-committed
			if ev.server == "slow" && ev.connected {
				break
			}
		}

		defs = mgr.Tools(context.Background())
		if len(defs) != 2 {
			t.Fatalf("Tools() after both recover = %+v, want both tools", defs)
		}
	})
}

// TestMCPManagerCallServerToolRetryingThenRecovers is invariant 4's test:
// CallServerTool against a failed-but-retrying server errors with a
// message distinguishing it from an unconfigured server; after a
// background retry succeeds, the identical call succeeds. This drives a
// REAL mcp.Client round trip (not the network-free fake the other tests in
// this section use), so it runs outside a synctest bubble with real
// backoff wall time — synchronized via the mcpTestRetryCommitted hook, not
// a sleep or a poll loop.
func TestMCPManagerCallServerToolRetryingThenRecovers(t *testing.T) {
	var mu sync.Mutex
	requestCount := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		first := requestCount == 1
		mu.Unlock()
		if first {
			// Fail the very first request outright: the initial connect
			// attempt's Initialize call fails immediately, no ConnectTimeout
			// wait needed.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		(&fakeMCPHTTPServer{tools: []fakeMCPTool{
			{name: "echo", content: []map[string]any{textContent("hi")}},
		}}).serveHTTP(w, r)
	})
	hs := httptest.NewServer(handler)
	t.Cleanup(hs.Close)

	committed := make(chan bool, 8)
	orig := mcpTestRetryCommitted
	t.Cleanup(func() { mcpTestRetryCommitted = orig })
	mcpTestRetryCommitted = func(server string, connected bool) {
		if server == "svc" {
			committed <- connected
		}
	}

	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: hs.URL, ConnectTimeout: 500 * time.Millisecond}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	// The first attempt fails synchronously inside Tools() (bounded by
	// ConnectTimeout, but the 500 response returns immediately).
	mgr.Tools(context.Background())

	_, _, err := mgr.CallServerTool(context.Background(), "svc", "echo", nil)
	if err == nil {
		t.Fatal("want an error while the server is still retrying")
	}
	if !strings.Contains(err.Error(), "retrying") {
		t.Errorf("error = %q, want it to mention retrying (distinct from the not-configured message)", err)
	}
	if strings.Contains(err.Error(), "not configured") {
		t.Errorf("error = %q, must not say \"not configured\" for a server that IS configured", err)
	}

	// Block until a background retry actually commits — real backoff wall
	// time (mcpRetryBackoffBase, ~0.5-1s here), not a sleep: this is the
	// production retry goroutine's own real, bounded work completing.
	for !<-committed {
		// keep waiting through any further failed attempts
	}

	out, isErr, err := mgr.CallServerTool(context.Background(), "svc", "echo", nil)
	if err != nil {
		t.Fatalf("CallServerTool after recovery: %v", err)
	}
	if isErr {
		t.Fatal("isErr = true, want false")
	}
	if out.Text() != "hi" {
		t.Errorf("output = %q, want \"hi\"", out.Text())
	}
}

// TestMCPManagerCallServerToolUnconfiguredMessageDistinctFromRetrying pins
// the not-configured-vs-failed error split (invariant 4, engine/mcp.go
// ~284-286 pre-change): a server name that was never configured at all
// gets a different message than one that IS configured but hasn't
// connected yet.
func TestMCPManagerCallServerToolUnconfiguredMessageDistinctFromRetrying(t *testing.T) {
	withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
		return nil, nil, errors.New("boom")
	})

	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: "http://unused"}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	_, _, err := mgr.CallServerTool(context.Background(), "svc", "echo", nil)
	if err == nil || !strings.Contains(err.Error(), "retrying") {
		t.Fatalf("configured-but-failed error = %v, want it to mention retrying", err)
	}

	_, _, err = mgr.CallServerTool(context.Background(), "nope", "echo", nil)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured-server error = %v, want it to say not configured", err)
	}
	if strings.Contains(err.Error(), "retrying") {
		t.Errorf("unconfigured-server error = %v, must not mention retrying", err)
	}
}

// TestMCPManagerCallServerToolRetryingErrorNeverLeaksURL is the should-fix
// finding's headline test for the second model-visible surface (the first
// is mcp_status_test.go's TestMCPStatusSegmentClassifiesReasonNeverLeaksURL):
// CallServerTool's failed-and-retrying error must carry only
// classifyMCPConnectError's classified reason, never the raw connect
// error's text — a *url.Error (the shape net/http returns on a failed
// dial) stringifies as `Post "<full-URL>": <cause>`, and a real HTTP MCP
// server's URL can carry a secret in its path or query.
func TestMCPManagerCallServerToolRetryingErrorNeverLeaksURL(t *testing.T) {
	const secret = "SUPERSECRET123"
	leaky := &url.Error{
		Op:  "Post",
		URL: "https://mcp.example.com/v1?token=" + secret,
		Err: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
		},
	}
	withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
		return nil, nil, leaky
	})

	mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: "http://unused"}})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })

	_, _, err := mgr.CallServerTool(context.Background(), "svc", "echo", nil)
	if err == nil {
		t.Fatal("want an error while the server is still retrying")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("CallServerTool error = %q, leaked the fake secret", err)
	}
	if strings.Contains(err.Error(), "mcp.example.com") {
		t.Fatalf("CallServerTool error = %q, leaked the endpoint host", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("CallServerTool error = %q, want the classified reason %q", err, "connection refused")
	}
	if !strings.Contains(err.Error(), "retrying") {
		t.Errorf("CallServerTool error = %q, want it to still mention retrying", err)
	}
}

// TestMCPManagerCloseDuringBackoffWaitStopsRetryPromptly is invariant 5's
// first scenario: Close while a failed server's retry goroutine is
// durably parked in its backoff wait. Cancellation must end the wait
// immediately rather than riding it out — asserted both directly (elapsed
// FAKE time is exactly zero: no clock advance was needed) and implicitly
// by the synctest bubble itself, which fails the test if the retry
// goroutine is still alive when this function returns (goroutine leak
// detection at bubble exit, per AGENTS.md).
func TestMCPManagerCloseDuringBackoffWaitStopsRetryPromptly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			return nil, nil, errors.New("boom") // never recovers
		})

		mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: "http://unused"}})

		mgr.Tools(context.Background()) // first attempt fails, spawning the retry
		synctest.Wait()                 // let the retry goroutine reach its backoff wait

		start := time.Now()
		if err := mgr.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("Close took %v of fake time, want exactly 0 (cancellation must interrupt the wait, not ride it out)", elapsed)
		}
	})
}

// TestMCPManagerCloseDuringInFlightRetryConnectStopsPromptly is invariant
// 5's second scenario: Close while a retry's connect ATTEMPT itself
// (not the backoff wait between attempts) is in flight. The fake connect
// func here blocks on ctx.Done(), simulating a real cancellable network
// call — Close must cancel that ctx and return promptly rather than
// waiting out the attempt's ConnectTimeout.
func TestMCPManagerCloseDuringInFlightRetryConnectStopsPromptly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int32
		reachedAttempt := make(chan struct{})
		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return nil, nil, errors.New("boom") // first attempt: fail fast, spawn the retry
			}
			close(reachedAttempt) // signal BEFORE blocking, so the test can force fake time through the backoff wait between attempt 1 and this one
			<-ctx.Done()          // the retry's own attempt: block until Close cancels it
			return nil, nil, ctx.Err()
		})

		mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: "http://unused"}})

		mgr.Tools(context.Background())
		// A receive here (not synctest.Wait()) is what forces fake time
		// through the backoff wait between attempt 1's failure and attempt
		// 2 actually starting — see the comment on
		// TestMCPManagerFailedServerRetriesInBackgroundAndRecovers.
		<-reachedAttempt

		start := time.Now()
		if err := mgr.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("Close took %v of fake time, want exactly 0 (cancelling ctx must unblock the in-flight attempt immediately)", elapsed)
		}
	})
}

// TestMCPManagerBackgroundRetryBoundedThenParked is invariant 1's headline
// test (docs/plans/2026-07-20-mcp-bounded-retry.md Task 1), red-verified
// against pre-Task-1 mcp.go: retryServer looped indefinitely, so a server
// whose EVERY attempt fails never stopped retrying and Status() had no
// notion of "gave up" at all (no Parked field existed). A server whose
// first attempt and every subsequent background retry fail gets exactly
// mcpRetryMaxAttempts (3) background attempts — on the jittered 1s/2s/4s
// schedule this asserts exactly via withZeroMCPJitter — before the retry
// goroutine gives up for good: the entry is marked Parked, Status()
// reflects it, and testing/synctest's own goroutine-leak detection at
// bubble exit proves no goroutine is left alive to fire a further attempt
// (if retryServer still looped past the bound, it would be durably parked
// in a future backoff wait when this closure returns, and synctest would
// fail the test for a leaked goroutine).
func TestMCPManagerBackgroundRetryBoundedThenParked(t *testing.T) {
	withZeroMCPJitter(t)

	synctest.Test(t, func(t *testing.T) {
		var calls int32
		committed := make(chan bool, 8)
		orig := mcpTestRetryCommitted
		t.Cleanup(func() { mcpTestRetryCommitted = orig })
		mcpTestRetryCommitted = func(server string, connected bool) { committed <- connected }

		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			atomic.AddInt32(&calls, 1)
			return nil, nil, errors.New("boom") // never recovers
		})

		mgr := NewMCPManager(map[string]MCPServerConfig{"weather": {URL: "http://unused"}})
		t.Cleanup(func() { _ = mgr.Close(context.Background()) })

		start := time.Now()

		defs := mgr.Tools(context.Background()) // first attempt: fails synchronously
		if len(defs) != 0 {
			t.Fatalf("Tools() after the failed first attempt = %+v, want empty", defs)
		}

		// Drain exactly mcpRetryMaxAttempts commits, each a failure — the
		// last one is the giving-up commit. Receiving from committed is
		// what forces the bubble's fake clock through the 1s/2s/4s backoff
		// schedule (see TestMCPManagerFailedServerRetriesInBackgroundAndRecovers's
		// comment on why a receive, not synctest.Wait(), does this).
		for i := 0; i < mcpRetryMaxAttempts; i++ {
			if connected := <-committed; connected {
				t.Fatalf("commit %d reported connected=true, want every background retry to fail", i+1)
			}
		}
		synctest.Wait() // let the goroutine finish returning after its last commit

		var wantElapsed time.Duration
		for attempt := 1; attempt <= mcpRetryMaxAttempts; attempt++ {
			wantElapsed += mcpRetryDelay(attempt) / 2 // zero jitter above halves each delay: 0.5s + 1s + 2s
		}
		if elapsed := time.Since(start); elapsed != wantElapsed {
			t.Errorf("elapsed = %v, want exactly %v (the 1s/2s/4s backoff schedule, halved by zero jitter)", elapsed, wantElapsed)
		}

		wantCalls := int32(1 + mcpRetryMaxAttempts)
		if n := atomic.LoadInt32(&calls); n != wantCalls {
			t.Errorf("connect attempts = %d, want exactly %d (first attempt + %d background retries)", n, wantCalls, mcpRetryMaxAttempts)
		}

		statuses := mgr.Status()
		if len(statuses) != 1 || statuses[0].Name != "weather" {
			t.Fatalf("Status() = %+v, want exactly one entry for weather", statuses)
		}
		st := statuses[0]
		if !st.Parked {
			t.Error("Status().Parked = false, want true once retries are exhausted")
		}
		if st.Connected {
			t.Error("Status().Connected = true, want false")
		}
		if st.Attempts != int(wantCalls) {
			t.Errorf("Status().Attempts = %d, want %d", st.Attempts, wantCalls)
		}

		// Because the closure is about to return, testing/synctest's own
		// leak detection now proves no goroutine is left alive for this
		// server: if retryServer had not actually exited (bound broken),
		// it would be durably blocked in a future backoff wait and this
		// test would fail with a goroutine-leak error, not the assertions
		// above.
	})
}

// TestMCPManagerAttemptsMonotonicAcrossToolAndBackgroundDials pins the fix
// for the non-monotonic Attempts counter a review of the bounded-retry PR
// flagged: retryServer used to commit an ABSOLUTE local counter
// (entry.Attempts = attempt) that had no knowledge of any tool-triggered
// Connect dials interleaved with it, so a sequence like "first attempt
// fails, two tool Connects fail, then the background retry's own second
// dial commits" could make the model-visible Attempts count go DOWN (3 ->
// 2) even though four real dials had happened. retryServer now increments
// the shared field the same way Connect always has (entry.Attempts++), so
// the count can only ever climb by exactly one per commit regardless of
// which path performed the dial.
//
// The four dials here are ordered deterministically by testing/synctest's
// fake-clock rule, not by an explicit gate: after the first attempt fails
// synchronously inside Tools(), the spawned retryServer goroutine reaches
// its backoff timer wait and durably blocks — the bubble's fake clock
// cannot advance past that wait until every goroutine in the bubble
// (including this test's own) is also durably blocked. Because the two
// manual Connect calls below run synchronously on this same goroutine with
// no timer of their own, retryServer's timer is structurally guaranteed to
// still be pending when they dial, so they are always the 2nd and 3rd
// calls — the background retry's own dial can only be the 4th, once this
// goroutine itself blocks (the <-committed receive) and lets fake time
// advance.
func TestMCPManagerAttemptsMonotonicAcrossToolAndBackgroundDials(t *testing.T) {
	withZeroMCPJitter(t)

	synctest.Test(t, func(t *testing.T) {
		var calls int32
		committed := make(chan bool, 8)
		orig := mcpTestRetryCommitted
		t.Cleanup(func() { mcpTestRetryCommitted = orig })
		mcpTestRetryCommitted = func(server string, connected bool) { committed <- connected }

		withMCPConnectFunc(t, func(ctx context.Context, name string, spec MCPServerConfig) (*mcp.Client, []mcp.Tool, error) {
			if atomic.AddInt32(&calls, 1) < 4 {
				return nil, nil, errors.New("boom")
			}
			return nil, []mcp.Tool{{Name: "ping"}}, nil // the background retry's own dial: succeeds
		})

		mgr := NewMCPManager(map[string]MCPServerConfig{"svc": {URL: "http://unused"}})
		t.Cleanup(func() { _ = mgr.Close(context.Background()) })

		mgr.Tools(context.Background()) // call 1: fails synchronously, spawns the background retry
		if got := mgr.Status()[0].Attempts; got != 1 {
			t.Fatalf("Attempts after the first attempt = %d, want 1", got)
		}

		if err := mgr.Connect(context.Background(), "svc"); err == nil {
			t.Fatal("want the first tool Connect (call 2) to fail")
		}
		if got := mgr.Status()[0].Attempts; got != 2 {
			t.Fatalf("Attempts after the first tool Connect = %d, want 2", got)
		}

		if err := mgr.Connect(context.Background(), "svc"); err == nil {
			t.Fatal("want the second tool Connect (call 3) to fail")
		}
		if got := mgr.Status()[0].Attempts; got != 3 {
			t.Fatalf("Attempts after the second tool Connect = %d, want 3 (still climbing, never decreasing)", got)
		}

		// Let fake time advance through the background retry's backoff wait
		// and its own dial (call 4, which succeeds).
		if connected := <-committed; !connected {
			t.Fatal("want the background retry's own commit to succeed")
		}

		st := mgr.Status()[0]
		if !st.Connected {
			t.Fatal("want svc connected via the background retry")
		}
		if st.Attempts != 4 {
			t.Fatalf("Attempts after the background retry commits = %d, want 4 (monotonic +1, not a regression to the retry loop's own local counter)", st.Attempts)
		}
		if n := atomic.LoadInt32(&calls); n != 4 {
			t.Fatalf("connect dials = %d, want exactly 4", n)
		}
	})
}
