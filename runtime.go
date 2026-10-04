package harness

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/internal/tool/mcpsrc"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/process"
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
	// Config configures the model providers and the MCP servers. A model
	// ref "provider/model" selects the entry of Config.Providers named
	// provider. Each session gets the tools of Config.MCPServers.
	Config config.Config
	// ModelTransport returns the HTTP transport for a model provider.
	// nil, or a nil result: the default transport.
	ModelTransport func(provider string) http.RoundTripper
	// Tools are the embedder tools. Each name must be unique. With
	// Config.MCPServers, no name may be mcp, list_mcp_resources,
	// read_mcp_resource, or start with mcp__.
	Tools []Tool
	// WorkDir is the directory of a coding agent. Each session reads its
	// AGENTS.md chain and skills when it starts, and the process tool runs
	// Config.Processes in it. The tool's declare action runs any argv, so
	// WorkDir alone grants command execution. Empty: the system prompt is
	// Config.AppendSystemPrompt alone, no file is read, and no process runs.
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
	models *models
	limits turn.Limits
	// prompt reads the system prompt of a session.
	prompt func() string
	// procs is nil without a WorkDir.
	procs *process.Manager
	// mcp is nil without MCP servers.
	mcp     *mcpsrc.Source
	workDir string
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
		sessions:  map[string]*entry{},
		threshold: positive(opts.Config.CompactionThreshold, d.CompactionThreshold), keep: positive(opts.Config.CompactionKeepTurns, d.CompactionKeepTurns)}
	r.limits = turn.Limits{Retries: opts.Config.PromptRetriesValue(), Continuations: opts.Config.MaxTokensContinuationsValue(),
		Idle: time.Duration(cmp.Or(opts.Config.StreamIdleTimeoutS, d.StreamIdleTimeoutS)) * time.Second}
	r.prompt = func() string { return strings.Join(prompt.Build(opts.Config, opts.WorkDir), "\n\n") }
	tools := opts.Tools
	if opts.WorkDir != "" {
		r.procs, r.workDir = newProcesses(opts.WorkDir, opts.Config.Processes), opts.WorkDir
		tools = append(slices.Clip(tools), newProcessTool(r.procs, opts.Config.Processes))
	}
	r.mcp = mcpsrc.New(opts.Config)
	names := map[string]bool{}
	for _, t := range tools {
		name := t.Spec().Name
		if name == "" || names[name] || r.mcp != nil && mcpsrc.Reserved(name) {
			return nil, fmt.Errorf("%w: tool name %q is empty, repeated, or an MCP tool name", ErrInvalidRequest, name)
		}
		names[name] = true
		r.tools = append(r.tools, t)
	}
	if r.owner == nil {
		r.owner = newLocalOwner()
	}
	if r.backend == nil {
		r.models = newModels(opts.Config, opts.WorkDir, opts.ModelTransport)
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
		Prompt:    r.instructions(),
		Sync:      r.sync,
		Limits:    r.limits,
		Threshold: r.threshold,
		KeepTurns: r.keep,
		Base:      r.base,
		Go:        r.group.Go,
		Done:      func() { r.forget(id, e) },
	}
	if r.models != nil {
		cfg.Check = func(from, to string, names []string) error { return r.models.change(from, to, names, r.tools) }
	}
	if r.mcp != nil {
		cfg.Source = r.mcp
	}
	a, err := start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Session{a: a}, nil
}

// instructions reads the system prompt of a session once and returns the
// system prompt of each of its turns: that prompt and the process status.
func (r *Runtime) instructions() func() string {
	p := r.prompt()
	return func() string {
		if r.procs == nil {
			return p
		}
		if s := processStatus(r.procs, r.workDir); s != "" {
			return strings.Join([]string{p, message.RenderEngineContext(s)}, "\n\n")
		}
		return p
	}
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
// closes the model connections, and stops the processes. When ctx ends
// first, it stops the remaining sessions without an append, kills the
// processes, and returns; their next Open finds a crashed turn.
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
		r.closeTools(ctx)
		close(done)
	}()
	defer r.cancel()
	select {
	case <-done:
		return errors.Join(errs...)
	case <-ctx.Done():
		r.cancel()
		r.closeTools(ctx)
		return errors.Join(append(errs, ctx.Err())...)
	}
}

// closeTools stops the processes and the MCP servers.
func (r *Runtime) closeTools(ctx context.Context) {
	if r.procs != nil {
		r.procs.Close(ctx)
	}
	if r.mcp != nil {
		r.mcp.Close()
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
