package harnesstest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/majorcontext/harness/mcp"
)

func mcpTestSpec() MCPSpec {
	return MCPSpec{
		Name:         "fake",
		Instructions: "use the tools",
		Tools: []MCPTool{
			{Def: mcp.Tool{Name: "echo"}, Echo: true},
			{Def: mcp.Tool{Name: "boom"}, RPCError: &mcp.RPCError{Code: -32000, Message: "scripted"}},
		},
		Resources: []MCPResource{{Resource: mcp.Resource{URI: "doc://a", Name: "a"}, Text: "alpha"}},
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
			if got := len(srv.Calls()); got != 3 {
				t.Fatalf("recorded %d calls, want 3", got)
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
