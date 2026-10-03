package external

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"

	"github.com/majorcontext/harness/mcp"
	"github.com/majorcontext/harness/mcpserver"
	"github.com/majorcontext/harness/protocol"
)

// ToolServer is the name of the MCP server that serves harness tools.
const ToolServer = "harness"

var emptySchema = json.RawMessage(`{"type":"object"}`)

// Tools is a loopback MCP endpoint that serves harness tools to an
// external harness for one turn.
type Tools struct {
	// URL is secret: any local process that knows it can run the tools.
	URL    string
	srv    *http.Server
	cancel context.CancelFunc
	closed chan struct{}
}

// ServeTools serves specs until Close. call runs each tool call under ctx.
// call.ID is the field idMeta of the request _meta: the external harness
// sends its tool use ID there. After ctx ends, a call gets no reply until
// Close, so the interrupted external harness records it as interrupted.
func ServeTools(ctx context.Context, specs []protocol.ToolSpec, idMeta string,
	call func(context.Context, protocol.ToolCall) protocol.ToolResult) (*Tools, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	t := &Tools{srv: &http.Server{}, cancel: cancel, closed: make(chan struct{})}
	reg := mcpserver.NewRegistry(ToolServer, "")
	for _, s := range specs {
		schema := s.InputSchema
		if len(schema) == 0 {
			schema = emptySchema
		}
		reg.RegisterTool(mcp.Tool{Name: s.Name, Description: s.Description, InputSchema: schema},
			func(rctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
				var res protocol.ToolResult
				if ctx.Err() == nil {
					res = call(ctx, protocol.ToolCall{ID: metaString(mcpserver.CallMeta(rctx), idMeta), Name: s.Name, Arguments: args})
				}
				if ctx.Err() != nil {
					<-t.closed
					return mcp.CallToolResult{}, context.Cause(ctx)
				}
				return mcp.CallToolResult{Content: []mcp.Content{{Type: mcp.ContentTypeText, Text: res.Text}}, IsError: res.IsError}, nil
			})
	}
	path := "/" + rand.Text()
	mux := http.NewServeMux()
	mux.Handle("POST "+path, reg)
	t.URL, t.srv.Handler = "http://"+ln.Addr().String()+path, mux
	go func() { _ = t.srv.Serve(ln) }()
	return t, nil
}

// Close ends each running tool call, waits for it to return, and closes
// the endpoint.
func (t *Tools) Close() {
	t.cancel()
	close(t.closed)
	_ = t.srv.Shutdown(context.Background())
}

func metaString(meta json.RawMessage, key string) string {
	var m map[string]json.RawMessage
	var s string
	if json.Unmarshal(meta, &m) == nil {
		_ = json.Unmarshal(m[key], &s)
	}
	return s
}
