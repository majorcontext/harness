package harness

import (
	"context"
	"fmt"
	"net/http"

	"github.com/majorcontext/harness/internal/server"
	"github.com/majorcontext/harness/protocol"
)

var (
	_ Reader = (*Session)(nil)
	_ Reader = (*View)(nil)
)

// Handler returns the HTTP API of r. It maps only the exported methods of
// Runtime and Session. A route that changes a session, and the stream of its
// events, open it through r.Open. A route that only reads a session reads it
// through r.Read, without an owner. It has no authentication; the embedder
// wraps it.
func (r *Runtime) Handler() http.Handler {
	var procs server.Processes
	if r.procs != nil {
		procs = processRoutes{r}
	}
	return server.New[*Session](serving{r}, server.Options{WorkDir: r.workDir, Processes: procs, Health: r.health, Logger: r.log})
}

// Read returns the running Session of id, or a View of its stored log. It
// acquires nothing and appends nothing. A View names the plugins of r.
func (r *Runtime) Read(ctx context.Context, id string) (Reader, error) {
	if err := CheckName(id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if s := r.running(id); s != nil {
		return s, nil
	}
	v, err := openView(ctx, r.store, id, r.models.Capabilities)
	if err != nil {
		return nil, err
	}
	v.plugins = r.pluginInfo()
	return v, nil
}

// serving is the Runtime as the server runs it.
type serving struct{ *Runtime }

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

// asServer is a Reader as a server.Reader.
type asServer struct{ Reader }

func (a asServer) Blob(ctx context.Context, key string) (server.Blob, error) {
	b, err := a.Reader.Blob(ctx, key)
	return server.Blob{MediaType: b.MediaType, Size: b.Size, Body: b.Body}, err
}
