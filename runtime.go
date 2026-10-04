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
	"github.com/majorcontext/harness/internal/tool/pluginsrc"
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
	// ErrRequestNotPending reports a Resolve of a request that is not open.
	ErrRequestNotPending = session.ErrRequestNotPending
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
	// AskUserQuestion declares that the embedder renders a question of a
	// backend that owns its loop, such as AskUserQuestion of Claude Code, and
	// answers it with Session.Resolve. A question that nothing answers parks
	// the session, so without it no turn may ask. A child session and a
	// session with an active goal never ask.
	AskUserQuestion bool
	// Version is the build version that the engine banner names. Each model
	// call of a harness-loop turn sends the banner as engine context, after
	// the newest message of the session when its first request left. Empty:
	// no banner.
	Version string
}

// Runtime hosts many sessions. Each runs only while its Ownership holds.
type Runtime struct {
	store Store
	owner Owner
	sync  Sync
	tools []turn.Tool
	// models routes each turn to the backend of its model.
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
	// banner is the engine status that a model call sends; empty: none.
	banner string
	// questions lets a backend ask the user a question.
	questions bool
	// commandDirs are the prompt-command dirs; nil without a WorkDir.
	commandDirs []string
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
	r := &Runtime{store: opts.Store, owner: opts.Owner, sync: opts.Sync,
		sessions:  map[string]*entry{},
		threshold: positive(opts.Config.CompactionThreshold, d.CompactionThreshold), keep: positive(opts.Config.CompactionKeepTurns, d.CompactionKeepTurns)}
	r.limits = turn.Limits{Retries: opts.Config.PromptRetriesValue(), Continuations: opts.Config.MaxTokensContinuationsValue(),
		Idle: time.Duration(cmp.Or(opts.Config.StreamIdleTimeoutS, d.StreamIdleTimeoutS)) * time.Second}
	if opts.Config.GoalEvaluatorModel != "" {
		r.evaluator = opts.Config.ResolveModel(opts.Config.GoalEvaluatorModel)
	}
	r.prompt = func() string { return strings.Join(prompt.Build(opts.Config, opts.WorkDir), "\n\n") }
	r.resolve = opts.Config.ResolveModel
	r.commandDirs = commandDirs(opts.WorkDir, opts.Config.CommandsDirs)
	r.sup = &supervisor{depth: positive(opts.Config.MaxTaskDepth, d.MaxTaskDepth),
		running: positive(opts.Config.MaxConcurrentTasks, d.MaxConcurrentTasks), tokens: opts.Config.MaxTreeTokens,
		locks: map[string]*treeLock{}, quiet: map[string]int{}}
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
	r.models = newModels(opts.Config, opts.WorkDir, opts.ModelTransport)
	for _, t := range tools {
		if name := t.Spec().Name; name == "" || r.known("", name) {
			return nil, fmt.Errorf("%w: tool name %q is empty, repeated, or reserved", ErrInvalidRequest, name)
		}
		r.tools = append(r.tools, t)
	}
	if r.owner == nil {
		r.owner = newLocalOwner()
	}
	r.name = sync.OnceValue(func() string {
		host, _ := os.Hostname()
		return fmt.Sprintf("%s/%d", host, os.Getpid())
	})
	r.banner, r.questions = banner(opts.Version, opts.Config.SessionSync, time.Now()), opts.AskUserQuestion
	r.base, r.cancel = context.WithCancel(context.Background())
	return r, nil
}

// banner renders the engine status line, or "" for an empty version.
func banner(version, sessionSync string, started time.Time) string {
	if version == "" {
		return ""
	}
	mode := "fsync"
	if sessionSync == "volume" {
		mode = sessionSync
	}
	return "[engine: harness " + version + " · session_sync=" + mode + " · engine started " + started.UTC().Format(time.RFC3339) + "]"
}

// positive returns v, or def when v is not positive.
func positive[T int | float64](v, def T) T {
	if v <= 0 {
		return def
	}
	return v
}

