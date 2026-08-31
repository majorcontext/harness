package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/majorcontext/harness/mcp"
)

// protocolVersion is the MCP protocol revision this server speaks —
// matches mcp.LatestProtocolVersion (package mcp's own client), the
// revision this repository has standardized on. A future client
// negotiating an older, still-supported revision is answered with ITS OWN
// requested version instead (see handleInitialize) — this server has no
// revision-specific behavior of its own to lose by echoing back whatever
// the client asked for.
const protocolVersion = "2025-11-25"

// JSON-RPC 2.0 method and error-code constants — mirrors
// mcp/protocol.go's identically-named, unexported constants for the
// client role. Duplicated rather than imported: those are unexported
// package mcp internals, and importing package mcp is only for its
// EXPORTED wire types (Implementation, Tool, CallToolResult, ...) — see
// this package's own doc comment.
const (
	methodInitialize        = "initialize"
	methodToolsList         = "tools/list"
	methodToolsCall         = "tools/call"
	notificationInitialized = "notifications/initialized"
	notificationCancelled   = "notifications/cancelled"
	codeParseError          = -32700
	codeInvalidRequest      = -32600
	codeMethodNotFound      = -32601
	codeInvalidParams       = -32602
	codeInternalError       = -32603
)

// rpcMessage is the JSON-RPC 2.0 envelope this server reads and writes —
// the server-role mirror of mcp/protocol.go's unexported "message" type.
// ID is raw JSON (not a fixed Go type) because JSON-RPC permits either a
// string or a number, and a response must echo a request's ID verbatim,
// whichever shape it came in as.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcp.RPCError   `json:"error,omitempty"`
}

func (m rpcMessage) isNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// callToolParams is this server's own tools/call request-payload shape —
// the server-role mirror of mcp/types.go's identically-shaped, unexported
// client-role callToolParams. initialize's own request body is never
// decoded at all (see the methodInitialize case below) and tools/list
// takes no params this server reads (no pagination — see dispatch's own
// methodToolsList case), so neither gets a params struct of its own.
type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolHandler executes one tools/call request for a registered tool. args
// is the request's raw "arguments" object — nil when the caller sent none
// at all, a zero-length-but-valid object ("{}") when it sent an empty one.
//
// Returning an error produces a CallToolResult with IsError set and the
// error's own message as its sole text content item — a TOOL-level
// failure (see mcp.CallToolResult's own doc comment for how this differs
// from a protocol-level RPCError, which Registry.ServeHTTP reserves for
// things like an unknown tool name) — so a handler never needs to
// construct a CallToolResult by hand just to report its own failure.
type ToolHandler func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error)

// Registry serves the MCP server role's Streamable HTTP transport
// (https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#streamable-http)
// over a fixed, in-process set of tools — see this package's own doc
// comment for the transport shape and scope this implements. The zero
// value is not usable; construct with NewRegistry.
type Registry struct {
	serverInfo   mcp.Implementation
	instructions string

	tools    []mcp.Tool
	handlers map[string]ToolHandler
}

// NewRegistry returns an empty Registry that identifies itself as name at
// version version during initialize (mcp.InitializeResult.ServerInfo).
// RegisterTool adds tools before the first request is served — Registry
// has no locking of its own, so registration must complete before
// ServeHTTP is reachable by any client (every call site in this repo
// registers once, synchronously, right after construction — see
// server/mcp_history.go).
func NewRegistry(name, version string) *Registry {
	return &Registry{
		serverInfo: mcp.Implementation{Name: name, Version: version},
		handlers:   make(map[string]ToolHandler),
	}
}

// SetInstructions sets the free-text guidance returned in
// InitializeResult.Instructions — see that field's own doc comment in
// package mcp. Optional; the zero value (unset) omits the field entirely.
func (reg *Registry) SetInstructions(s string) {
	reg.instructions = s
}

