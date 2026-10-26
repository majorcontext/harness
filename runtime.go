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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/backend"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/listing"
	"github.com/majorcontext/harness/internal/process"
	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/selftool"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/internal/storecheck"
	"github.com/majorcontext/harness/internal/tool/mcpsrc"
	"github.com/majorcontext/harness/internal/tool/pluginsrc"
	"github.com/majorcontext/harness/internal/tool/proc"
	"github.com/majorcontext/harness/internal/tracing"
	"github.com/majorcontext/harness/internal/tree"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/protocol"
)

var (
	// ErrInvalidRequest reports a malformed request.
	ErrInvalidRequest = &protocol.Error{Code: protocol.CodeInvalidRequest, Message: "harness: invalid request"}
	// ErrSessionNotFound reports a session with no log.
	ErrSessionNotFound = session.ErrNotFound
	// ErrBlobNotFound reports a blob key that no blob part of an input of the session names, or whose bytes the Store lacks.
	ErrBlobNotFound = &protocol.Error{Code: protocol.CodeBlobNotFound, Message: "harness: blob not found"}
	// ErrSessionExists reports a Create with the ID of an existing session.
	ErrSessionExists = session.ErrExists
	// ErrSessionNotOwned reports a session that this runtime does not run.
	ErrSessionNotOwned = session.ErrNotOwned
	// ErrSessionHeld reports a session that another owner holds. It matches ErrSessionNotOwned. An Owner fails Acquire with it.
	ErrSessionHeld = session.ErrHeld
	// ErrSessionReleased is the cause that Session.Err reports after Release, or after Close handed the session off. It matches ErrSessionNotOwned.
	ErrSessionReleased = session.ErrReleased
	// ErrSessionEnded is the cause that Session.Err reports after End. It matches ErrSessionNotOwned.
	ErrSessionEnded = session.ErrEnded
	// ErrOwnershipLost is the cause that Session.Err reports after the Ownership ended without Release, or after an append or Sync found a newer owner. It matches ErrSessionNotOwned.
	ErrOwnershipLost = session.ErrOwnershipLost
	// ErrInputConflict reports a repeated input ID with another body.
	ErrInputConflict = session.ErrInputConflict
	// ErrTurnMismatch reports a turn ID that is not the turn the call expects.
	ErrTurnMismatch = session.ErrTurnMismatch
	// ErrSessionBusy reports a Compact while a turn runs or inputs wait.
	ErrSessionBusy = session.ErrBusy
	// ErrRequestNotPending reports a Resolve of a request that is not open.
	ErrRequestNotPending = session.ErrRequestNotPending
	// ErrDraining reports a call after Runtime.Close started.
	ErrDraining = &protocol.Error{Code: protocol.CodeDraining, Message: "harness: runtime is draining"}
	// ErrProcessNotFound reports a process name that the box does not define, also without a WorkDir.
	ErrProcessNotFound = &protocol.Error{Code: protocol.CodeProcessNotFound, Message: "harness: process not found"}
	// ErrUnreplayable reports a session log that does not replay: a record that does not decode, or breaks a rule of the log.
	ErrUnreplayable = session.ErrUnreplayable
)

