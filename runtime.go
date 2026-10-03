package harness

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

var (
	// ErrInvalidRequest reports a malformed request.
	ErrInvalidRequest = errors.New("harness: invalid request")
	// ErrSessionNotFound reports a session with no log.
	ErrSessionNotFound = session.ErrNotFound
	// ErrSessionExists reports a Create with the ID of an existing session.
	ErrSessionExists = session.ErrExists
	// ErrSessionNotOwned reports a session that this runtime does not run.
	ErrSessionNotOwned = session.ErrNotOwned
	// ErrInputConflict reports a repeated input ID with another body.
	ErrInputConflict = session.ErrInputConflict
	// ErrTurnMismatch reports a turn ID that is not the running turn.
	ErrTurnMismatch = session.ErrTurnMismatch
	// ErrDraining reports a call after Runtime.Close started.
	ErrDraining = errors.New("harness: runtime is draining")
)

// Options configures a Runtime.
type Options struct {
	// Store holds every session log. It is required.
	Store Store
	// Owner grants sessions. nil: this process owns every session.
	Owner Owner

	backend turn.Backend
}

// Runtime hosts many sessions. Each runs only while its Ownership holds.
type Runtime struct {
	store   Store
	owner   Owner
	backend turn.Backend
	name    func() string
	base    context.Context
	cancel  context.CancelFunc
	group   sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	sessions map[string]*entry
}

type entry struct {
	ready chan struct{}
	s     *Session
	err   error
}

// New returns a Runtime. It does no I/O; sessions load on Create or Open.
func New(opts Options) (*Runtime, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("%w: Options.Store is nil", ErrInvalidRequest)
	}
	r := &Runtime{store: opts.Store, owner: opts.Owner, backend: opts.backend, sessions: map[string]*entry{}}
	if r.owner == nil {
		r.owner = newLocalOwner()
	}
	if r.backend == nil {
		r.backend = noBackend{}
	}
	r.name = sync.OnceValue(func() string {
		host, _ := os.Hostname()
		return fmt.Sprintf("%s/%d", host, os.Getpid())
	})
	r.base, r.cancel = context.WithCancel(context.Background())
	return r, nil
}

// Create creates a session and runs it.
func (r *Runtime) Create(ctx context.Context, req protocol.CreateSession) (*Session, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("%w: model is empty", ErrInvalidRequest)
	}
	id := req.ID
	if id == "" {
		id = "ses_" + strings.ToLower(rand.Text())
	}
	if err := checkName("session", id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	created := eventlog.SessionCreated{Model: req.Model, Origin: req.Origin,
		Settings: eventlog.Settings{Effort: req.Effort, ServiceTier: req.ServiceTier}}
	return r.load(ctx, id, true, func(cfg session.Config) (*session.Actor, error) {
		return session.Create(ctx, cfg, created)
	})
}

// Open acquires a session, fences earlier owners, replays its log, and runs
// it. A session that this runtime already runs is returned as is.
func (r *Runtime) Open(ctx context.Context, id string) (*Session, error) {
	if err := checkName("session", id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return r.load(ctx, id, false, func(cfg session.Config) (*session.Actor, error) {
		return session.Open(ctx, cfg)
	})
}

func (r *Runtime) load(ctx context.Context, id string, create bool, start func(session.Config) (*session.Actor, error)) (*Session, error) {
	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, ErrDraining
		}
		e := r.sessions[id]
		if e == nil {
			e = &entry{ready: make(chan struct{})}
			r.sessions[id] = e
			r.mu.Unlock()
			e.s, e.err = r.start(ctx, id, e, start)
			if e.err != nil {
				r.forget(id, e)
			}
			close(e.ready)
			return e.s, e.err
		}
		r.mu.Unlock()
		if create {
			return nil, fmt.Errorf("%w: %s", ErrSessionExists, id)
		}
		select {
		case <-e.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if e.err != nil {
			return nil, e.err
		}
		if !e.s.a.View().Stopped {
			return e.s, nil
		}
		r.forget(id, e)
	}
}

