package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/majorcontext/harness/internal/server"
	"github.com/majorcontext/harness/protocol"
)

// Queue lists inputs that wait outside the log of a session, such as the
// undelivered inputs of a box that is not running.
type Queue interface {
	// Queued returns the waiting inputs of the session, oldest first.
	Queued(ctx context.Context, session string) ([]protocol.QueuedInput, error)
}

// ReadHandler returns the HTTP API of the read routes of Runtime.Handler over st, with no Runtime and no owner.
// The inputs of queue follow the queued inputs of the log in GET /sessions/{id}/inputs and in Session.queued. A
// session with no log but with waiting inputs answers GET /sessions/{id}/inputs from the queue alone; its other
// routes answer session_not_found. A nil queue lists the log only.
func ReadHandler(st Store, queue Queue) http.Handler {
	return server.NewReads(storeReads{st: st, births: &births{}, queue: queue}, server.Options{Codes: codes})
}

// storeReads is a Store and its Queue as the reads of the server.
type storeReads struct {
	st     Store
	births *births
	queue  Queue
}

func (s storeReads) Read(ctx context.Context, id string) (server.Reader, error) {
	if err := checkName("session", id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	v, err := OpenView(ctx, s.st, id)
	if err != nil {
		return nil, err
	}
	return stored{View: v}, nil
}

// Session returns the session with the IDs of the queued inputs of the log and then those of the queue.
func (s storeReads) Session(ctx context.Context, id string) (protocol.Session, error) {
	if err := checkName("session", id); err != nil {
		return protocol.Session{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	rd, err := s.open(ctx, id)
	if err != nil {
		return protocol.Session{}, err
	}
	return rd.Session(), nil
}

func (s storeReads) open(ctx context.Context, id string) (queued, error) {
	v, err := OpenView(ctx, s.st, id)
	if err != nil {
		return queued{}, err
	}
	q, err := s.waiting(ctx, id)
	return queued{stored: stored{View: v}, waiting: q}, err
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
	if err := checkName("session", id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	rd, err := s.open(ctx, id)
	if !errors.Is(err, ErrSessionNotFound) {
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
	return listSessions(ctx, s.st, s.births, q, func(ctx context.Context, id string) (protocol.Session, error) {
		rd, err := s.open(ctx, id)
		if err != nil {
			return protocol.Session{}, err
		}
		return rd.Session(), nil
	})
}

// queued is a stored session with the inputs of a Queue behind those of its log.
type queued struct {
	stored
	waiting []protocol.QueuedInput
}

func (q queued) Session() protocol.Session {
	s := q.stored.Session()
	for _, in := range q.waiting {
		s.Queued = append(s.Queued, in.ID)
	}
	return s
}

func (q queued) Inputs(ctx context.Context) ([]protocol.QueuedInput, error) {
	in, err := q.stored.Inputs(ctx)
	return append(in, q.waiting...), err
}
