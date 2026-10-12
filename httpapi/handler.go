// Package httpapi serves the Go API of a harness Runtime, and of a Store, as HTTP and SSE.
package httpapi

import (
	"context"
	"net/http"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/server"
	"github.com/majorcontext/harness/protocol"
)

// Handler returns the HTTP API of r. It maps only the exported methods of
// Runtime and Session, so the compiler proves that the Go API covers each
// route. A route that changes a session, and the stream of its events, open it
// through r.Open. A route that only reads a session reads it through r.Read,
// without an owner. It has no authentication; the embedder wraps it.
func Handler(r *harness.Runtime) http.Handler {
	opts := server.Options{Processes: processes{r}, Health: r.Health(), Logger: r.Logger()}
	if r.WorkDir() != "" {
		opts.Workspace = r.WorkspaceChanges
	}
	return server.New[served](serving{r}, opts)
}

// serving is the Runtime as the server runs it.
type serving struct{ *harness.Runtime }

func (s serving) Create(ctx context.Context, req protocol.CreateSession) (served, error) {
	sess, err := s.Runtime.Create(ctx, req)
	return served{sess}, err
}

func (s serving) Open(ctx context.Context, id string) (served, error) {
	sess, err := s.Runtime.Open(ctx, id)
	return served{sess}, err
}

func (s serving) Read(ctx context.Context, id string) (server.Reader, error) {
	rd, err := s.Runtime.Read(ctx, id)
	if err != nil {
		return nil, err
	}
	return asServer{rd}, nil
}

func (s serving) Session(ctx context.Context, id string) (protocol.Session, error) {
	rd, err := s.Runtime.Read(ctx, id)
	if err != nil {
		return protocol.Session{}, err
	}
	return rd.Snapshot(), nil
}

func (s serving) Inputs(ctx context.Context, id string) ([]protocol.QueuedInput, error) {
	rd, err := s.Runtime.Read(ctx, id)
	if err != nil {
		return nil, err
	}
	return rd.Inputs(ctx)
}

// served is a Session as the server runs it.
type served struct{ *harness.Session }

func (s served) Admit(ctx context.Context, in protocol.Input) (protocol.Admitted, error) {
	t, err := s.Prompt(ctx, in)
	if err != nil {
		return protocol.Admitted{}, err
	}
	return t.Admitted(), nil
}

// asServer is a Reader as a server.Reader.
type asServer struct{ harness.Reader }

func (a asServer) Blob(ctx context.Context, key string) (server.Blob, error) {
	b, err := a.Reader.Blob(ctx, key)
	return server.Blob{MediaType: b.MediaType, Size: b.Size, Body: b.Body}, err
}

// processes is the box processes of a Runtime as the server runs them.
type processes struct{ r *harness.Runtime }

func (p processes) List() []protocol.ProcessInfo { return p.r.Processes() }

func (p processes) Start(ctx context.Context, name string) (protocol.ProcessStatus, error) {
	return p.r.StartProcess(ctx, name)
}

func (p processes) Stop(ctx context.Context, name string) (protocol.ProcessStatus, error) {
	return p.r.StopProcess(ctx, name)
}

func (p processes) Restart(ctx context.Context, name string) (protocol.ProcessStatus, error) {
	return p.r.RestartProcess(ctx, name)
}

func (p processes) Logs(name string, tail int) (protocol.ProcessLogs, error) {
	return p.r.ProcessLogs(name, tail)
}
