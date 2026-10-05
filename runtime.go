package harness

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/backend"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/internal/tool/mcpsrc"
	"github.com/majorcontext/harness/internal/tool/pluginsrc"
	"github.com/majorcontext/harness/internal/tool/proc"
	"github.com/majorcontext/harness/internal/tree"
	"github.com/majorcontext/harness/internal/turn"
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
	// WorkDir, no name may be process, task, model, or a built-in tool name. With a
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
	// ServeURL and RunToken go to each plugin. A token needs a URL.
	ServeURL, RunToken string
	// MaxTokens caps the response of each model call of a turn, as the engine
	// flag -max-tokens did. Zero: the backend default. Negative: New fails.
	MaxTokens int
}

// Runtime hosts many sessions. Each runs only while its Ownership holds.
type Runtime struct {
	store Store
	owner Owner
	sync  Sync
	tools []turn.Tool
	// models routes each turn to the backend of its model.
	models    *backend.Router
	limits    turn.Limits
	maxTokens int
	// prompt reads the system prompt of a session with the files that it holds.
	prompt    func() prompt.Info
	evaluator string
	resolve   func(string) string
	// aliases map a model alias to its ref.
	aliases map[string]string
	tree    *tree.Tree
	// procs is nil without a WorkDir.
	procs *process.Manager
	// mcp is nil without MCP servers.
	mcp *mcpsrc.Source
	// plugins is nil without plugins.
	plugins *pluginsrc.Plugins
	workDir string
	health  protocol.Health
	// banner is the engine status that a model call sends; empty: none.
	banner string
	// questions lets a backend ask the user a question.
	questions bool
	// commandDirs are the prompt-command dirs, and agentDirs the agent profile
	// dirs; both are nil without a WorkDir.
	commandDirs []string
	agentDirs   []string
	// threshold and keep are the compaction settings of each session.
	threshold float64
	keep      int
	name      func() string
	births    births
	base      context.Context
	cancel    context.CancelFunc
	// closing ends when Close starts.
	closing    context.Context
	closeStart context.CancelFunc
	group      sync.WaitGroup

	mu     sync.Mutex
	closed bool
	// syncStopped is set once a Sync rejects a batch for good.
	syncStopped atomic.Bool
	sessions    map[string]*entry
	// catching holds the sessions that CatchUp replicates.
	catching  map[string]*catchGrant
	catchSlot chan struct{}
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
	if key := ignoredKey(opts.Config); key != "" {
		return nil, fmt.Errorf("%w: config key %s is not read by the runtime", ErrInvalidRequest, key)
	}
	if err := checkEmbedder(opts); err != nil {
		return nil, err
	}
	d := config.Defaults()
	r := &Runtime{store: opts.Store, owner: opts.Owner, sync: opts.Sync, health: healthOf(opts.Version, opts.Config, time.Now()),
		sessions: map[string]*entry{}, catching: map[string]*catchGrant{}, catchSlot: make(chan struct{}, 1),
		threshold: positive(opts.Config.CompactionThreshold, d.CompactionThreshold), keep: positive(opts.Config.CompactionKeepTurns, d.CompactionKeepTurns)}
	r.maxTokens = opts.MaxTokens
	r.limits = turn.Limits{Retries: opts.Config.PromptRetriesValue(), Continuations: opts.Config.MaxTokensContinuationsValue(),
		Idle: time.Duration(cmp.Or(opts.Config.StreamIdleTimeoutS, d.StreamIdleTimeoutS)) * time.Second}
	if opts.Config.GoalEvaluatorModel != "" {
		r.evaluator = opts.Config.ResolveModel(opts.Config.GoalEvaluatorModel)
	}
	r.prompt = func() prompt.Info { return prompt.Describe(opts.Config, opts.WorkDir) }
	r.resolve, r.aliases = opts.Config.ResolveModel, opts.Config.Aliases
	r.commandDirs = resolveDirs(opts.WorkDir, opts.Config.CommandsDirs, ".agents/commands")
	r.agentDirs = resolveDirs(opts.WorkDir, opts.Config.AgentDefsDirs, ".agents")
	r.base, r.cancel = context.WithCancel(context.Background())
	r.closing, r.closeStart = context.WithCancel(r.base)
	r.tree = tree.New(host{r}, tree.Config{MaxDepth: positive(opts.Config.MaxTaskDepth, d.MaxTaskDepth),
		MaxRunning: positive(opts.Config.MaxConcurrentTasks, d.MaxConcurrentTasks), MaxTokens: opts.Config.MaxTreeTokens,
		Base: r.base, Go: r.group.Go, Profiles: func() (map[string]prompt.Profile, error) { return prompt.Profiles(r.agentDirs) },
		Resolve: opts.Config.ResolveModel, CheckModel: r.checkChildModel, Suffix: newSuffix})
	tools := opts.Tools
	if opts.WorkDir != "" && slices.ContainsFunc(tools, func(t Tool) bool { return t.Spec().Name == modelToolName }) {
		return nil, fmt.Errorf("%w: tool name %q is reserved", ErrInvalidRequest, modelToolName)
	}
	if r.evaluator != "" {
		tools = append(slices.Clip(tools), goalTool{r: r})
	}
	if opts.WorkDir != "" {
		r.procs, r.workDir = proc.NewManager(opts.WorkDir, opts.Config.Processes), opts.WorkDir
		tools = append(slices.Clip(tools), proc.NewTool(r.procs, opts.Config.Processes), r.tree.Tool())
		if opts.Config.ModelToolEnabled() {
			tools = append(tools, modelTool{r: r})
		}
	}
	r.mcp = mcpsrc.New(opts.Config)
	r.plugins = pluginsrc.New(opts.Config, opts.WorkDir, r.history, r.store.GetBlob, opts.ServeURL, opts.RunToken)
	r.models = backend.New(opts.Config, opts.WorkDir, opts.ModelTransport)
	for _, t := range tools {
		if name := t.Spec().Name; name == "" || r.known("", name) {
			return nil, fmt.Errorf("%w: tool name %q is empty, repeated, or reserved", ErrInvalidRequest, name)
		}
		r.tools = append(r.tools, t)
	}
	if r.owner == nil {
		r.owner = newLocalOwner(uint64(cmp.Or(opts.Config.OwnerEpoch, 1)))
	}
	if r.sync == nil && opts.Config.Sync != nil {
		r.sync = newHTTPSync(*opts.Config.Sync)
	}
	r.name = sync.OnceValue(func() string {
		host, _ := os.Hostname()
		return fmt.Sprintf("%s/%d", host, os.Getpid())
	})
	r.banner, r.questions = banner(opts.Version, opts.Config.SessionSync, time.Now()), opts.AskUserQuestion
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
		r.tree.Recover(s.a)
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
	if err := r.yieldCatchUp(ctx, id); err != nil {
		return nil, err
	}
	own, err := r.owner.Acquire(ctx, id)
	switch {
	case err == nil:
	case r.base.Err() != nil:
		return nil, ErrDraining
	case ctx.Err() != nil:
		return nil, err
	default:
		return nil, fmt.Errorf("%w: %w", ErrSessionNotOwned, err)
	}
	var c eventlog.SessionCreated
	if l.created != nil {
		c = *l.created
	} else if c, err = r.created(ctx, id); err != nil {
		own.Release()
		return nil, err
	}
	root, depth, err := r.tree.Lineage(ctx, id, c.ParentID)
	if err != nil {
		own.Release()
		return nil, err
	}
	profile := r.profile(c.Agent, l.profile)
	var plug *pluginsrc.Session
	if r.plugins != nil {
		plug = r.plugins.Session(id)
	}
	sp := r.newSessionPrompt(c.Agent, profile)
	var a *session.Actor
	cfg := session.Config{
		ID:              id,
		Store:           storeLog{r.store, id},
		Ownership:       own,
		Owner:           r.name(),
		Backend:         r.models,
		Banner:          r.banner,
		AskUserQuestion: r.questions,
		Evaluator:       r.evaluator,
		Source:          r.source(id, c.ParentID != "", plug, sp),
		Retain:          r.workDir != "",
		Prompt:          sp.system,
		Appended:        r.appended(id, plug),
		Sync:            r.sync,
		Limits:          r.limits,
		MaxTokens:       r.maxTokens,
		Threshold:       r.threshold,
		KeepTurns:       r.keep,
		Base:            r.base,
		Go:              r.group.Go,
		Done: func() {
			if a != nil && a.Rejected() {
				r.syncStopped.Store(true)
			}
			r.forget(id, e)
		},
		Check: r.changeModel,
	}
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
// WorkDir now. The zero profile is no profile. A repeated agent name fails a
// spawn, not the open of a session that exists.
func (r *Runtime) profile(agent string, read *prompt.Profile) prompt.Profile {
	switch {
	case read != nil:
		return *read
	case agent == "":
		return prompt.Profile{}
	}
	profiles, _ := prompt.Profiles(r.agentDirs)
	return profiles[agent]
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
		if s, report, ok := session.Settlement(id, st); ok {
			r.tree.Report(parent, s, report)
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

// End stops session id on this runtime and releases its ownership once Sync
// has acknowledged every record. It appends nothing and deletes nothing: the
// log stays in the store, and Open runs the session again. It also stops the
// turn of each descendant that this runtime runs, and opens no session. A
// session with a running turn or control command fails with ErrSessionBusy and
// stops no descendant; a running compaction or evaluation that no command
// started stops as under Release. A
// session that this runtime does not run is ended already; an ID with no log
// fails with ErrSessionNotFound.
func (r *Runtime) End(ctx context.Context, id string) error {
	if err := checkName("session", id); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	s := r.loaded(ctx, id)
	return r.tree.End(ctx, id, func(ctx context.Context) error {
		if s != nil {
			return s.a.End(ctx)
		}
		_, err := OpenView(ctx, r.store, id)
		return err
	})
}

// List returns a page of sessions in creation order. The After of a page is
// the ID of the last session of the page before it.
func (r *Runtime) List(ctx context.Context, q protocol.ListSessions) (protocol.SessionPage, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	ids, err := r.sessionsByCreation(ctx, q.After, limit)
	if err != nil {
		return protocol.SessionPage{}, err
	}
	page := protocol.SessionPage{Sessions: []protocol.Session{}}
	for _, id := range ids {
		v, err := r.describe(ctx, id)
		if errors.Is(err, session.ErrUnreplayable) {
			slog.Warn("harness: session skipped in the list", "session", id, "err", err)
			continue
		}
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
	r.closeStart()
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
				err := e.s.Release(ctx)
				if !errors.Is(err, ErrSessionNotOwned) && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrSyncRejected) {
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

// SyncStopped reports whether a Sync rejected a batch for good since New, so
// that Sync may lack records of a session that this runtime ran. It stays
// true for the life of the runtime.
func (r *Runtime) SyncStopped() bool { return r.syncStopped.Load() }

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

// ProbePlugins reads the manifest of each configured plugin, as the first
// Create or Open does, and returns each plugin with its tools and hooks. It
// fails as that Create or Open fails. It returns nil with no plugin.
func (r *Runtime) ProbePlugins(ctx context.Context) ([]protocol.Plugin, error) {
	if err := r.startPlugins(ctx); err != nil {
		return nil, err
	}
	return r.pluginInfo(), nil
}

// created returns the session.created record of session id, which is its
// first record.
func (r *Runtime) created(ctx context.Context, id string) (eventlog.SessionCreated, error) {
	recs, err := r.store.Read(ctx, id, 0, 1)
	if err == nil && len(recs) == 0 {
		err = fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	if err != nil {
		return eventlog.SessionCreated{}, err
	}
	env, err := eventlog.Decode(recs[0].Data)
	if err != nil {
		return eventlog.SessionCreated{}, err
	}
	c, ok := env.Event.(eventlog.SessionCreated)
	if !ok {
		return c, fmt.Errorf("harness: session %s starts with %s, not session.created", id, env.Event.Kind())
	}
	return c, nil
}

// read runs f with the state of session id, which f must not keep. A session
// that this runtime runs answers through its actor; any other session
// replays from the store.
func (r *Runtime) read(ctx context.Context, id string, f func(*eventlog.State)) error {
	if s := r.running(id); s != nil {
		if err := s.a.Read(ctx, f); !errors.Is(err, ErrSessionNotOwned) {
			return err
		}
	}
	st, err := session.Load(ctx, id, storeLog{r.store, id})
	if err != nil {
		return err
	}
	f(st)
	return nil
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
func (r *Runtime) Models() []protocol.Model { return r.models.List() }
