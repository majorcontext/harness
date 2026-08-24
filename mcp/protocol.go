package mcp

import (
	"encoding/json"
	"fmt"
)

// LatestProtocolVersion is the protocol version this client requests during
// initialization.
const LatestProtocolVersion = "2025-11-25"

// supportedProtocolVersions are the versions this client can speak if a
// server negotiates down to them during initialize.
var supportedProtocolVersions = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

func isSupportedProtocolVersion(v string) bool {
	return supportedProtocolVersions[v]
}

// Method names used by this client. Notification methods live under
// "notifications/" per the spec.
const (
	methodInitialize = "initialize"
	methodToolsList  = "tools/list"
	methodToolsCall  = "tools/call"

	notificationInitialized = "notifications/initialized"
	notificationCancelled   = "notifications/cancelled"
)

// JSON-RPC 2.0 standard error codes
// (https://modelcontextprotocol.io/specification/2025-11-25/basic#error-handling).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// RPCError is a JSON-RPC 2.0 error object, returned from Client methods when
// the server responds with an error.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("mcp: rpc error %d: %s", e.Code, e.Message)
}

// message is a JSON-RPC 2.0 request, notification, or response envelope.
// The ID is kept as raw JSON (rather than a fixed Go type) because the spec
// permits either a string or a number, and — while this client always
// generates its own numeric IDs for outgoing requests — an envelope must
// still be able to represent whatever a peer sends back verbatim.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// isRequest reports whether msg is a request or notification sent to a
// peer (has a Method). isResponse (no Method, has an ID) is the converse
// for envelopes we read.
func (m message) isRequestOrNotification() bool { return m.Method != "" }
func (m message) isNotification() bool          { return m.Method != "" && len(m.ID) == 0 }
func (m message) isResponse() bool              { return m.Method == "" && len(m.ID) != 0 }

func marshalParams(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

func idToken(id json.RawMessage) string {
	return string(id)
}
