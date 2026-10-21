package external

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/majorcontext/harness/internal/mcp"
	"github.com/majorcontext/harness/protocol"
)

// ToolServer is the name of the MCP server that serves harness tools.
const ToolServer = "harness"

var emptySchema = json.RawMessage(`{"type":"object"}`)

// Group is one MCP server of the endpoint: the specs it lists, each under its
// name without Prefix, the _meta of a listed spec by spec name, and the usage
// guidance that its initialize reply carries.
type Group struct {
	Server       string
	Prefix       string
	Specs        []protocol.ToolSpec
	Meta         map[string]json.RawMessage
	Instructions string
}

// Tools is a loopback MCP endpoint that serves harness tools to an
// external harness for one turn.
type Tools struct {
	// URLs holds the URL of each group by server name. A URL is secret: any
	// local process that knows it can run the tools.
	URLs   map[string]string
	srv    *http.Server
	served chan struct{}
	closed chan struct{}
	// mu orders a call start before the cancel of Close, so that Close
	// waits for every call that started.
	mu     sync.Mutex
	cancel context.CancelFunc
	calls  sync.WaitGroup
}

// ServeTools serves groups until Close, each on its own path of one listener.
// call runs each tool call under ctx, and ends the call when the external
// harness drops its request. call.Name is the spec name, with its Prefix.
// call.ID is the field idMeta of the request _meta: the external harness
// sends its tool use ID there, and a call without it is an error result.
// After ctx ends or the external harness cancels a request, the call gets no
// reply, so the interrupted external harness records it as interrupted.
func ServeTools(ctx context.Context, groups []Group, idMeta string,
	call func(context.Context, protocol.ToolCall) protocol.ToolResult) (*Tools, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	t := &Tools{srv: &http.Server{}, cancel: cancel, URLs: map[string]string{}, served: make(chan struct{}), closed: make(chan struct{})}
	mux := http.NewServeMux()
	base := "/" + rand.Text()
	for i, g := range groups {
		reg := mcp.NewRegistry(g.Server, "")
		reg.SetInstructions(g.Instructions)
		for _, s := range g.Specs {
			reg.RegisterTool(mcp.Tool{Name: strings.TrimPrefix(s.Name, g.Prefix), Description: s.Description, InputSchema: schemaOf(s), Annotations: annotations(s), Meta: g.Meta[s.Name]},
				t.handler(ctx, s.Name, idMeta, call))
		}
		path := base + "/" + strconv.Itoa(i) // a server name may hold pattern or URL syntax
		mux.Handle("POST "+path, reg)
		t.URLs[g.Server] = "http://" + ln.Addr().String() + path
	}
	t.srv.Handler = mux
	go func() {
		defer close(t.served)
		_ = t.srv.Serve(ln)
	}()
	return t, nil
}

func schemaOf(s protocol.ToolSpec) json.RawMessage {
	if len(s.InputSchema) == 0 {
		return emptySchema
	}
	return s.InputSchema
}

// handler runs the calls of the tool name through call.
func (t *Tools) handler(ctx context.Context, name, idMeta string,
	call func(context.Context, protocol.ToolCall) protocol.ToolResult) func(context.Context, json.RawMessage) (mcp.CallToolResult, error) {
	return func(rctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
		id := metaString(mcp.CallMeta(rctx), idMeta)
		if id == "" {
			return mcp.CallToolResult{}, fmt.Errorf("the call has no %q in _meta", idMeta)
		}
		cctx, stop := context.WithCancel(ctx)
		defer stop()
		defer context.AfterFunc(rctx, stop)()
		res, ok := t.run(cctx, call, protocol.ToolCall{ID: id, Name: name, Arguments: args})
		if !ok || cctx.Err() != nil {
			<-t.closed
			return mcp.CallToolResult{}, context.Cause(ctx)
		}
		return mcp.CallToolResult{Content: content(res), IsError: res.IsError}, nil
	}
}

// annotations is the MCP annotations of a tool: readOnlyHint lets the
// external harness run parallel calls of the tool at once.
func annotations(s protocol.ToolSpec) json.RawMessage {
	if !s.ReadOnly {
		return nil
	}
	return json.RawMessage(`{"readOnlyHint":true}`)
}

// content is the MCP content of a tool result: the text, then each blob as
// image, audio or embedded resource content.
func content(res protocol.ToolResult) []mcp.Content {
	var out []mcp.Content
	if res.Text != "" || len(res.Blobs) == 0 {
		out = append(out, mcp.Content{Type: mcp.ContentTypeText, Text: res.Text})
	}
	for _, b := range res.Blobs {
		data := base64.StdEncoding.EncodeToString(b.Data)
		switch {
		case strings.HasPrefix(b.MediaType, "image/"):
			out = append(out, mcp.Content{Type: mcp.ContentTypeImage, Data: data, MimeType: b.MediaType})
		case strings.HasPrefix(b.MediaType, "audio/"):
			out = append(out, mcp.Content{Type: mcp.ContentTypeAudio, Data: data, MimeType: b.MediaType})
		default:
			out = append(out, mcp.Content{Type: mcp.ContentTypeResource,
				Resource: &mcp.EmbeddedResource{URI: "blob:" + b.MediaType, MimeType: b.MediaType, Blob: data}})
		}
	}
	return out
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
