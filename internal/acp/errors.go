package acp

import (
	"errors"

	"github.com/majorcontext/harness/internal/acp/rpc"
	"github.com/majorcontext/harness/protocol"
)

// errNoRuntimes is the error of a request that needs a runtime on a connection that has none.
var errNoRuntimes = errors.New("acp: no runtimes")

// codeNotFound is the JSON-RPC code of a missing resource.
const codeNotFound = -32002

// rpcError maps err to the JSON-RPC error of the Errors table of the ACP
// section. A *rpc.Error passes unchanged. A code that exposes detail keeps the
// message of its protocol error; internal and unreplayable errors, and any
// error that carries no protocol code, answer a fixed message.
func rpcError(err error) error {
	if _, ok := errors.AsType[*rpc.Error](err); ok {
		return err
	}
	code := protocol.CodeOf(err)
	data := map[string]any{"code": string(code)}
	pe, ok := errors.AsType[*protocol.Error](err)
	if ok {
		if cause, ok := pe.Details[protocol.CauseKey]; ok {
			data["cause"] = cause
		}
	}
	if code == protocol.CodeInvalidRequest {
		data["reason"] = protocol.Reason(err)
	}
	e := &rpc.Error{Code: RPCCode(code), Message: "Internal error", Data: data}
	if ok && code != protocol.CodeInternal && code != protocol.CodeSessionUnreplayable {
		e.Message = pe.Message
	}
	if code == protocol.CodeInvalidRequest {
		e.Message = protocol.Reason(err)
	}
	return e
}

// RPCCode returns the JSON-RPC code of a protocol error code, as the Error mapping table of the spec lists it.
func RPCCode(code protocol.Code) int {
	switch code {
	case protocol.CodeInvalidRequest, protocol.CodeInputConflict, protocol.CodeRequestNotPending:
		return rpc.CodeInvalidParams
	case protocol.CodeSessionNotFound, protocol.CodeBlobNotFound:
		return codeNotFound
	}
	return rpc.CodeInternal
}

func invalidParams(reason string) error {
	return &rpc.Error{Code: rpc.CodeInvalidParams, Message: "Invalid params", Data: map[string]string{"reason": reason}}
}
