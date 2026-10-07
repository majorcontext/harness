package external

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/majorcontext/harness/internal/mcp"
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
	served chan struct{}
	closed chan struct{}
	// mu orders a call start before the cancel of Close, so that Close
	// waits for every call that started.
	mu     sync.Mutex
	cancel context.CancelFunc
	calls  sync.WaitGroup
}

// ServeTools serves specs until Close. call runs each tool call under ctx.
// call.ID is the field idMeta of the request _meta: the external harness
// sends its tool use ID there, and a call without it is an error result.
// After ctx ends, a call gets no reply, so the interrupted external harness
// records it as interrupted.
func ServeTools(ctx context.Context, specs []protocol.ToolSpec, idMeta string,
	call func(context.Context, protocol.ToolCall) protocol.ToolResult) (*Tools, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	t := &Tools{srv: &http.Server{}, cancel: cancel, served: make(chan struct{}), closed: make(chan struct{})}
	reg := mcp.NewRegistry(ToolServer, "")
	for _, s := range specs {
		schema := s.InputSchema
		if len(schema) == 0 {
			schema = emptySchema
		}
		reg.RegisterTool(mcp.Tool{Name: s.Name, Description: s.Description, InputSchema: schema},
			func(rctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
				id := metaString(mcp.CallMeta(rctx), idMeta)
				if id == "" {
					return mcp.CallToolResult{}, fmt.Errorf("the call has no %q in _meta", idMeta)
				}
				res, ok := t.run(ctx, call, protocol.ToolCall{ID: id, Name: s.Name, Arguments: args})
				if !ok || ctx.Err() != nil {
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
	go func() {
		defer close(t.served)
		_ = t.srv.Serve(ln)
	}()
	return t, nil
}

// run runs c unless ctx ended first.
func (t *Tools) run(ctx context.Context, call func(context.Context, protocol.ToolCall) protocol.ToolResult,
	c protocol.ToolCall) (protocol.ToolResult, bool) {
	t.mu.Lock()
	if ctx.Err() != nil {
		t.mu.Unlock()
		return protocol.ToolResult{}, false
	}
	t.calls.Add(1)
	t.mu.Unlock()
	defer t.calls.Done()
	return call(ctx, c), true
}

// Close ends each running tool call and waits for it to return. It drops
// every connection first, so a stopped call never gets a reply.
func (t *Tools) Close() {
	t.mu.Lock()
	t.cancel()
	t.mu.Unlock()
	_ = t.srv.Close()
	close(t.closed)
	t.calls.Wait()
	<-t.served
}

func metaString(meta json.RawMessage, key string) string {
	var m map[string]json.RawMessage
	var s string
	if json.Unmarshal(meta, &m) == nil {
		_ = json.Unmarshal(m[key], &s)
	}
	return s
}
