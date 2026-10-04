package harness

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/prompt"
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
	// ErrSessionBusy reports a Compact while a turn runs or inputs wait.
	ErrSessionBusy = session.ErrBusy
	// ErrDraining reports a call after Runtime.Close started.
	ErrDraining = errors.New("harness: runtime is draining")
)

// Options configures a Runtime.
type Options struct {
	// Store holds every session log. It is required.
	Store Store
	// Owner grants sessions. nil: this Runtime owns every session. The
	// default does not exclude another Runtime on the same Store; the fence
	// that Open appends stops the earlier one.
	Owner Owner
	// Sync replicates every record that this Runtime appends. nil: no replication.
	Sync Sync
	// Config configures the model providers. A model ref "provider/model"
	// selects the entry of Config.Providers named provider.
	Config config.Config
	// ModelTransport returns the HTTP transport for a model provider.
	// nil, or a nil result: the default transport.
	ModelTransport func(provider string) http.RoundTripper
	// Tools are the embedder tools. Each name must be unique.
	Tools []Tool
	// WorkDir is the directory of a coding agent. Each session reads its
	// AGENTS.md chain and skills when it starts. Empty: the system prompt is
	// Config.AppendSystemPrompt alone, and no file is read.
	WorkDir string

	backend turn.Backend
}

// Runtime hosts many sessions. Each runs only while its Ownership holds.
type Runtime struct {
	store   Store
	owner   Owner
	sync    Sync
	backend turn.Backend
	tools   []turn.Tool
	// models is nil when Options.backend runs every turn.
	models  *models
	retries int
	// prompt reads the system prompt of a session.
	prompt func() string
	// threshold and keep are the compaction settings of each session.
	threshold float64
	keep      int
	name      func() string
	base      context.Context
	cancel    context.CancelFunc
	group     sync.WaitGroup

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
	if err := opts.Config.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	d := config.Defaults()
	r := &Runtime{store: opts.Store, owner: opts.Owner, sync: opts.Sync, backend: opts.backend,
		retries: opts.Config.PromptRetriesValue(), sessions: map[string]*entry{},
		threshold: positive(opts.Config.CompactionThreshold, d.CompactionThreshold), keep: positive(opts.Config.CompactionKeepTurns, d.CompactionKeepTurns)}
	r.prompt = func() string { return strings.Join(prompt.Build(opts.Config, opts.WorkDir), "\n\n") }
	names := map[string]bool{}
	for _, t := range opts.Tools {
		name := t.Spec().Name
		if name == "" || names[name] {
			return nil, fmt.Errorf("%w: tool name %q is empty or repeated", ErrInvalidRequest, name)
		}
		names[name] = true
		r.tools = append(r.tools, t)
	}
	if r.owner == nil {
		r.owner = newLocalOwner()
	}
	if r.backend == nil {
		r.models = newModels(opts.Config, opts.ModelTransport)
		r.backend = r.models
	}
	r.name = sync.OnceValue(func() string {
		host, _ := os.Hostname()
		return fmt.Sprintf("%s/%d", host, os.Getpid())
	})
	r.base, r.cancel = context.WithCancel(context.Background())
	return r, nil
}

// positive returns v, or def when v is not positive.
func positive[T int | float64](v, def T) T {
	if v <= 0 {
		return def
	}
	return v
}

// Create creates a session and runs it.
func (r *Runtime) Create(ctx context.Context, req protocol.CreateSession) (*Session, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("%w: model is empty", ErrInvalidRequest)
	}
	if r.models != nil {
		if err := r.models.check(req.Model, req.AllowedTools, r.tools); err != nil {
			return nil, err
		}
	}
	id := req.ID
	if id == "" {
		id = "ses_" + strings.ToLower(rand.Text())
	}
	if err := checkName("session", id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	created := eventlog.SessionCreated{Model: req.Model, Origin: req.Origin,
		Settings: eventlog.Settings{Effort: req.Effort, ServiceTier: req.ServiceTier}, AllowedTools: req.AllowedTools}
	return r.load(ctx, id, true, func(ctx context.Context, cfg session.Config) (*session.Actor, error) {
		return session.Create(ctx, cfg, created)
	})
}

// Open acquires a session, fences earlier owners, replays its log, and runs
// it. A session that this runtime already runs is returned as is.
func (r *Runtime) Open(ctx context.Context, id string) (*Session, error) {
	if err := checkName("session", id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return r.load(ctx, id, false, func(ctx context.Context, cfg session.Config) (*session.Actor, error) {
		return session.Open(ctx, cfg)
	})
}

func (r *Runtime) load(ctx context.Context, id string, create bool, start func(context.Context, session.Config) (*session.Actor, error)) (*Session, error) {
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
			r.group.Add(1)
			r.mu.Unlock()
			e.s, e.err = r.start(ctx, id, e, start)
			r.group.Done()
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
		select {
		case <-e.s.a.Done():
		case <-ctx.Done():
			return nil, ctx.Err()
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

func (r *Runtime) start(ctx context.Context, id string, e *entry, start func(context.Context, session.Config) (*session.Actor, error)) (*Session, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(r.base, cancel)()
	own, err := r.owner.Acquire(ctx, id)
	if err != nil {
		return nil, err
	}
	cfg := session.Config{
		ID:        id,
		Log:       storeLog{r.store, id},
		Blobs:     storeLog{r.store, id},
		Ownership: own,
		Owner:     r.name(),
		Backend:   r.backend,
		Tools:     r.tools,
		Prompt:    r.prompt(),
		Sync:      r.sync,
		Retries:   r.retries,
		Threshold: r.threshold,
		KeepTurns: r.keep,
		Base:      r.base,
		Go:        r.group.Go,
		Done:      func() { r.forget(id, e) },
	}
	if r.models != nil {
		cfg.Check = func(from, to string, names []string) error { return r.models.change(from, to, names, r.tools) }
	}
	a, err := start(ctx, cfg)
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

// Close hands off every session, waits for every goroutine of the runtime,
// and closes the model connections. When ctx ends first, it stops the
// remaining sessions without an append and returns; their next Open finds a
// crashed turn.
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
	done := make(chan struct{})
	go func() {
		r.group.Wait()
		if r.models != nil {
			r.models.Close()
		}
		close(done)
	}()
	defer r.cancel()
	select {
	case <-done:
		return errors.Join(errs...)
	case <-ctx.Done():
		return errors.Join(append(errs, ctx.Err())...)
	}
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

func (l storeLog) PutBlob(ctx context.Context, key string, r io.Reader) error {
	return l.st.PutBlob(ctx, l.id, key, r)
}

func (l storeLog) GetBlob(ctx context.Context, key string) (io.ReadCloser, error) {
	return l.st.GetBlob(ctx, l.id, key)
}

// Models returns the models that the configured providers serve, by ID. It
// does no I/O.
func (r *Runtime) Models() []protocol.Model {
	if r.models == nil {
		return []protocol.Model{}
	}
	return r.models.list()
}