// Create creates a session and runs it. An empty model is Config.Model, or
// config.DefaultModel.
func (r *Runtime) Create(ctx context.Context, req protocol.CreateSession) (*Session, error) {
	if req.Model == "" {
		req.Model = r.resolve("")
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
	return r.create(ctx, id, launch{created: &created})
}

func (r *Runtime) create(ctx context.Context, id string, l launch) (*Session, error) {
	if err := r.checkModel(l.created.Model, l.created.AllowedTools); err != nil {
		return nil, err
	}
	return r.load(ctx, id, l)
}

func newSuffix() string { return strings.ToLower(rand.Text()) }

// Open acquires a session, fences earlier owners, replays its log, and runs
// it. A session that this runtime already runs is returned as is.
func (r *Runtime) Open(ctx context.Context, id string) (*Session, error) {
	if err := checkName("session", id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return r.load(ctx, id, launch{})
}

// launch is how a load starts a session that this runtime does not run yet.
type launch struct {
	// created and first start a new session. A nil created opens the session
	// from its log.
	created *eventlog.SessionCreated
	first   *eventlog.InputAdmitted
	// profile is the agent profile of the session, when the caller has read
	// it.
	profile *prompt.Profile
}

func (r *Runtime) load(ctx context.Context, id string, l launch) (*Session, error) {
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
			e.s, e.err = r.start(ctx, id, e, l)
			if e.err != nil {
				r.forget(id, e)
			}
			close(e.ready)
			if e.err == nil {
				r.run(e.s, l.created != nil)
			}
			r.group.Done()
			return e.s, e.err
		}
		r.mu.Unlock()
		if l.created != nil {
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

// run runs session s after load publishes it, so a tool of its first run
// finds it. An opened session then settles or opens its unsettled children.
func (r *Runtime) run(s *Session, create bool) {
	s.a.Run()
	if create {
		close(s.recovered)
		return
	}
	r.group.Go(func() {
		r.recoverChildren(s.a)
		close(s.recovered)
	})
}

func (r *Runtime) forget(id string, e *entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[id] == e {
		delete(r.sessions, id)
	}
}

func (r *Runtime) start(ctx context.Context, id string, e *entry, l launch) (*Session, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(r.base, cancel)()
	if err := r.startPlugins(ctx); err != nil {
		return nil, err
	}
	own, err := r.owner.Acquire(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSessionNotOwned, err)
	}
	var c eventlog.SessionCreated
	if l.created != nil {
		c = *l.created
	} else if c, err = r.created(ctx, id); err != nil {
		own.Release()
		return nil, err
	}
	root, depth, err := r.tree(ctx, id, c.ParentID)
	if err != nil {
		own.Release()
		return nil, err
	}
	profile := r.profile(c.Agent, l.profile)
	var plug *pluginsrc.Session
	if r.plugins != nil {
		plug = r.plugins.Session(id)
	}
	cfg := session.Config{
		ID:              id,
		Store:           storeLog{r.store, id},
		Ownership:       own,
		Owner:           r.name(),
		Backend:         r.models,
		Banner:          r.banner,
		AskUserQuestion: r.questions,
		Evaluator:       r.evaluator,
		Source:          r.source(id, c.ParentID != "", plug),
		Retain:          r.workDir != "",
		Prompt:          r.instructions(c.Agent, profile),
		Appended:        r.appended(id, plug),
		Sync:            r.sync,
		Limits:          r.limits,
		Threshold:       r.threshold,
		KeepTurns:       r.keep,
		Base:            r.base,
		Go:              r.group.Go,
		Done:            func() { r.forget(id, e) },
		Check:           r.changeModel,
	}
	var a *session.Actor
	if l.created != nil {
		a, err = session.Create(ctx, cfg, c, l.first)
	} else {
		a, err = session.Open(ctx, cfg)
	}
	if err != nil {
		return nil, err
	}
	return &Session{a: a, r: r, id: id, root: root, depth: depth, recovered: make(chan struct{})}, nil
}

// profile returns the agent profile of a session: read, or read from the
// WorkDir now. The zero profile is no profile.
func (r *Runtime) profile(agent string, read *prompt.Profile) prompt.Profile {
	switch {
	case read != nil:
		return *read
	case agent == "":
		return prompt.Profile{}
	}
	return prompt.Profiles(r.workDir)[agent]
}

// instructions reads the system prompt of a session once and returns the
// system prompt of each of its turns: that prompt, the prompt of its agent
// profile, and the process status.
func (r *Runtime) instructions(agent string, p prompt.Profile) func() string {
	base := r.prompt()
	if agent != "" {
		base = strings.Trim(base+"\n\n"+p.Prompt, "\n")
	}
	return func() string {
		if r.procs == nil {
			return base
		}
		if s := processStatus(r.procs, r.workDir); s != "" {
			return strings.Join([]string{base, message.RenderEngineContext(s)}, "\n\n")
		}
		return base
	}
}

// appended gives the events of session id to its plugins, and reports the
// end of a turn of a child to its parent.
func (r *Runtime) appended(id string, plug *pluginsrc.Session) func([]eventlog.Event, *eventlog.State) {
	return func(events []eventlog.Event, st *eventlog.State) {
		if plug != nil {
			plug.Appended(events)
		}
		parent := st.Summary().ParentID
		if parent == "" || !slices.ContainsFunc(events, func(e eventlog.Event) bool { _, ok := e.(eventlog.TurnEnded); return ok }) {
			return
		}
		if s, text, ok := session.Settlement(id, st); ok {
			r.report(parent, s, text)
		}
	}
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

// loaded returns session id once this runtime has loaded it, or nil when
// it does not run it.
func (r *Runtime) loaded(ctx context.Context, id string) *Session {
	r.mu.Lock()
	e := r.sessions[id]
	r.mu.Unlock()
	if e == nil {
		return nil
	}
	select {
	case <-e.ready:
		return e.s
	case <-ctx.Done():
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

// pluginInfo returns the state of each plugin, or nil without plugins.
func (r *Runtime) pluginInfo() []protocol.Plugin {
	if r.plugins == nil {
		return nil
	}
	return r.plugins.Info()
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
	out := v.Session()
	out.Plugins = r.pluginInfo()
	return out, nil
}

// Close hands off every session, waits for every goroutine of the runtime,
// closes the model connections, and stops the processes. When ctx ends
// first, it stops the remaining sessions without an append, which cancels
// their turns, and still waits for every goroutine before it closes the
// model connections and kills the processes; their next Open finds a
// crashed turn. An external harness ends within its own stop grace.
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
			}
			if ctx.Err() != nil {
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
	stopped := make(chan struct{})
	go func() {
		r.group.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
		r.cancel()
		<-stopped
	}
	r.cancel()
	r.models.Close()
	r.closeTools(ctx)
	return errors.Join(errs...)
}

// hold adds one unit of the work that Close waits for, unless Close started.
func (r *Runtime) hold() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrDraining
	}
	r.group.Add(1)
	return nil
}

// startPlugins reads the plugin manifests once for each runtime, as part of
// the work that Close waits for. A plugin tool may not take the name of
// another tool.
func (r *Runtime) startPlugins(ctx context.Context) error {
	if r.plugins == nil {
		return nil
	}
	if err := r.hold(); err != nil {
		return err
	}
	defer r.group.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(r.base, cancel)()
	return r.plugins.Start(ctx, func(name string) bool { return r.known("", name) })
}

// history returns the conversation of session id.
func (r *Runtime) history(ctx context.Context, id string) ([]eventlog.Message, error) {
	var h []eventlog.Message
	err := r.read(ctx, id, func(st *eventlog.State) { h = st.History() })
	return h, err
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
	return l.st.Append(ctx, l.id, expectedSeq, records...)
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
func (r *Runtime) Models() []protocol.Model { return r.models.list() }
