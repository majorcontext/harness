package harness

import (
	"context"
	"fmt"
	"iter"
	"net/http"

	"github.com/majorcontext/harness/internal/server"
	"github.com/majorcontext/harness/protocol"
)

// Handler returns the HTTP API of r. A route that changes a session, and the
// stream of its events, open it through r.Open. A route that only reads a
// session reads it without an owner. It has no authentication; the embedder
// wraps it.
func (r *Runtime) Handler() http.Handler {
	var procs server.Processes
	if r.procs != nil {
		procs = processRoutes{r}
	}
	return server.New[*Session](reads{r}, server.Options{WorkDir: r.workDir, Processes: procs, Codes: codes})
}

// codes maps each sentinel error of the runtime to its wire code.
var codes = []server.Code{
	{Err: ErrInvalidRequest, Code: protocol.CodeInvalidRequest},
	{Err: ErrSessionNotFound, Code: protocol.CodeSessionNotFound},
	{Err: ErrSessionExists, Code: protocol.CodeSessionExists},
	{Err: ErrSessionNotOwned, Code: protocol.CodeSessionNotOwned},
	{Err: ErrInputConflict, Code: protocol.CodeInputConflict},
	{Err: ErrTurnMismatch, Code: protocol.CodeTurnMismatch},
	{Err: ErrSessionBusy, Code: protocol.CodeSessionBusy},
	{Err: ErrRequestNotPending, Code: protocol.CodeRequestNotPending},
	{Err: ErrModelUnavailable, Code: protocol.CodeModelUnavailable},
	{Err: ErrDraining, Code: protocol.CodeDraining},
}

// reads is the Runtime with the read route of the server.
type reads struct{ *Runtime }

// Read returns session id. A session that this runtime runs answers from its
// actor; any other session replays from the store. Nothing acquires it.
func (r reads) Read(ctx context.Context, id string) (server.Reader, error) {
	if err := checkName("session", id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if s := r.running(id); s != nil {
		return live{s}, nil
	}
	v, err := OpenView(ctx, r.store, id)
	if err != nil {
		return nil, err
	}
	return cold{v, r.pluginInfo()}, nil
}

// cold is a session that this runtime does not run, as a server.Reader. The
// log does not hold the plugins, so it names those of this runtime.
type cold struct {
	*View
	plugins []protocol.Plugin
}

func (c cold) Session() protocol.Session {
	s := c.View.Session()
	s.Plugins = c.plugins
	return s
}

// live is a session that this runtime runs, as a server.Reader.
type live struct{ s *Session }

func (l live) Session() protocol.Session { return l.s.View() }

func (l live) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error] {
	return l.s.Events(ctx, after)
}
