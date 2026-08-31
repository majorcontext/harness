package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/majorcontext/harness/mcp"
)

// rpcResponse decodes one JSON-RPC 2.0 response body for assertions below.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *mcp.RPCError   `json:"error"`
}

func post(t *testing.T, reg *Registry, method string, id string, params any) (int, rpcResponse) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != "" {
		body["id"] = id
	}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshaling request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	reg.ServeHTTP(rec, req)
	if id == "" {
		return rec.Code, rpcResponse{}
	}
	var resp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response %s: %v", rec.Body.String(), err)
	}
	return rec.Code, resp
}

// TestRegistryInitializeReturnsCapabilities proves initialize answers with
// the server's own identity and a tools capability — the minimum an MCP
// client needs to know it can proceed to tools/list.
func TestRegistryInitializeReturnsCapabilities(t *testing.T) {
	reg := NewRegistry("test-server", "1.2.3")
	code, resp := post(t, reg, methodInitialize, "1", map[string]any{"protocolVersion": "2025-11-25"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Error != nil {
		t.Fatalf("initialize returned an error: %+v", resp.Error)
	}
	var result mcp.InitializeResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decoding InitializeResult: %v", err)
	}
	if result.ServerInfo.Name != "test-server" || result.ServerInfo.Version != "1.2.3" {
		t.Errorf("ServerInfo = %+v, want Name test-server, Version 1.2.3", result.ServerInfo)
	}
	if result.Capabilities.Tools == nil {
		t.Error("Capabilities.Tools is nil, want a non-nil tools capability")
	}
	if result.ProtocolVersion != "2025-11-25" {
		t.Errorf("ProtocolVersion = %q, want the client's own requested 2025-11-25 echoed back", result.ProtocolVersion)
	}
}

// TestRegistryToolsListIncludesRegisteredTools proves every RegisterTool
// call is reflected verbatim in tools/list.
func TestRegistryToolsListIncludesRegisteredTools(t *testing.T) {
	reg := NewRegistry("test-server", "1.0.0")
	reg.RegisterTool(mcp.Tool{Name: "get_conversation_history", Description: "reads prior history"}, func(context.Context, json.RawMessage) (mcp.CallToolResult, error) {
		return mcp.CallToolResult{}, nil
	})

	code, resp := post(t, reg, methodToolsList, "1", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	var result mcp.ListToolsResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decoding ListToolsResult: %v", err)
	}
	if len(result.Tools) != 1 || result.Tools[0].Name != "get_conversation_history" {
		t.Errorf("Tools = %+v, want exactly [get_conversation_history]", result.Tools)
	}
}

// TestRegistryToolsCallDispatchesToHandlerWithArguments proves tools/call
// routes to the registered handler by name and hands it the raw arguments
// object byte-for-byte (the concrete get_conversation_history tool's own
// pagination args, e.g., ride this path unmodified).
func TestRegistryToolsCallDispatchesToHandlerWithArguments(t *testing.T) {
	var gotArgs json.RawMessage
	reg := NewRegistry("test-server", "1.0.0")
	reg.RegisterTool(mcp.Tool{Name: "echo"}, func(_ context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
		gotArgs = args
		return mcp.CallToolResult{Content: []mcp.Content{{Type: mcp.ContentTypeText, Text: "echoed"}}}, nil
	})

	code, resp := post(t, reg, methodToolsCall, "1", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"offset": 5, "limit": 10},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Error != nil {
		t.Fatalf("tools/call returned an error: %+v", resp.Error)
	}
	var result mcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decoding CallToolResult: %v", err)
	}
	if result.IsError {
		t.Errorf("CallToolResult.IsError = true, want false")
	}
	if len(result.Content) != 1 || result.Content[0].Text != "echoed" {
		t.Errorf("Content = %+v, want one text item %q", result.Content, "echoed")
	}

	var args struct {
		Offset int `json:"offset"`
		Limit  int `json:"limit"`
	}
	if err := json.Unmarshal(gotArgs, &args); err != nil {
		t.Fatalf("decoding arguments the handler received: %v", err)
	}
	if args.Offset != 5 || args.Limit != 10 {
		t.Errorf("handler received offset=%d limit=%d, want 5 and 10", args.Offset, args.Limit)
	}
}

