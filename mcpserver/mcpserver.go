package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/majorcontext/harness/mcp"
)

// protocolVersion is the ONE MCP protocol revision this server speaks —
// matches mcp.LatestProtocolVersion (package mcp's own client), the
// revision this repository has standardized on, and initialize always
// reports it verbatim (see dispatch's methodInitialize case), regardless
// of whatever protocolVersion a client's own initialize request asks for.
// This is deliberate, not a placeholder: this server implements exactly
// this one revision, so claiming support for a client-requested version it
// does not actually speak would be dishonest, and the transport spec's own
// negotiation contract expects a server to report a version it genuinely
// supports, not to echo the request. The sole intended client (a
// delegated Claude Code CLI turn, see engine/claude_code_backend.go) is
// documented to fall back gracefully when a server reports an older
// revision than it asked for, so pinning this — rather than tracking
// whatever the newest spec revision becomes — is the safer, simpler
// choice for as long as this server's own tool surface has no need for
// anything a newer revision adds.
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
	if !validOrigin(r) {
		// The transport spec's own DNS-rebinding security warning
		// (https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#security-warning)
		// makes Origin validation a MUST: a page loaded from an
		// attacker's own site, opened in a victim's browser, can still
		// issue same-machine requests to a server bound to 127.0.0.1 (the
		// browser resolves and connects; the attacker's page just names
		// the URL) — the Origin header is the one thing that request
		// carries that a same-machine, non-browser caller's does not.
		// See validOrigin's own doc comment for the accept/reject rule.
		w.WriteHeader(http.StatusForbidden)
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

// validOrigin reports whether r's Origin header is safe to serve, per the
// transport spec's DNS-rebinding security warning (see ServeHTTP's own
// comment at its call site). Two cases pass:
//
//   - No Origin header at all. A browser attaches Origin to every
//     fetch/XHR; a plain HTTP client (net/http, or whatever the `claude`
//     CLI's own MCP client uses) ordinarily does not, unless a caller
//     explicitly sets it. This server's sole documented consumer — a
//     delegated Claude Code CLI subprocess calling back over loopback,
//     see engine/claude_code_backend.go's ClaudeCodeConfig.HTTPBaseURL —
//     is exactly that case, so requiring an Origin header at all would
//     break the one real client this server has.
//   - An Origin naming a loopback host (localhost, 127.0.0.1, or ::1),
//     any port. A same-machine tool genuinely running on the user's own
//     box is the only thing that can claim this truthfully; a remote
//     attacker's page cannot forge the browser's own Origin header to a
//     value other than the page's real origin.
//
// Anything else — a parseable Origin naming a non-loopback host, or a
// value that fails to parse as a URL at all — is rejected: exactly the
// shape a cross-origin browser page (the DNS-rebinding attack the spec
// warns about) would send.
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
