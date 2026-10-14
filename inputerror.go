package harness

import "github.com/majorcontext/harness/protocol"

// invalidInput is the invalid_request error of a refused input: the reason is the message, the input ID is a detail.
func invalidInput(id string, reason error) error {
	code, msg := protocol.CodeInvalidRequest, reason.Error()
	return &protocol.Error{Code: code, Message: msg, Details: map[string]any{"input_id": id}}
}