// TestRegistryToolsCallHandlerErrorBecomesIsErrorResult proves a handler
// error is reported as a successful JSON-RPC response carrying
// CallToolResult.IsError — a TOOL-level failure, distinct from the
// protocol-level RPCError an unknown tool name gets (see the next test).
func TestRegistryToolsCallHandlerErrorBecomesIsErrorResult(t *testing.T) {
	reg := NewRegistry("test-server", "1.0.0")
	reg.RegisterTool(mcp.Tool{Name: "fails"}, func(context.Context, json.RawMessage) (mcp.CallToolResult, error) {
		return mcp.CallToolResult{}, errors.New("boom")
	})

	code, resp := post(t, reg, methodToolsCall, "1", map[string]any{"name": "fails"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Error != nil {
		t.Fatalf("tools/call returned a protocol-level error for a tool-level failure: %+v", resp.Error)
	}
	var result mcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decoding CallToolResult: %v", err)
	}
	if !result.IsError {
		t.Error("CallToolResult.IsError = false, want true")
	}
	if len(result.Content) != 1 || result.Content[0].Text != "boom" {
		t.Errorf("Content = %+v, want one text item %q", result.Content, "boom")
	}
}

// TestRegistryToolsCallUnknownToolReturnsRPCError proves an unregistered
// tool name fails cleanly as a JSON-RPC protocol error, not a panic or a
// silently empty result.
func TestRegistryToolsCallUnknownToolReturnsRPCError(t *testing.T) {
	reg := NewRegistry("test-server", "1.0.0")
	code, resp := post(t, reg, methodToolsCall, "1", map[string]any{"name": "does_not_exist"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (JSON-RPC errors ride HTTP 200)", code)
	}
	if resp.Error == nil {
		t.Fatal("tools/call for an unknown tool returned no error")
	}
	if resp.Error.Code != codeInvalidParams {
		t.Errorf("error code = %d, want %d (invalid params)", resp.Error.Code, codeInvalidParams)
	}
}

// TestRegistryUnknownMethodReturnsRPCError proves an unrecognized
// top-level method fails cleanly as a JSON-RPC "method not found" error.
func TestRegistryUnknownMethodReturnsRPCError(t *testing.T) {
	reg := NewRegistry("test-server", "1.0.0")
	code, resp := post(t, reg, "prompts/list", "1", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Error == nil {
		t.Fatal("unknown method returned no error")
	}
	if resp.Error.Code != codeMethodNotFound {
		t.Errorf("error code = %d, want %d (method not found)", resp.Error.Code, codeMethodNotFound)
	}
}

// TestRegistryNotificationGetsNoResponseBody proves a JSON-RPC
// notification (notifications/initialized, notably — the lifecycle step
// every MCP client sends right after a successful initialize) gets HTTP
// 202 with an empty body, per the Streamable HTTP transport spec, rather
// than a JSON-RPC response no one asked for.
func TestRegistryNotificationGetsNoResponseBody(t *testing.T) {
	reg := NewRegistry("test-server", "1.0.0")
	code, _ := post(t, reg, notificationInitialized, "", nil)
	if code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", code)
	}
}

// TestRegistryRejectsNonPOST proves this server's single endpoint refuses
// any method other than POST — it issues no session ID for a client to
// DELETE and opens no independent GET listening stream.
func TestRegistryRejectsNonPOST(t *testing.T) {
	reg := NewRegistry("test-server", "1.0.0")
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()
	reg.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// TestRegistryRegisterToolReplacesExistingByName proves registering the
// same tool name twice replaces the earlier entry rather than duplicating
// it in tools/list.
func TestRegistryRegisterToolReplacesExistingByName(t *testing.T) {
	reg := NewRegistry("test-server", "1.0.0")
	reg.RegisterTool(mcp.Tool{Name: "t", Description: "first"}, func(context.Context, json.RawMessage) (mcp.CallToolResult, error) {
		return mcp.CallToolResult{}, nil
	})
	reg.RegisterTool(mcp.Tool{Name: "t", Description: "second"}, func(context.Context, json.RawMessage) (mcp.CallToolResult, error) {
		return mcp.CallToolResult{Content: []mcp.Content{{Type: mcp.ContentTypeText, Text: "second handler"}}}, nil
	})

	_, resp := post(t, reg, methodToolsList, "1", nil)
	var listResult mcp.ListToolsResult
	if err := json.Unmarshal(resp.Result, &listResult); err != nil {
		t.Fatalf("decoding ListToolsResult: %v", err)
	}
	if len(listResult.Tools) != 1 || listResult.Tools[0].Description != "second" {
		t.Errorf("Tools = %+v, want exactly one tool with Description \"second\"", listResult.Tools)
	}

	_, callResp := post(t, reg, methodToolsCall, "1", map[string]any{"name": "t"})
	var callResult mcp.CallToolResult
	if err := json.Unmarshal(callResp.Result, &callResult); err != nil {
		t.Fatalf("decoding CallToolResult: %v", err)
	}
	if len(callResult.Content) != 1 || callResult.Content[0].Text != "second handler" {
		t.Errorf("tools/call dispatched to the first handler, not the replacement: %+v", callResult.Content)
	}
}
