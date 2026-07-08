package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"

	"github.com/majorcontext/harness/message"
)

// Hooks holds a plugin's hook implementations. Nil fields are not subscribed
// and never dispatched; Serve derives the manifest hook list from the non-nil
// fields automatically.
//
// Plugin processes stay warm for the session, so module-level caches (token
// TTL caches, compiled matchers, per-session state) are expected and fine.
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

// WorkspaceDir is the harness workspace (project) directory.
func (cl *Client) WorkspaceDir() string { return cl.init.WorkspaceDir }

// Config is this plugin's raw config block from the harness config file.
func (cl *Client) Config() json.RawMessage { return cl.init.Config }

// SessionMessages returns the canonical message history for a session.
func (cl *Client) SessionMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	var resp SessionMessagesResponse
	err := cl.c.call(ctx, methodSessionMessages, &SessionMessagesRequest{SessionID: sessionID}, &resp)
	return resp.Messages, err
}

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

// Generate makes an LLM call through the harness provider layer.
func (cl *Client) Generate(ctx context.Context, req *GenerateRequest) (*message.Message, error) {
	var resp GenerateResponse
	if err := cl.c.call(ctx, methodGenerate, req, &resp); err != nil {
		return nil, err
	}
	return &resp.Message, nil
}

// HTTPClient returns an http.Client that stamps the harness-configured
// headers (InitializeParams.HTTPHeaders) on every request. Plugins should
// use it for all outbound HTTP so attribution headers are never missed.
func (cl *Client) HTTPClient() *http.Client {
	return &http.Client{Transport: &headerTransport{headers: cl.init.HTTPHeaders}}
}

type headerTransport struct {
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.headers) > 0 {
		req = req.Clone(req.Context())
		for k, v := range t.headers {
			if req.Header.Get(k) == "" {
				req.Header.Set(k, v)
			}
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

// Serve runs the plugin: it speaks the protocol on stdin/stdout and blocks
// until the harness shuts the plugin down or the stream closes. Manifest
// hooks, tools, and protocol version are filled in from hooks; only Name
// (and optionally Version) need to be set by the caller.
//
// Log to stderr — stdout belongs to the protocol.
func Serve(m Manifest, hooks *Hooks) error {
	if m.Name == "" {
		return fmt.Errorf("plugin: manifest name is required")
	}
	if hooks == nil {
		hooks = &Hooks{}
	}
	return serve(stdio{}, m, hooks)
}

// stdio adapts the process's stdin/stdout to an io.ReadWriteCloser.
type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return os.Stdout.Close() }

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