// RegisterTool adds one tool to the registry: tool is advertised verbatim
// by tools/list, and a tools/call naming tool.Name dispatches to handler.
// Registering the same Name twice replaces the earlier tool/handler pair
// (last write wins) rather than duplicating an entry in tools/list.
func (reg *Registry) RegisterTool(tool mcp.Tool, handler ToolHandler) {
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

// ServeHTTP implements the Streamable HTTP transport's single POST
// endpoint. Only POST is accepted (this server issues no session ID for a
// client to DELETE, and opens no independent GET listening stream — see
// this package's own doc comment) — any other method gets 405.
func (reg *Registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var msg rpcMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		reg.writeError(w, nil, codeParseError, fmt.Sprintf("parse error: %v", err))
		return
	}

	if msg.isNotification() {
		// notifications/initialized, notifications/cancelled, or any other
		// notification a future client sends: a JSON-RPC notification
		// gets no response body at all, per the transport spec — this
		// server has nothing to acknowledge or clean up for either one
		// (RegisterTool state is fixed at construction, and a canceled
		// tools/call is not tracked as a separate in-flight operation this
		// stateless server could abort).
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if msg.Method == "" || len(msg.ID) == 0 {
		reg.writeError(w, msg.ID, codeInvalidRequest, "invalid request: missing method or id")
		return
	}

	result, rerr := reg.dispatch(r.Context(), msg.Method, msg.Params)
	if rerr != nil {
		reg.writeError(w, msg.ID, rerr.Code, rerr.Message)
		return
	}
	reg.writeResult(w, msg.ID, result)
}

// dispatch routes one request method to its handler, returning either a
// result to marshal into the response's "result" field or a protocol-level
// *mcp.RPCError (an unknown method or tool name, or malformed params) —
// see ToolHandler's own doc comment for how this differs from a TOOL
// execution failure, which becomes a successful response carrying
// CallToolResult.IsError instead.
func (reg *Registry) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *mcp.RPCError) {
	switch method {
	case methodInitialize:
		// The request body (initializeParams) is intentionally not even
		// decoded: this server implements exactly one protocol revision
		// (protocolVersion) and always reports THAT version, never the
		// client's own requested one. Per the transport spec, a server
		// that does not support the client's requested version responds
		// with a version it DOES support so the client can decide whether
		// to proceed — echoing the client's request back unconditionally
		// would claim support for a revision this server may not actually
		// implement.
		return mcp.InitializeResult{
			ProtocolVersion: protocolVersion,
			Capabilities:    mcp.ServerCapabilities{Tools: &mcp.ToolsCapability{}},
			ServerInfo:      reg.serverInfo,
			Instructions:    reg.instructions,
		}, nil

	case methodToolsList:
		// No pagination: every Registry in this repo holds a small, fixed
		// tool set (see this package's own doc comment), so there is
		// nothing to page through and NextCursor is always left empty.
		return mcp.ListToolsResult{Tools: append([]mcp.Tool(nil), reg.tools...)}, nil

	case methodToolsCall:
		var req callToolParams
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &mcp.RPCError{Code: codeInvalidParams, Message: fmt.Sprintf("invalid tools/call params: %v", err)}
		}
		handler, ok := reg.handlers[req.Name]
		if !ok {
			return nil, &mcp.RPCError{Code: codeInvalidParams, Message: fmt.Sprintf("unknown tool %q", req.Name)}
		}
		res, err := handler(ctx, req.Arguments)
		if err != nil {
			return mcp.CallToolResult{
				Content: []mcp.Content{{Type: mcp.ContentTypeText, Text: err.Error()}},
				IsError: true,
			}, nil
		}
		return res, nil

	default:
		return nil, &mcp.RPCError{Code: codeMethodNotFound, Message: fmt.Sprintf("unknown method %q", method)}
	}
}

func (reg *Registry) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		reg.writeError(w, id, codeInternalError, fmt.Sprintf("encoding result: %v", err))
		return
	}
	body, err := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: id, Result: raw})
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
func (reg *Registry) writeError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	body, err := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: id, Error: &mcp.RPCError{Code: code, Message: message}})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// JSON-RPC errors still ride an HTTP 200: the error is at the JSON-RPC
	// protocol layer, not the HTTP transport layer (the Streamable HTTP
	// spec reserves non-2xx status codes for transport-level failures,
	// e.g. an unrecognized session ID) — mirrors package mcp's own client,
	// which reads msg.Error regardless of a 200 status.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
