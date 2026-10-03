package harness

import (
	"net/http"

	"github.com/majorcontext/harness/internal/server"
	"github.com/majorcontext/harness/protocol"
)

// Handler returns the HTTP API of r. A route that names a session opens it
// through r.Open. It has no authentication; the embedder wraps it.
func (r *Runtime) Handler() http.Handler {
	return server.New[*Session](r, []server.Code{
		{Err: ErrInvalidRequest, Code: protocol.CodeInvalidRequest},
		{Err: ErrSessionNotFound, Code: protocol.CodeSessionNotFound},
		{Err: ErrSessionExists, Code: protocol.CodeSessionExists},
		{Err: ErrSessionNotOwned, Code: protocol.CodeSessionNotOwned},
		{Err: ErrInputConflict, Code: protocol.CodeInputConflict},
		{Err: ErrTurnMismatch, Code: protocol.CodeTurnMismatch},
		{Err: ErrSessionBusy, Code: protocol.CodeSessionBusy},
		{Err: ErrModelUnavailable, Code: protocol.CodeModelUnavailable},
		{Err: ErrDraining, Code: protocol.CodeDraining},
	})
}
