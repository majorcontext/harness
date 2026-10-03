package external_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/majorcontext/harness/internal/backend/external"
	"github.com/majorcontext/harness/protocol"
)

func TestServeToolsRefusesACallWithNoToolUseID(t *testing.T) {
	ran := false
	tools, err := external.ServeTools(context.Background(), []protocol.ToolSpec{{Name: "echo"}}, "id",
		func(context.Context, protocol.ToolCall) protocol.ToolResult {
			ran = true
			return protocol.ToolResult{Text: "ran"}
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tools.Close)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{},"_meta":{"other":"x"}}}`
	resp, err := http.Post(tools.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var msg struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	if ran || !msg.Result.IsError || len(msg.Result.Content) != 1 || !strings.Contains(msg.Result.Content[0].Text, `"id"`) {
		t.Errorf("ran = %v, result = %+v, want an error result that names the id field and no call", ran, msg.Result)
	}
}

func TestCloseDropsAStalledRequest(t *testing.T) {
	tools, err := external.ServeTools(context.Background(), nil, "id", nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(tools.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\nExpect: 100-continue\r\nContent-Length: 100\r\n\r\n{", u.Path)
	if err != nil {
		t.Fatal(err)
	}
	// The server sends 100 Continue when the handler starts to read the body.
	if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || !strings.Contains(line, " 100 ") {
		t.Fatalf("read %q, %v; want 100 Continue", line, err)
	}
	tools.Close()
}
