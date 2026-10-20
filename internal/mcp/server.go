package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
)

// callToolRequest is the tools/call request payload.
type callToolRequest struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

type metaKey struct{}

// CallMeta returns the _meta object of the tools/call request that a
// ToolHandler serves, or nil.
func CallMeta(ctx context.Context) json.RawMessage {
	m, _ := ctx.Value(metaKey{}).(json.RawMessage)
	return m
}

// ToolHandler executes a registered tools/call request. args is nil when the
// request omits arguments.
//
// A returned error becomes an IsError result. Protocol failures use RPCError.
type ToolHandler func(ctx context.Context, args json.RawMessage) (CallToolResult, error)

// Registry serves Streamable HTTP requests for a fixed in-process tool set.
// Construct Registry values with NewRegistry.
type Registry struct {
	serverInfo Implementation
	// instructions is the usage guidance that initialize returns.
	instructions string

	tools    []Tool
	handlers map[string]ToolHandler

	// mu guards inflight, the cancel function of each running request by its
	// JSON-RPC id.
	mu       sync.Mutex
	inflight map[string]context.CancelFunc
}

// NewRegistry returns an empty registry with the given server information.
// Registry has no locking. Register tools before serving requests.
func NewRegistry(name, version string) *Registry {
	return &Registry{
		serverInfo: Implementation{Name: name, Version: version},
		handlers:   make(map[string]ToolHandler),
		inflight:   make(map[string]context.CancelFunc),
	}
}

// SetInstructions sets the instructions that initialize returns. Call it before serving requests.
func (reg *Registry) SetInstructions(s string) { reg.instructions = s }

// RegisterTool adds a tool and its handler. The last registration for a name wins.
func (reg *Registry) RegisterTool(tool Tool, handler ToolHandler) {
	for i, existing := range reg.tools {
		if existing.Name == tool.Name {
			reg.tools[i] = tool
			reg.handlers[tool.Name] = handler
			return
		}
	}
	reg.tools = append(reg.tools, tool)
	reg.handlers[tool.Name] = handler
}

// ServeHTTP implements the Streamable HTTP POST endpoint.
func (reg *Registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !validOrigin(r) {
		// Origin validation prevents DNS-rebinding requests to a loopback server.
		w.WriteHeader(http.StatusForbidden)
		return
	}

	var msg message
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		reg.writeError(w, nil, codeParseError, fmt.Sprintf("parse error: %v", err))
		return
	}

	if msg.isNotification() {
		if msg.Method == notificationCancelled {
			reg.cancel(msg.Params)
		}
		// JSON-RPC notifications have no response body.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if msg.Method == "" || len(msg.ID) == 0 {
		reg.writeError(w, msg.ID, codeInvalidRequest, "invalid request: missing method or id")
		return
	}

	// Draining the body lets the server notice a client that closes the
	// connection while the request runs.
	_, _ = io.Copy(io.Discard, r.Body)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	key := requestKey(msg.ID)
	reg.mu.Lock()
	reg.inflight[key] = cancel
	reg.mu.Unlock()
	defer func() {
		reg.mu.Lock()
		delete(reg.inflight, key)
		reg.mu.Unlock()
	}()
	result, rerr := reg.dispatch(ctx, msg.Method, msg.Params)
	if rerr != nil {
		reg.writeError(w, msg.ID, rerr.Code, rerr.Message)
		return
	}
	reg.writeResult(w, msg.ID, result)
}

// requestKey is the key of a JSON-RPC id in inflight: a string id and a number
// id never share a key, and equal numbers share one however they are spelled.
func requestKey(id json.RawMessage) string {
	var str string
	if json.Unmarshal(id, &str) == nil {
		return "s:" + str
	}
	var num float64
	if json.Unmarshal(id, &num) == nil {
		return "n:" + strconv.FormatFloat(num, 'g', -1, 64)
	}
	return "r:" + string(id)
}

// cancel ends the running request that a notifications/cancelled payload
// names. An unknown or finished request is ignored.
func (reg *Registry) cancel(params json.RawMessage) {
	var p cancelledParams
	if json.Unmarshal(params, &p) != nil || len(p.RequestID) == 0 {
		return
	}
	reg.mu.Lock()
	cancel := reg.inflight[requestKey(p.RequestID)]
	reg.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// validOrigin reports whether Origin is absent or names a loopback host.
// It rejects malformed and cross-origin values to prevent DNS rebinding.
func validOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// dispatch routes one request method to its handler, returning either a
// result to marshal into the response's "result" field or a protocol-level
// *RPCError (an unknown method or tool name, or malformed params) —
// see ToolHandler's own doc comment for how this differs from a TOOL
// execution failure, which becomes a successful response carrying
// CallToolResult.IsError instead.
func (reg *Registry) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *RPCError) {
	switch method {
	case methodInitialize:
		// The request body (initializeParams) is intentionally not even
		// decoded: this server implements exactly one protocol revision
		// (LatestProtocolVersion) and always reports THAT version, never the
		// client's own requested one. Per the transport spec, a server
		// that does not support the client's requested version responds
		// with a version it DOES support so the client can decide whether
		// to proceed — echoing the client's request back unconditionally
		// would claim support for a revision this server may not actually
		// implement.
		return InitializeResult{
			ProtocolVersion: LatestProtocolVersion,
			Capabilities:    ServerCapabilities{Tools: &ToolsCapability{}},
			ServerInfo:      reg.serverInfo,
			Instructions:    reg.instructions,
		}, nil

	case methodToolsList:
		// No pagination: every Registry in this repo holds a small, fixed
		// tool set (see this package's own doc comment), so there is
		// nothing to page through and NextCursor is always left empty.
		// The schema requires an array, also for a Registry with no tools.
		return ListToolsResult{Tools: append([]Tool{}, reg.tools...)}, nil

	case methodToolsCall:
		var req callToolRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: fmt.Sprintf("invalid tools/call params: %v", err)}
		}
		handler, ok := reg.handlers[req.Name]
		if !ok {
			return nil, &RPCError{Code: codeInvalidParams, Message: fmt.Sprintf("unknown tool %q", req.Name)}
		}
		res, err := handler(context.WithValue(ctx, metaKey{}, req.Meta), req.Arguments)
		if err != nil {
			return CallToolResult{
				Content: []Content{{Type: ContentTypeText, Text: err.Error()}},
				IsError: true,
			}, nil
		}
		return res, nil

	default:
		return nil, &RPCError{Code: codeMethodNotFound, Message: fmt.Sprintf("unknown method %q", method)}
	}
}

func (reg *Registry) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		reg.writeError(w, id, codeInternalError, fmt.Sprintf("encoding result: %v", err))
		return
	}
	body, err := json.Marshal(message{JSONRPC: "2.0", ID: id, Result: raw})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeError writes a JSON-RPC error response. A nil id (a request this
// server could not even parse an id out of, e.g. a parse-error body) is
// written as JSON null, per the spec's guidance for that case.
func (reg *Registry) writeError(w http.ResponseWriter, id json.RawMessage, code int, text string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	body, err := json.Marshal(message{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: text}})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// JSON-RPC errors still ride an HTTP 200: the error is at the JSON-RPC
	// protocol layer, not the HTTP transport layer (the Streamable HTTP
	// spec reserves non-2xx status codes for transport-level failures,
	// e.g. an unrecognized session ID); the client reads msg.Error
	// regardless of a 200 status.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