// Options configures a Runtime.
type Options struct {
	// Store holds every session log. It is required.
	Store Store
	// Owner grants sessions. nil: NewLocalOwner(1), which owns every session. The default does not
	// exclude another Runtime on the same Store; the fence that Open appends stops the earlier one.
	Owner Owner
	// Sync replicates every record that this Runtime appends. nil: no replication. New reads neither Config.OwnerEpoch nor Config.Sync; the embedder passes NewLocalOwner and httpsync.New.
	Sync Sync
	// Config configures the model providers and the MCP servers. A model ref "provider/model" selects the entry of Config.Providers named provider. Each session gets the tools of Config.MCPServers.
	Config config.Config
	// ModelTransport returns the HTTP transport for a model provider. nil, or a nil result: the default transport.
	ModelTransport func(provider string) http.RoundTripper
	// Getenv reads the variables that hold provider API keys, by the api_key_env of the provider or its
	// default name. nil: New and the runtime read no variable. A native API provider then needs
	// configured auth or a credential header. A transport that supplies auth sets auth injected.
	// A backend child process, such as the claude CLI, a plugin,
	// an MCP stdio server, a process, or a bash call, still inherits the environment of this process.
	Getenv func(key string) string
	// Logins holds the sign-in state of the injected providers. The hosts of this module set it; nil: the runtime makes its own from ModelTransport.
	Logins *Logins
	// Host says what the host cannot do. The zero value is a host that can do everything.
	Host HostLimits
	// Logger receives every log line of the runtime. nil: slog.Default().
	Logger *slog.Logger
	// Tools are the embedder tools. Each name must be unique. With a
	// WorkDir, no name may be process, task, model, read_tool_result, or session_info; a
	// file, search, or bash name needs AllowedTools to be a list. With a goal evaluator, no name may be goal. With Config.MCPServers, no name
	// may be mcp, list_mcp_resources, read_mcp_resource, load_mcp_skill, or start with mcp__.
	Tools []Tool
	// WorkDir is the directory of a coding agent. Each session reads its AGENTS.md chain and skills when
	// it starts, gets the file, search, and bash tools, the process tool runs Config.Processes in it, and
	// the task tool starts child sessions with the agent profiles of its .agents dir. bash and the process
	// tool run any command, so WorkDir alone grants command execution. Empty: the system prompt is
	// Config.AppendSystemPrompt alone, no file is read, no process runs, no built-in tool exists, and no
	// session has the task tool. New makes a relative path absolute, and fails when it cannot.
	WorkDir string
	// AskUserQuestion lets a session ask the user a question: the tool
	// ask_user_question of a model API session, and AskUserQuestion of Claude
	// Code. The embedder renders the question and answers it with
	// Session.Resolve. A question that nothing answers parks the session, so
	// without it no turn may ask. A child session and a session with an
	// active goal never ask.
	AskUserQuestion bool
	// WebTest is a test seam of web_fetch: loopback, resolver, proxy, and root CAs. Production code leaves it zero.
	WebTest WebTestSeam
	// Version is the build version that the engine banner names. Each model call of a harness-loop turn
	// sends the banner as engine context, after the newest message of the session when its first request left. Empty: no banner.
	Version string
	// Plugins run in this process, after Config.Plugins. A name and a tool name
	// must be unique. Close waits for their calls; a call that ignores its
	// context holds Close until the context of Close ends.
	Plugins []*plugin.Plugin
	// ServeURL and RunToken go to each plugin. A token needs a URL.
	ServeURL, RunToken string
	// TracerProvider receives the spans of turns, model calls, and tool calls. nil: no span is recorded.
	TracerProvider trace.TracerProvider
	// CaptureMessageContent adds the messages, tool definitions, and tool arguments and results to the spans. It is off by default.
	CaptureMessageContent bool
}

// HostLimits says what the host of a Runtime cannot do.
type HostLimits struct {
	// NoProcess says that the host starts no process. A provider that runs a child process, such as the Claude Code CLI, has no backend.
	NoProcess bool
	// NoLogin says that the host has no file login. A provider that uses the ChatGPT login is then not listed, reads no login file, and fails a request before it is sent.
	NoLogin bool
	// NoNetwork says that the host has no direct network. The runtime offers no web_fetch.
	NoNetwork bool
}

// Runtime hosts many sessions. Each runs only while its Ownership holds.
type Runtime struct {
	store Store
	owner Owner
	sync  Sync
	log   *slog.Logger
	tools []turn.Tool
	// models routes each turn to the backend of its model.
	models    *backend.Router
	limits    turn.Limits
	maxTokens int
	// prompt reads the system prompt of a session with the files that it holds.
	prompt     func() prompt.Info
	evaluator  string
	titleModel string
	resolve    func(string) string
	// aliases map a model alias to its ref.
	aliases map[string]string
	tree    *tree.Tree
	// procs is nil without a WorkDir.
	procs  *process.Manager
	mcp    *mcpsrc.Source // the live set of runtime MCP servers
	mcpCfg config.Config  // the config of the runtime source
	// plugins is nil without plugins.
	plugins *pluginsrc.Plugins
	workDir string
	web     *webOptions // nil: no web_fetch
	health  protocol.Health
	// version is the build version that the engine banner names; empty: no banner.
	version string
	// questions lets a backend ask the user a question.
	questions bool
	// webSearch is Config.Web.Search: models whose backend has a native web search get it.
	webSearch bool
	// commandDirs are the prompt-command dirs, and agentDirs the agent profile
	// dirs; both are nil without a WorkDir.
	commandDirs []string
	agentDirs   []string
	// threshold and keep are the compaction settings of each session.
	threshold float64
	keep      int
	name      func() string
	lister    listing.Lister
	base      context.Context
	cancel    context.CancelFunc
	// closing ends when Close starts.
	closing    context.Context
	closeStart context.CancelFunc
	group      sync.WaitGroup

	mu     sync.Mutex
	closed bool
	// closeDone closes when the first Close returns; closeErr is its result.
	closeDone chan struct{}
	closeErr  error
	// syncStopped is set once a Sync rejects a batch for good.
	syncStopped atomic.Bool
	sessions    map[string]*entry
	surfaces    map[string]bool // per session ID: whether the session has MCP tools (see mcpSurface)
	// catching holds the sessions that CatchUp replicates.
	catching  map[string]*catchGrant
	catchSlot chan struct{}
}

