package harness

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"net/http"

	"github.com/majorcontext/harness/internal/eventlog"
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
	return server.New[withdrawing](reads{r}, server.Options{WorkDir: r.workDir, Processes: procs, Health: r.health, Codes: codes})
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
	{Err: ErrBlobNotFound, Code: protocol.CodeBlobNotFound},
}

// reads is the Runtime with the read route of the server.
type reads struct{ *Runtime }

// Create creates a session as the server runs it.
func (r reads) Create(ctx context.Context, req protocol.CreateSession) (withdrawing, error) {
	s, err := r.Runtime.Create(ctx, req)
	return withdrawing{s}, err
}

// Open opens a session as the server runs it.
func (r reads) Open(ctx context.Context, id string) (withdrawing, error) {
	s, err := r.Runtime.Open(ctx, id)
	return withdrawing{s}, err
}

// withdrawing is a Session with the withdraw route of the server. The Go API
// of Session lists no such method.
type withdrawing struct{ *Session }

// Withdraw removes the queued input inputID. An input that is not queued is
// no error.
func (w withdrawing) Withdraw(ctx context.Context, inputID string) error {
	return w.a.Withdraw(ctx, inputID)
}

// Read returns session id. A session that this runtime runs answers from its
// actor; any other session replays from the store. Nothing acquires it.
func (r reads) Read(ctx context.Context, id string) (server.Reader, error) {
	if err := checkName("session", id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if s := r.running(id); s != nil {
		return live{s}, nil
	}
	v, err := openView(ctx, r.store, id, r.models.Capabilities)
	if err != nil {
		return nil, err
	}
	return stored{v, r.pluginInfo()}, nil
}

// stored is a session that no runtime runs, as a server.Reader. The log does
// not hold the plugins, so it names those of the runtime that serves it, if any.
type stored struct {
	*View
	plugins []protocol.Plugin
}

func (c stored) Session() protocol.Session {
	s := c.View.Session()
	s.Plugins = c.plugins
	return s
}

// Inputs returns the inputs that no turn has taken, oldest first, as of OpenView.
func (c stored) Inputs(context.Context) ([]protocol.QueuedInput, error) {
	return c.log.QueuedInputs(), nil
}

// Blob returns the attachment that a blob part of an input of the session
// names by key. Any other key fails with ErrBlobNotFound.
func (c stored) Blob(ctx context.Context, key string) (server.Blob, error) {
	mediaType, size, ok := c.log.Attachment(key)
	return attachment(ctx, c.st, c.id, key, mediaType, size, ok)
}

func attachment(ctx context.Context, st Store, id, key, mediaType string, size int, ok bool) (server.Blob, error) {
	if !ok {
		return server.Blob{}, fmt.Errorf("%w: %s", ErrBlobNotFound, key)
	}
	body, err := st.GetBlob(ctx, id, key)
	if errors.Is(err, fs.ErrNotExist) {
		return server.Blob{}, fmt.Errorf("%w: %s", ErrBlobNotFound, key)
	}
	return server.Blob{MediaType: mediaType, Size: size, Body: body}, err
}

// Session returns the state of session id.
func (r reads) Session(ctx context.Context, id string) (protocol.Session, error) {
	rd, err := r.Read(ctx, id)
	if err != nil {
		return protocol.Session{}, err
	}
	return rd.Session(), nil
}

// Inputs returns the queued inputs of session id.
func (r reads) Inputs(ctx context.Context, id string) ([]protocol.QueuedInput, error) {
	rd, err := r.Read(ctx, id)
	if err != nil {
		return nil, err
	}
	return rd.Inputs(ctx)
}

// live is a session that this runtime runs, as a server.Reader.
type live struct{ s *Session }

func (l live) Session() protocol.Session { return l.s.View() }

func (l live) Messages(ctx context.Context, before uint64, limit int) (protocol.MessagePage, error) {
	if err := checkMessageLimit(limit); err != nil {
		return protocol.MessagePage{}, err
	}
	var page protocol.MessagePage
	err := l.s.a.Read(ctx, func(s *eventlog.State) { page = s.MessagePage(before, limit) })
	return page, err
}

func (l live) Inputs(ctx context.Context) ([]protocol.QueuedInput, error) {
	var in []protocol.QueuedInput
	err := l.s.a.Read(ctx, func(s *eventlog.State) { in = s.QueuedInputs() })
	return in, err
}

func (l live) Blob(ctx context.Context, key string) (server.Blob, error) {
	var mediaType string
	var size int
	var ok bool
	if err := l.s.a.Read(ctx, func(s *eventlog.State) { mediaType, size, ok = s.Attachment(key) }); err != nil {
		return server.Blob{}, err
	}
	return attachment(ctx, l.s.r.store, l.s.id, key, mediaType, size, ok)
}

func (l live) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error] {
	return l.s.Events(ctx, after)
}