func (r *Runtime) forget(id string, e *entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[id] == e {
		delete(r.sessions, id)
	}
}

func (r *Runtime) start(ctx context.Context, id string, e *entry, start func(session.Config) (*session.Actor, error)) (*Session, error) {
	own, err := r.owner.Acquire(ctx, id)
	if err != nil {
		return nil, err
	}
	a, err := start(session.Config{
		ID:        id,
		Log:       storeLog{r.store, id},
		Ownership: own,
		Owner:     r.name(),
		Backend:   r.backend,
		Base:      r.base,
		Go:        r.group.Go,
		Done:      func() { r.forget(id, e) },
	})
	if err != nil {
		return nil, err
	}
	return &Session{a: a}, nil
}

// List returns a page of sessions in ID order.
func (r *Runtime) List(ctx context.Context, q protocol.ListSessions) (protocol.SessionPage, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	ids, err := r.store.Sessions(ctx, q.After, limit)
	if err != nil {
		return protocol.SessionPage{}, err
	}
	page := protocol.SessionPage{Sessions: []protocol.Session{}}
	for _, id := range ids {
		v, err := r.describe(ctx, id)
		if err != nil {
			return protocol.SessionPage{}, err
		}
		page.Sessions = append(page.Sessions, v)
	}
	if len(ids) == limit {
		page.Next = ids[len(ids)-1]
	}
	return page, nil
}

func (r *Runtime) describe(ctx context.Context, id string) (protocol.Session, error) {
	r.mu.Lock()
	e := r.sessions[id]
	r.mu.Unlock()
	if e != nil {
		select {
		case <-e.ready:
			if e.s != nil {
				return e.s.View(), nil
			}
		default:
		}
	}
	v, err := OpenView(ctx, r.store, id)
	if err != nil {
		return protocol.Session{}, err
	}
	return v.Session(), nil
}

// Close hands off every session and waits for every goroutine of the
// runtime. When ctx ends first, it stops the remaining sessions without an
// append; their next Open finds a crashed turn.
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	entries := make([]*entry, 0, len(r.sessions))
	for _, e := range r.sessions {
		entries = append(entries, e)
	}
	r.mu.Unlock()
	var wg sync.WaitGroup
	errs := make([]error, len(entries))
	for i, e := range entries {
		wg.Go(func() {
			select {
			case <-e.ready:
			case <-ctx.Done():
				return
			}
			if e.s != nil {
				if err := e.s.Release(ctx); !errors.Is(err, ErrSessionNotOwned) {
					errs[i] = err
				}
			}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		r.cancel()
	}
	r.group.Wait()
	r.cancel()
	return errors.Join(errs...)
}

type storeLog struct {
	st Store
	id string
}

func (l storeLog) Head(ctx context.Context) (uint64, error) { return l.st.Head(ctx, l.id) }

func (l storeLog) Append(ctx context.Context, expectedSeq uint64, records ...[]byte) error {
	err := l.st.Append(ctx, l.id, expectedSeq, records...)
	if errors.Is(err, ErrConflict) {
		return fmt.Errorf("%w: %w", session.ErrConflict, err)
	}
	return err
}

func (l storeLog) Read(ctx context.Context, afterSeq uint64, limit int) ([]eventlog.Record, error) {
	recs, err := l.st.Read(ctx, l.id, afterSeq, limit)
	out := make([]eventlog.Record, len(recs))
	for i, rec := range recs {
		out[i] = eventlog.Record{Seq: rec.Seq, Data: rec.Data}
	}
	return out, err
}

type noBackend struct{}

func (noBackend) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (noBackend) Run(context.Context, turn.Request, turn.Sink) (turn.Result, error) {
	return turn.Result{}, errors.New("harness: no model backend is configured")
}