// New returns a Runtime. It does no I/O; sessions load on Create or Open.
func New(opts Options) (*Runtime, error) {
	if opts.Store == nil {
		return nil, errors.New("harness: Options.Store is nil")
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	if opts.RunToken != "" && opts.ServeURL == "" {
		return nil, errors.New("harness: Options.RunToken needs Options.ServeURL")
	}
	if opts.WorkDir != "" {
		abs, err := filepath.Abs(opts.WorkDir)
		if err != nil {
			return nil, fmt.Errorf("harness: Options.WorkDir: %w", err)
		}
		opts.WorkDir = abs
	}
	d := config.Defaults()
	r := &Runtime{store: opts.Store, owner: opts.Owner, sync: opts.Sync, log: cmp.Or(opts.Logger, slog.Default()), health: healthOf(opts.Version, time.Now()),
		sessions: map[string]*entry{}, surfaces: map[string]bool{}, catching: map[string]*catchGrant{}, catchSlot: make(chan struct{}, 1),
		titleModel: modelRef(opts.Config, opts.Config.TitleModel),
		threshold:  positive(opts.Config.CompactionThreshold, d.CompactionThreshold), keep: positive(opts.Config.CompactionKeepTurns, d.CompactionKeepTurns)}
	r.maxTokens = opts.Config.MaxTokens
	r.limits = turn.Limits{Retries: opts.Config.StreamMaxRetriesValue(),
		Idle: time.Duration(cmp.Or(opts.Config.StreamIdleTimeoutS, d.StreamIdleTimeoutS)) * time.Second, Log: r.log,
		Trace: tracing.New(opts.TracerProvider, opts.CaptureMessageContent)}
	r.evaluator = modelRef(opts.Config, opts.Config.GoalEvaluatorModel)
	r.prompt = func() prompt.Info { return prompt.Describe(opts.Config, opts.WorkDir, r.log) }
	r.resolve, r.aliases = opts.Config.ResolveModel, opts.Config.Aliases
	r.commandDirs = resolveDirs(opts.WorkDir, opts.Config.CommandsDirs, ".agents/commands")
	r.agentDirs = resolveDirs(opts.WorkDir, opts.Config.AgentDefsDirs, ".agents")
	r.base, r.cancel = context.WithCancel(context.Background())
	r.closing, r.closeStart = context.WithCancel(r.base)
	catalog := selftool.Catalog{Aliases: r.aliases, Providers: func() []string { return r.models.Providers() }, Unavailable: func(p string) string { return r.models.Unavailable(p) }}
	r.tree = tree.New(host{r}, tree.Config{MaxDepth: positive(opts.Config.MaxTaskDepth, d.MaxTaskDepth),
		MaxRunning: positive(opts.Config.MaxConcurrentTasks, d.MaxConcurrentTasks), MaxTokens: opts.Config.MaxTreeTokens,
		Base: r.base, Go: r.group.Go, Profiles: func() (map[string]prompt.Profile, error) { return prompt.Profiles(r.agentDirs, r.log) },
		Resolve: opts.Config.ResolveModel, CheckModel: catalog.CheckChild,
		Vendor: func(model string) string {
			provider, _, _ := strings.Cut(model, "/")
			return opts.Config.ProviderEntries()[provider].Vendor
		}, Suffix: newSuffix, Log: r.log})
	tools := make([]turn.Tool, len(opts.Tools))
	for i, t := range opts.Tools {
		tools[i] = embedded{t}
	}
	if opts.WorkDir != "" && slices.ContainsFunc(tools, func(t turn.Tool) bool { return t.Spec().Name == selftool.ModelName }) {
		return nil, fmt.Errorf("harness: tool name %q is reserved", selftool.ModelName)
	}
	if r.evaluator != "" {
		tools = append(slices.Clip(tools), selftool.NewGoal(toolHost{r}))
	}
	if opts.WorkDir != "" {
		r.workDir, r.web = opts.WorkDir, newWebOptions(opts)
		r.procs = proc.NewManager(opts.WorkDir, opts.Config.Processes)
		tools = append(slices.Clip(tools), proc.NewTool(r.procs, opts.Config.Processes), r.tree.Tool())
		if opts.Config.ModelToolEnabled() {
			tools = append(tools, selftool.NewModel(toolHost{r}, catalog))
		}
	}
	r.mcp = mcpsrc.New(opts.Config, opts.WorkDir, r.log)
	r.mcpCfg = opts.Config
	r.plugins = pluginsrc.New(opts.Config, opts.Plugins, opts.WorkDir, pluginsrc.Calls{History: r.history, Blob: r.store.GetBlob, MCP: r.mcpCall, Generate: r.generate}, opts.ServeURL, opts.RunToken, r.log)
	r.models = backend.New(opts.Config, opts.WorkDir, opts.ModelTransport, backend.Env{Getenv: opts.Getenv, NoGetenv: "Options.Getenv is nil", Log: r.log, Logins: opts.Logins.owner(), NoProcess: opts.Host.NoProcess, NoLogin: opts.Host.NoLogin})
	if err := r.checkOptions(tools, opts); err != nil {
		r.mcp.Close()
		return nil, err
	}
	if r.owner == nil {
		r.owner = NewLocalOwner(1)
	}
	r.name = sync.OnceValue(func() string {
		host, _ := os.Hostname()
		return fmt.Sprintf("%s/%d", host, os.Getpid())
	})
	r.version, r.questions, r.webSearch = opts.Version, opts.AskUserQuestion, opts.Config.WebSearchEnabled()
	return r, nil
}

// positive returns v, or def when v is not positive.
func positive[T int | float64](v, def T) T {
	if v <= 0 {
		return def
	}
	return v
}

// Create creates a session and runs it. An empty model is Config.Model, or
// config.DefaultModel. A SessionOption applies to this session only.
func (r *Runtime) Create(ctx context.Context, req protocol.CreateSession, opts ...SessionOption) (*Session, error) {
	if req.Model == "" {
		req.Model = r.resolve("")
	}
	if err := checkEffort(req.Effort); err != nil {
		return nil, err
	}
	so, err := r.sessionOptions(opts)
	if err != nil {
		return nil, err
	}
	if so.allowedTools != nil {
		return nil, fmt.Errorf("%w: WithAllowedTools is only valid for Open", ErrInvalidRequest)
	}
	if err := r.startPlugins(ctx, false); err != nil {
		return nil, err
	}
	id := cmp.Or(req.ID, "ses_"+newSuffix())
	if err := storecheck.Name(id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	created := protocol.SessionCreated{Model: req.Model, Origin: req.Origin,
		Settings: protocol.Settings{Effort: req.Effort, ServiceTier: req.ServiceTier}, AllowedTools: req.AllowedTools}
	return r.create(ctx, id, launch{created: &created, mcp: so.mcp})
}

func (r *Runtime) create(ctx context.Context, id string, l launch) (*Session, error) {
	if r.workDir != "" {
		l.created.WorkDir = r.workDir
	}
	if err := r.checkModel(l.created.Model, l.created.AllowedTools, len(l.mcp) > 0); err != nil {
		return nil, err
	}
	return r.load(ctx, id, l)
}

func newSuffix() string { return strings.ToLower(rand.Text()) }

// Open acquires a session, fences earlier owners, replays its log, and runs
// it. A session that this runtime already runs is returned as is.
func (r *Runtime) Open(ctx context.Context, id string, opts ...SessionOption) (*Session, error) {
	if err := storecheck.Name(id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	so, err := r.sessionOptions(opts)
	if err != nil {
		return nil, err
	}
	return r.load(ctx, id, launch{mcp: so.mcp, allowedTools: so.allowedTools})
}

// launch is how a load starts a session that this runtime does not run yet.
type launch struct {
	// created and first start a new session. A nil created opens the session
	// from its log.
	created *protocol.SessionCreated
	first   *protocol.InputAdmitted
	// profile is the agent profile of the session, when the caller has read
	// it.
	profile *prompt.Profile
	// mcp holds the session MCP servers of WithSessionMCP.
	mcp map[string]config.MCPServerSpec
	// accounts applies session account selections before an opened turn resumes.
	accounts     *map[string]string
	allowedTools *[]string
}

func (r *Runtime) start(ctx context.Context, id string, e *entry, l launch) (*Session, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(r.base, cancel)()
	if err := r.beforeStart(ctx, id, l); err != nil {
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
	c, root, depth, err := r.lineageOf(ctx, own, id, l)
	if err != nil {
		return nil, err
	}
	plug := r.pluginSession(id)
	sp := selftool.NewPrompt(r.prompt(), c.Agent, r.profile(c.Agent, l.profile))
	sm := r.sessionSource(l.mcp)
	var a *session.Actor
	cfg := session.Config{
		ID:              id,
		Store:           storeLog{r.store, id},
		Ownership:       own,
		Owner:           r.name(),
		Backend:         r.models,
		Version:         r.version,
		AskUserQuestion: r.questions,
		WebSearch:       r.webSearch,
		Evaluator:       r.evaluator,
		TitleModel:      r.titleModel,
		Source:          r.source(id, root, c.ParentID != "", depth, plug, sp, sm),
		Record:          sp.Recording,
		Retain:          r.workDir != "",
		Prompt:          sp.System,
		Skills:          sp.SkillList(),
		Sent:            sp.Sent,
		Appended:        r.appended(id, plug),
		Params:          chatParams(plug),
		Sync:            r.sync,
		Log:             r.log,
		Limits:          r.limits,
		MaxTokens:       r.maxTokens,
		Threshold:       r.threshold,
		KeepTurns:       r.keep,
		Base:            r.base,
		Go:              r.group.Go,
		Done: func(closeDone func()) {
			if a != nil && a.Rejected() {
				r.syncStopped.Store(true)
			}
			sm.Close()
			r.forget(id, e, closeDone)
		},
		Check: r.changeModel(len(l.mcp) > 0),
	}
	if l.created != nil {
		a, err = session.Create(ctx, cfg, c, l.first)
	} else {
		a, err = session.OpenWithSettings(ctx, cfg, protocol.SettingsChanged{Accounts: l.accounts, AllowedTools: l.allowedTools})
	}
	if err != nil {
		sm.Close()
		return nil, err
	}
	return &Session{a: a, r: r, id: id, root: root, depth: depth, recovered: make(chan struct{})}, nil
}

func (r *Runtime) pluginSession(id string) *pluginsrc.Session {
	if r.plugins == nil {
		return nil
	}
	return r.plugins.Session(id)
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
	profiles, _ := prompt.Profiles(r.agentDirs, r.log)
	return profiles[agent]
}

// appended gives the events of session id to its plugins, and reports the
// end of a turn of a child to its parent.
func (r *Runtime) appended(id string, plug *pluginsrc.Session) func([]protocol.Payload, *eventlog.State, trace.SpanContext) {
	return func(events []protocol.Payload, st *eventlog.State, ended trace.SpanContext) {
		if plug != nil {
			plug.Appended(events)
		}
		parent := st.Summary().ParentID
		if parent == "" || !slices.ContainsFunc(events, func(e protocol.Payload) bool { _, ok := e.(protocol.TurnEnded); return ok }) {
			return
		}
		if s, report, ok := session.Settlement(id, st); ok {
			if report != nil {
				report.Caused(ended)
			}
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
	if err := storecheck.Name(id); err != nil {
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
	return r.lister.Page(ctx, listing.Source{Sessions: r.store.Sessions, First: firstRecord(r.store), Describe: r.describe, Invalid: ErrInvalidRequest, Log: r.log}, q)
}

// pluginInfo returns the state of each plugin, or nil without plugins.
func (r *Runtime) pluginInfo() []protocol.Plugin {
	if r.plugins == nil {
		return nil
	}
	return r.plugins.Info()
}

func (r *Runtime) describe(ctx context.Context, id string) (protocol.Session, error) {
	rd, err := r.Read(ctx, id)
	if err != nil {
		return protocol.Session{}, err
	}
	return rd.Snapshot(), nil
}

// Close hands off every session, then closes the model connections and stops
// the processes. It is idempotent: the first call shuts the runtime down and
// every later call returns its result. Each call returns by the end of its
// ctx with context.Cause(ctx). When the ctx of the first call ends, it
// cancels every run and kills the processes, and does not wait for a tool
// that ignores its ctx; a session whose run is still going keeps its
// Ownership until that run returns, and the next Open finds a crashed turn.
// An external harness ends within its own stop grace.
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closeDone != nil {
		done := r.closeDone
		r.mu.Unlock()
		select {
		case <-done:
			return r.closeErr
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	r.closeDone = make(chan struct{})
	r.mu.Unlock()
	r.closeErr = r.shutdown(ctx)
	close(r.closeDone)
	return r.closeErr
}

func (r *Runtime) shutdown(ctx context.Context) error {
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
				if rerr := e.s.a.ReleaseErr(); rerr != nil {
					errs[i] = rerr
				} else if !errors.Is(err, ErrSessionNotOwned) && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrSyncRejected) {
					errs[i] = err
				}
			}
		})
	}
	wg.Wait()
	var waitCut bool
	stopped := make(chan struct{})
	go func() {
		r.group.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		errs = append(errs, context.Cause(ctx))
		waitCut = true
	}
	r.cancel()
	clean := closeBy(ctx, r.models.Close)
	if !r.closeTools(ctx) {
		clean = false
	}
	if !clean && !waitCut {
		errs = append(errs, context.Cause(ctx))
	}
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
func (r *Runtime) startPlugins(ctx context.Context, refresh bool) error {
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
	return r.plugins.Start(ctx, func(name string) bool { return r.known("", name) }, refresh)
}

// ProbePlugins probes each plugin of Config.Plugins, as the first Create or
// Open does, and stores the manifests in the plugin cache. It returns each
// plugin of Config.Plugins and Options.Plugins with its tools and hooks. It
// fails as that Create or Open fails. It returns nil with no plugin. It
// refreshes the cache only before the first Create or Open of the runtime;
// after that it returns the started plugins unchanged.
func (r *Runtime) ProbePlugins(ctx context.Context) ([]protocol.Plugin, error) {
	if err := r.startPlugins(ctx, true); err != nil {
		return nil, err
	}
	return r.pluginInfo(), nil
}

// created returns the session.created record of session id, which is its
// first record.
func (r *Runtime) created(ctx context.Context, id string) (protocol.SessionCreated, error) {
	recs, err := r.store.Read(ctx, id, 0, 1)
	if err == nil && len(recs) == 0 {
		err = fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	if err != nil {
		return protocol.SessionCreated{}, err
	}
	env, err := eventlog.Decode(recs[0].Data)
	if err != nil {
		return protocol.SessionCreated{}, fmt.Errorf("%w: session %s: %w", ErrUnreplayable, id, err)
	}
	c, ok := env.Event.(protocol.SessionCreated)
	if !ok {
		return c, fmt.Errorf("%w: session %s starts with %s, not session.created", ErrUnreplayable, id, env.Event.Kind())
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
func (r *Runtime) history(ctx context.Context, id string) ([]protocol.LogMessage, error) {
	var h []protocol.LogMessage
	err := r.read(ctx, id, func(st *eventlog.State) { h = st.History() })
	return h, err
}

// closeTools stops the processes, the plugins, and the MCP servers, and
// reports whether each finished before ctx ended.
func (r *Runtime) closeTools(ctx context.Context) bool {
	clean := true
	if r.procs != nil {
		clean = r.procs.Close(ctx)
	}
	if r.plugins != nil {
		clean = closeBy(ctx, r.plugins.Close) && clean
	}
	clean = closeBy(ctx, r.mcp.Close) && clean
	return clean
}

// closeBy runs closeFn and reports whether it returned before ctx ended.
func closeBy(ctx context.Context, closeFn func()) bool {
	done := make(chan struct{})
	go func() {
		closeFn()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
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

// Models returns the models of the configured providers, by ID. A provider with
// auth "chatgpt" fetches its list once, within 3 s or until ctx ends.
func (r *Runtime) Models(ctx context.Context) []protocol.Model { return r.models.List(ctx) }
