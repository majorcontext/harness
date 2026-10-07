package harnesstest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/majorcontext/harness/internal/mcp"
)

func mcpTestSpec() MCPSpec {
	return MCPSpec{
		Name:         "fake",
		Instructions: "use the tools",
		Tools: []MCPTool{
			{Name: "echo", Echo: true},
			{Name: "boom", RPCError: &MCPError{Code: -32000, Message: "scripted"}},
		},
		Resources: []MCPResource{{URI: "doc://a", Name: "a", Text: "alpha"}},
	}
}

func TestMCPServerSpeaksToTheClient(t *testing.T) {
	for _, sse := range []bool{false, true} {
		name := map[bool]string{false: "json", true: "sse"}[sse]
		t.Run(name, func(t *testing.T) {
			srv := NewMCPServer(t, mcpTestSpec())
			if sse {
				srv.UseSSE()
			}
			ctx := context.Background()
			c, err := mcp.NewClient(&mcp.HTTPTransport{Endpoint: srv.URL()}, mcp.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			init, err := c.Initialize(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if init.Instructions != "use the tools" || c.ServerCapabilities().Resources == nil {
				t.Fatalf("initialize = %+v", init)
			}
			tools, err := c.ListAllTools(ctx)
			if err != nil || len(tools) != 2 {
				t.Fatalf("tools = %v, %v", tools, err)
			}
			res, err := c.CallTool(ctx, "echo", map[string]any{"k": "v"})
			if err != nil || len(res.Content) != 1 || res.Content[0].Text != `{"k":"v"}` {
				t.Fatalf("echo = %+v, %v", res, err)
			}
			var rpc *mcp.RPCError
			if _, err := c.CallTool(ctx, "boom", nil); !errors.As(err, &rpc) || rpc.Message != "scripted" {
				t.Fatalf("boom error = %v", err)
			}
			read, err := c.ReadResource(ctx, "doc://a")
			if err != nil || read.Contents[0].Text != "alpha" {
				t.Fatalf("read = %+v, %v", read, err)
			}
			want := []MCPCall{
				{Method: "tools/call", Name: "echo", Args: map[string]any{"k": "v"}},
				{Method: "tools/call", Name: "boom"},
				{Method: "resources/read", Name: "doc://a"},
			}
			if got := srv.Calls(); !reflect.DeepEqual(got, want) {
				t.Fatalf("calls = %+v, want %+v", got, want)
			}
		})
	}
}

func TestMCPServerRefusesWhenUnavailable(t *testing.T) {
	srv := NewMCPServer(t, mcpTestSpec())
	srv.FailInitialize(1)
	connect := func() error {
		c, err := mcp.NewClient(&mcp.HTTPTransport{Endpoint: srv.URL()}, mcp.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		_, err = c.Initialize(context.Background())
		return err
	}
	if err := connect(); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("first initialize error = %v, want HTTP 503", err)
	}
	if err := connect(); err != nil {
		t.Fatalf("second initialize: %v", err)
	}
	srv.SetAvailable(false)
	if err := connect(); err == nil {
		t.Fatal("initialize succeeded on an unavailable server")
	}
}

func TestServeMCPStdioAnswersRequestsAndSkipsNotifications(t *testing.T) {
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"nope"}` + "\n")
	var out bytes.Buffer
	if err := ServeMCPStdio(in, &out, mcpTestSpec()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d response lines, want 2:\n%s", len(lines), out.String())
	}
	var second struct {
		Error *mcp.RPCError `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil || second.Error == nil || second.Error.Code != -32601 {
		t.Fatalf("second response = %s", lines[1])
	}
}

func TestMCPServerEnforcesProtocolHeaders(t *testing.T) {
	const both = "application/json, text/event-stream"
	post := func(t *testing.T, srv *MCPServer, method string, h map[string]string) int {
		t.Helper()
		id := `"id":1,`
		if strings.HasPrefix(method, "notifications/") {
			id = ""
		}
		body := `{"jsonrpc":"2.0",` + id + `"method":"` + method + `"}`
		req, err := http.NewRequest(http.MethodPost, srv.URL(), strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	session := map[string]string{"Accept": both, "Mcp-Session-Id": mcpSessionID, "Mcp-Protocol-Version": mcp.LatestProtocolVersion}
	without := func(k string) map[string]string {
		h := map[string]string{}
		for hk, hv := range session {
			if hk != k {
				h[hk] = hv
			}
		}
		return h
	}
	tests := []struct {
		name     string
		initDone bool
		method   string
		headers  map[string]string
		want     int
	}{
		{"initialize without Accept", false, "initialize", map[string]string{}, http.StatusNotAcceptable},
		{"initialize with JSON-only Accept", false, "initialize", map[string]string{"Accept": "application/json"}, http.StatusNotAcceptable},
		{"initialize with both media types", false, "initialize", map[string]string{"Accept": both}, http.StatusOK},
		{"tools/list without Accept", true, "tools/list", without("Accept"), http.StatusNotAcceptable},
		{"tools/list without protocol version", true, "tools/list", without("Mcp-Protocol-Version"), http.StatusBadRequest},
		{"tools/list without session", true, "tools/list", without("Mcp-Session-Id"), http.StatusBadRequest},
		{"tools/list before notifications/initialized", false, "tools/list", session, http.StatusBadRequest},
		{"tools/list after notifications/initialized", true, "tools/list", session, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewMCPServer(t, mcpTestSpec())
			if tt.method != "initialize" {
				if got := post(t, srv, "initialize", map[string]string{"Accept": both}); got != http.StatusOK {
					t.Fatalf("setup initialize = %d", got)
				}
			}
			if tt.initDone {
				if got := post(t, srv, "notifications/initialized", session); got != http.StatusOK && got != http.StatusAccepted {
					t.Fatalf("setup notifications/initialized = %d", got)
				}
			}
			if got := post(t, srv, tt.method, tt.headers); got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
		})
	}
}
