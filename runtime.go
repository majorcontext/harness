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
	"github.com/majorcontext/harness/internal/tool/builtin"
	"github.com/majorcontext/harness/internal/tool/mcpsrc"
	"github.com/majorcontext/harness/internal/tool/pluginsrc"
	"github.com/majorcontext/harness/internal/toolresult"
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
	// Tools are the embedder tools. Each name must be unique. With a
	// WorkDir, no name may be process, task, or a built-in tool name. With a
	// goal evaluator, no name may be goal. With Config.MCPServers, no name
	// may be mcp, list_mcp_resources, read_mcp_resource, or start with mcp__.
	Tools []Tool
	// WorkDir is the directory of a coding agent. Each session reads its
	// AGENTS.md chain and skills when it starts, gets the file, search, and
	// bash tools, the process tool runs Config.Processes in it, and the task
	// tool starts child sessions with the agent profiles of its .agents dir.
	// bash and the process tool run any command, so WorkDir alone grants
	// command execution. Empty: the system prompt is
	// Config.AppendSystemPrompt alone, no file is read, no process runs, no
	// built-in tool exists, and no session has the task tool.
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
	prompt    func() string
	evaluator string
	resolve   func(string) string
	sup       *supervisor
	// procs is nil without a WorkDir.
	procs *process.Manager
	// mcp is nil without MCP servers.
	mcp *mcpsrc.Source
	// plugins is nil without plugins.
	plugins *pluginsrc.Plugins
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
	if opts.Config.GoalEvaluatorModel != "" {
		r.evaluator = opts.Config.ResolveModel(opts.Config.GoalEvaluatorModel)
	}
	r.prompt = func() string { return strings.Join(prompt.Build(opts.Config, opts.WorkDir), "\n\n") }
	r.resolve = opts.Config.ResolveModel
	r.sup = &supervisor{depth: positive(opts.Config.MaxTaskDepth, d.MaxTaskDepth),
		running: positive(opts.Config.MaxConcurrentTasks, d.MaxConcurrentTasks), tokens: opts.Config.MaxTreeTokens,
		roots: map[string]string{}, quiet: map[string]int{}}
	tools := opts.Tools
	if r.evaluator != "" {
		tools = append(slices.Clip(tools), goalTool{r: r})
	}
	if opts.WorkDir != "" {
		r.procs, r.workDir = newProcesses(opts.WorkDir, opts.Config.Processes), opts.WorkDir
		tools = append(slices.Clip(tools), newProcessTool(r.procs, opts.Config.Processes), taskTool{r: r})
	}
	r.mcp = mcpsrc.New(opts.Config)
	r.plugins = pluginsrc.New(opts.Config, opts.WorkDir, r.history)
	names := map[string]bool{}
	for _, t := range tools {
		name := t.Spec().Name
		if name == "" || names[name] || r.mcp != nil && mcpsrc.Reserved(name) || r.builtin(name) {
			return nil, fmt.Errorf("%w: tool name %q is empty, repeated, or reserved", ErrInvalidRequest, name)
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
	if err := r.startPlugins(ctx); err != nil {
		return nil, err
	}
	id := cmp.Or(req.ID, "ses_"+newSuffix())
	if err := checkName("session", id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	created := eventlog.SessionCreated{Model: req.Model, Origin: req.Origin,
		Settings: eventlog.Settings{Effort: req.Effort, ServiceTier: req.ServiceTier}, AllowedTools: req.AllowedTools}
	return r.create(ctx, id, created, nil)
}

func (r *Runtime) create(ctx context.Context, id string, c eventlog.SessionCreated, first *eventlog.InputAdmitted) (*Session, error) {
	if r.models != nil {
		if err := r.models.check(c.Model, r.unowned(c.AllowedTools), r.named()); err != nil {
			return nil, err
		}
	}
	return r.load(ctx, id, true, func(ctx context.Context, cfg session.Config) (*session.Actor, error) {
		return session.Create(ctx, cfg, c, first)
	})
}

func newSuffix() string { return strings.ToLower(rand.Text()) }

// Open acquires a session, fences earlier owners, replays its log, and runs
// it. A session that this runtime already runs is returned as is.
func (r *Runtime) Open(ctx context.Context, id string) (*Session, error) {
	if err := checkName("session", id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return r.load(ctx, id, false, func(ctx context.Context, cfg session.Config) (*session.Actor, error) {
		a, err := session.Open(ctx, cfg)
		if err == nil {
			r.group.Go(func() { r.recoverChildren(id, a) })
		}
		return a, err
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
	if err := r.startPlugins(ctx); err != nil {
		return nil, err
	}
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
		Evaluator: r.evaluator,
		Tools:     r.bind(id),
		Prompt:    r.instructions(),
		Report:    r.report,
		Agent:     r.agent(),
		Sync:      r.sync,
		Limits:    r.limits,
		Threshold: r.threshold,
		KeepTurns: r.keep,
		Base:      r.base,
		Go:        r.group.Go,
		Done:      func() { r.forget(id, e) },
	}
	if r.models != nil {
		cfg.Check = func(from, to string, names []string) error {
			return r.models.change(from, to, r.unowned(names), r.named())
		}
	}
	var srcs turn.Sources
	if r.mcp != nil {
		srcs = append(srcs, mcpTools{r.mcp, r.backend})
	}
	if r.plugins != nil {
		p := r.plugins.Session(id)
		srcs, cfg.Appended = append(srcs, p), p.Appended
	}
	if srcs != nil {
		cfg.Source = srcs
	}
	a, err := start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Session{a: a, r: r, id: id}, nil
}

// mcpTools gives the MCP tools to each turn whose backend does not connect
// the servers itself.
type mcpTools struct {
	src     *mcpsrc.Source
	backend turn.Backend
}

func (m mcpTools) Toolset(ctx context.Context, history []eventlog.Message, allowed []string, model string) turn.Toolset {
	if allowed == nil && m.backend.Capabilities(model).OwnsMCP {
		return turn.Toolset{}
	}
	return m.src.Toolset(ctx, history, allowed, model)
}

// instructions reads the system prompt of a session once and returns the
// system prompt of each of its turns: that prompt, the prompt of its agent
// profile, and the process status. The actor calls it from one goroutine.
func (r *Runtime) instructions() func(agent string) string {
	p, read := r.prompt(), false
	return func(agent string) string {
		if !read && agent != "" {
			p = strings.Trim(p+"\n\n"+prompt.Profiles(r.workDir)[agent].Prompt, "\n")
		}
		read = true
		if r.procs == nil {
			return p
		}
		if s := processStatus(r.procs, r.workDir); s != "" {
			return strings.Join([]string{p, message.RenderEngineContext(s)}, "\n\n")
		}
		return p
	}
}

// bind returns the tools of session id, with each session tool bound to id.
func (r *Runtime) bind(id string) []turn.Tool {
	tools := slices.Clone(r.tools)
	for i, t := range tools {
		if b, ok := t.(interface{ bind(string) turn.Tool }); ok {
			tools[i] = b.bind(id)
		}
	}
	return tools
}

// running returns session id when this runtime runs it, or nil.
func (r *Runtime) running(id string) *Session {
	r.mu.Lock()
	e := r.sessions[id]
	r.mu.Unlock()
	if e == nil {
		return nil
	}
	select {
	case <-e.ready:
		return e.s
	default:
		return nil
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

// unowned returns the allowed names that the Options.Tools and the backend
// do not own. The MCP source owns its names, which depend on the servers.
func (r *Runtime) unowned(names []string) []string {
	if r.mcp == nil {
		return names
	}
	return slices.DeleteFunc(slices.Clone(names), mcpsrc.Reserved)
}

// named returns the Options.Tools and the plugin tools, for their names.
func (r *Runtime) named() []turn.Tool {
	if r.plugins == nil {
		return r.tools
	}
	return append(slices.Clip(r.tools), r.plugins.Tools()...)
}

// startPlugins reads the plugin manifests once for each runtime, as part of
// the work that Close waits for. A plugin tool may not take the name of
// another tool.
func (r *Runtime) startPlugins(ctx context.Context) error {
	if r.plugins == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrDraining
	}
	r.group.Add(1)
	r.mu.Unlock()
	defer r.group.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(r.base, cancel)()
	return r.plugins.Start(ctx, func(name string) bool {
		return r.mcp != nil && mcpsrc.Reserved(name) || r.builtin(name) || slices.ContainsFunc(r.tools, func(t turn.Tool) bool { return t.Spec().Name == name })
	})
}

// agent returns the file tools of a new session. Each session gets its own:
// the read guard of write_file belongs to one session. nil without a WorkDir.
func (r *Runtime) agent() []turn.Tool {
	if r.workDir == "" {
		return nil
	}
	return builtin.Tools(r.workDir)
}

// builtin reports whether name is a built-in tool of the WorkDir.
func (r *Runtime) builtin(name string) bool {
	return r.workDir != "" && (slices.Contains(builtin.Names, name) || name == toolresult.ToolName)
}

// history returns the conversation of session id from the store.
func (r *Runtime) history(ctx context.Context, id string) ([]eventlog.Message, error) {
	st, err := session.Load(ctx, id, storeLog{r.store, id})
	if err != nil {
		return nil, err
	}
	return st.History(), nil
}

// closeTools stops the processes, the plugins, and the MCP servers, and
// returns when ctx ends.
func (r *Runtime) closeTools(ctx context.Context) {
	if r.procs != nil {
		r.procs.Close(ctx)
	}
	if r.plugins != nil {
		closeBy(ctx, r.plugins.Close)
	}
	if r.mcp != nil {
		closeBy(ctx, r.mcp.Close)
	}
}

func closeBy(ctx context.Context, closeFn func()) {
	done := make(chan struct{})
	go func() {
		closeFn()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
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
