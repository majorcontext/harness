package httpapi

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/listing"
	"github.com/majorcontext/harness/internal/server"
	"github.com/majorcontext/harness/protocol"
)

// ReadOptions configures ReadHandler.
type ReadOptions struct {
	// Queue lists inputs that wait outside the log. nil: the log only.
	Queue Queue
	// Logger receives the cause of each error that the handler masks as
	// internal. nil: slog.Default().
	Logger *slog.Logger
}

// Queue lists inputs that wait outside the log of a session, such as the
// undelivered inputs of a box that is not running.
type Queue interface {
	// Queued returns the waiting inputs of the session, oldest first.
	Queued(ctx context.Context, session string) ([]protocol.QueuedInput, error)
}

// ReadHandler returns the HTTP API of the read routes of Handler over st, with no Runtime and no owner.
// The inputs of opts.Queue follow the queued inputs of the log in GET /sessions/{id}/inputs and in Session.queued. A
// session with no log but with waiting inputs answers GET /sessions/{id}/inputs from the queue alone; its other
// routes answer session_not_found. A nil opts.Queue lists the log only.
func ReadHandler(st harness.Store, opts ReadOptions) http.Handler {
	log := cmp.Or(opts.Logger, slog.Default())
	return server.NewReads(storeReads{st: st, lister: &listing.Lister{}, queue: opts.Queue, log: log}, server.Options{Logger: log})
}

// storeReads is a Store and its Queue as the reads of the server.
type storeReads struct {
	st     harness.Store
	lister *listing.Lister
	queue  Queue
	log    *slog.Logger
}

func (s storeReads) Read(ctx context.Context, id string) (server.Reader, error) {
	v, err := harness.OpenView(ctx, s.st, id)
	if err != nil {
		return nil, err
	}
	return asServer{v}, nil
}

// Session returns the session with the IDs of the queued inputs of the log and then those of the queue.
func (s storeReads) Session(ctx context.Context, id string) (protocol.Session, error) {
	rd, err := s.open(ctx, id)
	if err != nil {
		return protocol.Session{}, err
	}
	return rd.Snapshot(), nil
}

func (s storeReads) open(ctx context.Context, id string) (queued, error) {
	v, err := harness.OpenView(ctx, s.st, id)
	if err != nil {
		return queued{}, err
	}
	q, err := s.waiting(ctx, id)
	return queued{View: v, waiting: q}, err
}

func (s storeReads) waiting(ctx context.Context, id string) ([]protocol.QueuedInput, error) {
	if s.queue == nil {
		return nil, nil
	}
	return s.queue.Queued(ctx, id)
}

// Inputs returns the queued inputs of the log and then those of the queue. A session with no log lists those of
// the queue alone.
func (s storeReads) Inputs(ctx context.Context, id string) ([]protocol.QueuedInput, error) {
	rd, err := s.open(ctx, id)
	if !errors.Is(err, harness.ErrSessionNotFound) {
		if err != nil {
			return nil, err
		}
		return rd.Inputs(ctx)
	}
	q, qerr := s.waiting(ctx, id)
	if qerr != nil {
		return nil, qerr
	}
	if len(q) == 0 {
		return nil, err
	}
	return q, nil
}

func (s storeReads) List(ctx context.Context, q protocol.ListSessions) (protocol.SessionPage, error) {
	return s.lister.Page(ctx, listing.Source{Sessions: s.st.Sessions, First: s.first, Describe: s.describe, Invalid: harness.ErrInvalidRequest, Log: s.log}, q)
}

func (s storeReads) describe(ctx context.Context, id string) (protocol.Session, error) {
	rd, err := s.open(ctx, id)
	if err != nil {
		return protocol.Session{}, err
	}
	return rd.Snapshot(), nil
}

func (s storeReads) first(ctx context.Context, id string) ([]byte, error) {
	recs, err := s.st.Read(ctx, id, 0, 1)
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	return recs[0].Data, nil
}

// queued is a stored session with the inputs of a Queue behind those of its log.
type queued struct {
	*harness.View
	waiting []protocol.QueuedInput
}

func (q queued) Snapshot() protocol.Session {
	s := q.View.Snapshot()
	for _, in := range q.waiting {
		s.Queued = append(s.Queued, in.ID)
	}
	return s
}

func (q queued) Inputs(ctx context.Context) ([]protocol.QueuedInput, error) {
	in, err := q.View.Inputs(ctx)
	return append(in, q.waiting...), err
}
