package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/majorcontext/harness/internal/plugin/wire"
	"github.com/majorcontext/harness/internal/turn"
)

type stdio struct {
	io.Reader
	io.Writer
}

func (stdio) Close() error { return os.Stdin.Close() }

// Serve runs p as a plugin process: it reads requests from standard input and
// writes responses to standard output. It returns nil when the host sends
// shutdown, and an error when the input ends first. It cancels running calls
// and returns without waiting for them: the process exit ends them.
func Serve(p *Plugin) error {
	return serveConn(stdio{os.Stdin, os.Stdout}, p, false)
}

// ServeConn serves p over rwc until the host sends shutdown or the stream
// ends, and returns nil after a shutdown. It cancels running calls and waits
// for them to return. A panic in a hook or tool becomes an error response
// "panic: <value>", and the connection stays up.
func ServeConn(rwc io.ReadWriteCloser, p *Plugin) error {
	return serveConn(rwc, p, true)
}

func serveConn(rwc io.ReadWriteCloser, p *Plugin, wait bool) error {
	s := &server{p: p, manifest: p.Manifest(), client: &Client{}, tools: make(map[string]Tool, len(p.Tools))}
	for _, t := range p.Tools {
		s.tools[t.Def.Name] = t
	}
	s.conn = wire.NewConn(rwc, s.handle)
	s.client.conn = s.conn
	err := s.conn.Run()
	if wait {
		s.conn.Wait()
	}
	if s.shutdown.Load() {
		return nil
	}
	return err
}

type server struct {
	p        *Plugin
	manifest Manifest
	tools    map[string]Tool
	client   *Client
	conn     *wire.Conn
	shutdown atomic.Bool
}

// defaultHookTimeout is the deadline of a hook that HookTimeouts does not list.
const defaultHookTimeout = 5 * time.Second

func (s *server) handle(ctx context.Context, method string, params json.RawMessage) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = nil, &wire.Error{Code: wire.CodeInternalError, Message: fmt.Sprintf("panic: %v", r)}
		}
	}()
	if name, ok := strings.CutPrefix(method, wire.HookPrefix); ok && Hook(name) != HookEvent {
		d := defaultHookTimeout
		if ms, ok := s.manifest.HookTimeouts[Hook(name)]; ok {
			d = time.Duration(ms) * time.Millisecond
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	switch method {
	case wire.MethodInitialize:
		var init InitializeParams
		if err := json.Unmarshal(params, &init); err != nil {
			return nil, err
		}
		if init.ProtocolVersion != ProtocolVersion {
			return nil, fmt.Errorf("plugin: protocol version mismatch: harness=%d plugin=%d", init.ProtocolVersion, ProtocolVersion)
		}
		s.client.init.Store(&init)
		return s.manifest, nil
	case wire.MethodShutdown:
		s.shutdown.Store(true)
		_ = s.conn.Close()
		return nil, nil
	case wire.MethodToolExecute:
		return s.executeTool(ctx, params)
	case hookMethod(HookEvent):
		if s.p.Event == nil {
			return nil, nil
		}
		var batch EventBatch
		if err := json.Unmarshal(params, &batch); err != nil {
			return nil, err
		}
		s.p.Event(ctx, s.client, batch.Events)
		return nil, nil
	case hookMethod(HookChatParams):
		return handleHook(ctx, s, s.p.ChatParams, params)
	case hookMethod(HookChatMessage):
		return handleHook(ctx, s, s.p.ChatMessage, params)
	case hookMethod(HookSystemTransform):
		return handleHook(ctx, s, s.p.SystemTransform, params)
	case hookMethod(HookShellEnv):
		return handleHook(ctx, s, s.p.ShellEnv, params)
	case hookMethod(HookToolExecuteBefore):
		if s.p.ToolExecuteBefore == nil {
			return handleHook[ToolExecuteBeforeRequest, ToolExecuteBeforeResponse](ctx, s, nil, params)
		}
		return handleHook(ctx, s, s.p.ToolExecuteBefore.Run, params)
	case hookMethod(HookToolExecuteAfter):
		return handleHook(ctx, s, s.p.ToolExecuteAfter, params)
	}
	return nil, &wire.Error{Code: wire.CodeMethodNotFound, Message: fmt.Sprintf("unknown method %q", method)}
}

func hookMethod(h Hook) string { return wire.HookPrefix + string(h) }

func (s *server) executeTool(ctx context.Context, params json.RawMessage) (any, error) {
	var req ToolExecuteRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, err
	}
	t, ok := s.tools[req.Tool]
	if !ok || t.Execute == nil {
		return nil, &wire.Error{Code: wire.CodeMethodNotFound, Message: fmt.Sprintf("unknown tool %q", req.Tool)}
	}
	out, err := t.Execute(turn.WithSessionID(ctx, req.SessionID), s.client, req.Args)
	if err != nil {
		return &ToolExecuteResponse{Output: []Part{TextPart(err.Error())}, IsError: true}, nil
	}
	if out == nil {
		out = []Part{}
	}
	return &ToolExecuteResponse{Output: out}, nil
}

func handleHook[Req, Resp any](ctx context.Context, s *server, fn func(context.Context, *Client, *Req) (*Resp, error), params json.RawMessage) (any, error) {
	if fn == nil {
		return nil, &wire.Error{Code: wire.CodeMethodNotFound, Message: "hook not subscribed"}
	}
	var req Req
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, err
	}
	resp, err := fn(ctx, s.client, &req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return struct{}{}, nil
	}
	return resp, nil
}
