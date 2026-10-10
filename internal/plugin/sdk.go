package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/majorcontext/harness/internal/message"
)

// Hooks holds a plugin's hook implementations. Nil fields are not subscribed
// and never dispatched; serve derives the manifest hook list from the non-nil
// fields automatically.
//
// Plugin processes stay warm for the session, so module-level caches (token
// TTL caches, compiled matchers, per-session state) are expected and fine.
//
// A hook function must be SAFE FOR CONCURRENT USE. The harness keeps several
// requests in flight on one connection (see PROTOCOL.md, "Concurrency"), and
// this SDK serves each incoming request on its own goroutine, so two calls of
// the same hook — or of two different hooks — can run at the same time. Guard
// any shared cache. A plugin that cannot be made reentrant may serialize its
// own handlers with a mutex; the harness stays correct and is only throttled.
// Tool functions in Tools below follow the same rule.
type Hooks struct {
	Event             func(ctx context.Context, c *Client, events []Event)
	ChatParams        func(ctx context.Context, c *Client, req *ChatParamsRequest) (*ChatParamsResponse, error)
	ChatMessage       func(ctx context.Context, c *Client, req *ChatMessageRequest) (*ChatMessageResponse, error)
	SystemTransform   func(ctx context.Context, c *Client, req *SystemTransformRequest) (*SystemTransformResponse, error)
	ShellEnv          func(ctx context.Context, c *Client, req *ShellEnvRequest) (*ShellEnvResponse, error)
	ToolExecuteBefore func(ctx context.Context, c *Client, req *ToolExecuteBeforeRequest) (*ToolExecuteBeforeResponse, error)
	ToolExecuteAfter  func(ctx context.Context, c *Client, req *ToolExecuteAfterRequest) (*ToolExecuteAfterResponse, error)

	// Tools are plugin-provided tools, added to the model's tool list.
	Tools []Tool
}

// Tool pairs a tool definition with its implementation.
type Tool struct {
	Def     ToolDef
	Execute func(ctx context.Context, c *Client, args json.RawMessage) (message.Parts, error)
}

func (h *Hooks) hookList() []Hook {
	var hooks []Hook
	if h.Event != nil {
		hooks = append(hooks, HookEvent)
	}
	if h.ChatParams != nil {
		hooks = append(hooks, HookChatParams)
	}
	if h.ChatMessage != nil {
		hooks = append(hooks, HookChatMessage)
	}
	if h.SystemTransform != nil {
		hooks = append(hooks, HookSystemTransform)
	}
	if h.ShellEnv != nil {
		hooks = append(hooks, HookShellEnv)
	}
	if h.ToolExecuteBefore != nil {
		hooks = append(hooks, HookToolExecuteBefore)
	}
	if h.ToolExecuteAfter != nil {
		hooks = append(hooks, HookToolExecuteAfter)
	}
	return hooks
}

// Client is the plugin's handle to the harness: the client API plus the
// initialize-time environment. It is valid after initialize and passed to
// every hook invocation.
type Client struct {
	c    *conn
	init InitializeParams
}

// ServeURL is the base URL of this process's `harness serve` HTTP API, or
// "" in `harness run` mode (no HTTP API to reach). See InitializeParams and
// PROTOCOL.md's trust model section.
func (cl *Client) ServeURL() string { return cl.init.ServeURL }

// RunToken authenticates requests to ServeURL — the same bearer token the
// orchestrator holds for this run. Empty whenever ServeURL is empty.
func (cl *Client) RunToken() string { return cl.init.RunToken }

// MCPCall invokes a tool on one of the harness's configured MCP servers.
func (cl *Client) MCPCall(ctx context.Context, server, tool string, args any) (*MCPCallResult, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	var resp MCPCallResult
	if err := cl.c.call(ctx, methodMCPCall, &MCPCallRequest{Server: server, Tool: tool, Args: raw}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// serve is the transport-agnostic core of Serve, factored out for tests.
func serve(rwc io.ReadWriteCloser, m Manifest, hooks *Hooks) error {
	m.ProtocolVersion = ProtocolVersion
	m.Hooks = hooks.hookList()
	m.Tools = nil
	toolsByName := make(map[string]Tool, len(hooks.Tools))
	for _, t := range hooks.Tools {
		m.Tools = append(m.Tools, t.Def)
		toolsByName[t.Def.Name] = t
	}

	client := &Client{}
	s := &server{manifest: m, hooks: hooks, tools: toolsByName, client: client}
	c := newConn(rwc, s.handle)
	client.c = c
	s.conn = c

	err := c.run()
	if s.shutdown.Load() {
		return nil
	}
	return err
}

type server struct {
	manifest Manifest
	hooks    *Hooks
	tools    map[string]Tool
	client   *Client
	conn     *conn
	shutdown atomic.Bool
}

func (s *server) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case methodInitialize:
		var init InitializeParams
		if err := json.Unmarshal(params, &init); err != nil {
			return nil, err
		}
		if init.ProtocolVersion != ProtocolVersion {
			return nil, fmt.Errorf("plugin: protocol version mismatch: harness=%d plugin=%d",
				init.ProtocolVersion, ProtocolVersion)
		}
		s.client.init = init
		return s.manifest, nil

	case methodShutdown:
		s.shutdown.Store(true)
		s.conn.close()
		return nil, nil

	case methodToolExecute:
		var req ToolExecuteRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, err
		}
		t, ok := s.tools[req.Tool]
		if !ok {
			return nil, &rpcError{Code: codeMethodNotFound, Message: fmt.Sprintf("unknown tool %q", req.Tool)}
		}
		out, err := t.Execute(ctx, s.client, req.Args)
		if err != nil {
			// Tool errors go back to the model as error results, not as
			// protocol failures.
			return &ToolExecuteResponse{
				Output:  message.Parts{&message.Text{Text: err.Error()}},
				IsError: true,
			}, nil
		}
		return &ToolExecuteResponse{Output: out}, nil

	case HookEvent.method():
		if s.hooks.Event == nil {
			return nil, nil
		}
		var batch EventBatch
		if err := json.Unmarshal(params, &batch); err != nil {
			return nil, err
		}
		s.hooks.Event(ctx, s.client, batch.Events)
		return nil, nil

	case HookChatParams.method():
		return handleHook(ctx, s, s.hooks.ChatParams, params)
	case HookChatMessage.method():
		return handleHook(ctx, s, s.hooks.ChatMessage, params)
	case HookSystemTransform.method():
		return handleHook(ctx, s, s.hooks.SystemTransform, params)
	case HookShellEnv.method():
		return handleHook(ctx, s, s.hooks.ShellEnv, params)
	case HookToolExecuteBefore.method():
		return handleHook(ctx, s, s.hooks.ToolExecuteBefore, params)
	case HookToolExecuteAfter.method():
		return handleHook(ctx, s, s.hooks.ToolExecuteAfter, params)

	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: fmt.Sprintf("unknown method %q", method)}
	}
}

func handleHook[Req, Resp any](ctx context.Context, s *server, fn func(context.Context, *Client, *Req) (*Resp, error), params json.RawMessage) (any, error) {
	if fn == nil {
		return nil, &rpcError{Code: codeMethodNotFound, Message: "hook not subscribed"}
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
		// A nil response means "no changes"; send an empty object.
		return struct{}{}, nil
	}
	return resp, nil
}
