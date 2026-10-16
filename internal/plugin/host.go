package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	pub "github.com/majorcontext/harness/plugin"

	"github.com/majorcontext/harness/internal/plugin/wire"
)

// defaultEventQueueSize is the per-plugin-instance bound on queued
// hook/event batches when Options.EventQueueSize is unset. See
// Host.Emit and PROTOCOL.md for drop semantics.
const defaultEventQueueSize = 256

// ClientAPI is implemented by the runtime to serve plugin → harness calls.
type ClientAPI interface {
	SessionMessages(ctx context.Context, req *pub.SessionMessagesRequest) (*pub.SessionMessagesResponse, error)
	MCPCall(ctx context.Context, req *pub.MCPCallRequest) (*pub.MCPCallResult, error)
	Generate(ctx context.Context, req *pub.GenerateRequest) (*pub.GenerateResponse, error)
}

// Spec configures one plugin for a Host.
//
// Manifest comes from the probe cache (see ProbeSpec), which lets the Host
// route hooks and advertise tools without starting the long-lived process. A
// plugin process starts on its first hook dispatch or tool call, then stays
// warm.
type Spec struct {
	Command []string
	Env     []string // appended to the harness environment
	Dir     string
	// Config is this plugin's block from the harness config, passed
	// verbatim in InitializeParams.
	Config   json.RawMessage
	Manifest pub.Manifest

	// dial overrides process spawning; used by tests.
	dial func() (io.ReadWriteCloser, error)
}

// Options configures a Host.
type Options struct {
	HarnessVersion string
	WorkspaceDir   string
	// HTTPHeaders are passed to every plugin in InitializeParams for its
	// outbound HTTP traffic (e.g. workspace attribution headers).
	HTTPHeaders map[string]string
	// ServeURL and RunToken are forwarded verbatim into every plugin's
	// InitializeParams (see that type's doc comment for the trust model).
	// Set both only when this Host belongs to a running `harness serve`
	// instance; leave both empty in `harness run` mode, where there is no
	// HTTP API for a plugin to reach.
	ServeURL string
	RunToken string
	// Client serves plugin → harness API calls. May be nil, in which case
	// those calls fail with method-not-found.
	Client ClientAPI
	// HookTimeout bounds each sync hook dispatch so a hung plugin cannot
	// wedge a session. Defaults to 5s.
	HookTimeout time.Duration
	// OnError observes per-plugin dispatch failures. Sync hook chains fail
	// open: the erroring plugin is skipped and the chain continues. It is
	// also invoked (once, on the first occurrence) when a plugin's event
	// queue is full and an event had to be dropped — see Host.Emit.
	OnError func(plugin string, hook pub.Hook, err error)
	// Log receives one line for each client/generate call. nil: slog.Default().
	Log *slog.Logger
	// EventQueueSize bounds the per-plugin-instance event queue drained by
	// that instance's dedicated sender goroutine (see Host.Emit). Defaults
	// to defaultEventQueueSize when <= 0.
	EventQueueSize int
}

// Host manages plugin processes on the harness side and dispatches hooks.
// Sync hooks chain across plugins in Spec order: each plugin sees the
// previous plugin's mutations.
type Host struct {
	opts      Options
	instances []*instance
}

// NewHost creates a Host from cached manifests. Nothing is spawned.
func NewHost(opts Options, specs ...Spec) (*Host, error) {
	if opts.HookTimeout <= 0 {
		opts.HookTimeout = 5 * time.Second
	}
	if opts.EventQueueSize <= 0 {
		opts.EventQueueSize = defaultEventQueueSize
	}
	h, _, err := (&Host{opts: opts}).Replace(specs, nil)
	return h, err
}

// Replace returns a Host of specs, in chain order. When keep reports the name of a spec and h has a plugin of that
// name, the new Host carries over that instance as it is, and the spec is not read. Every other spec starts a new
// instance. h stays usable: a tool call that holds it finishes on its own instances. The returned func stops the
// instances of h that the new Host lacks. Nothing is spawned.
func (h *Host) Replace(specs []Spec, keep func(name string) bool) (*Host, func(), error) {
	n := &Host{opts: h.opts}
	seen := make(map[string]bool)
	carried := make(map[*instance]bool)
	for _, spec := range specs {
		name := spec.Manifest.Name
		if seen[name] {
			return nil, nil, fmt.Errorf("plugin %s: duplicate name", name)
		}
		seen[name] = true
		if old := h.instance(name); old != nil && keep != nil && keep(name) {
			carried[old] = true
			n.instances = append(n.instances, old)
			continue
		}
		if name == "" {
			return nil, nil, fmt.Errorf("plugin: spec missing manifest (run Probe at install time)")
		}
		if spec.Manifest.ProtocolVersion != pub.ProtocolVersion {
			return nil, nil, fmt.Errorf("plugin %s: protocol version %d, harness speaks %d", name, spec.Manifest.ProtocolVersion, pub.ProtocolVersion)
		}
		if err := validateManifest(spec.Manifest); err != nil {
			return nil, nil, err
		}
		n.instances = append(n.instances, &instance{host: n, spec: spec})
	}
	return n, func() {
		for _, inst := range h.instances {
			if !carried[inst] {
				inst.stop()
			}
		}
	}, nil
}

func (h *Host) instance(name string) *instance {
	for _, inst := range h.instances {
		if inst.spec.Manifest.Name == name {
			return inst
		}
	}
	return nil
}

// Tools returns all plugin-provided tool definitions, for the model's tool
// list. It reads cached manifests only.
func (h *Host) Tools() []pub.ToolDef {
	var defs []pub.ToolDef
	for _, inst := range h.instances {
		defs = append(defs, inst.spec.Manifest.Tools...)
	}
	return defs
}

// Plugin spawn states reported by Host.Plugins. A plugin process spawns
// lazily on its first hook dispatch or tool call, so PluginNotSpawned is the
// normal state for a configured plugin no turn has used yet.
const (
	PluginNotSpawned = "not-spawned"
	PluginRunning    = "running"
	PluginErrored    = "errored"
	PluginStopped    = "stopped"
)

// Info is a read-only snapshot of one configured plugin's registration: its
// name, current spawn state, the tools it registers, and the hook names it
// subscribes to. It describes CONFIGURED plugins from the cached manifest, so
// a plugin that has not spawned yet still appears (state PluginNotSpawned).
// Tools and Hooks are never nil, so the value serializes to a JSON array,
// never null.
type Info struct {
	Name  string   `json:"name"`
	State string   `json:"state"`
	Tools []string `json:"tools"`
	Hooks []string `json:"hooks"`
}

// Plugins returns one Info per configured plugin, in Spec order. It reads the
// cached manifest for each plugin's name, tools, and hooks, and each
// instance's live spawn state. Nothing is spawned.
func (h *Host) Plugins() []Info {
	out := make([]Info, 0, len(h.instances))
	for _, inst := range h.instances {
		out = append(out, inst.info())
	}
	return out
}

// info snapshots one instance: manifest name/tools/hooks plus live spawn
// state. The state read never takes inst.mu — see instance.liveState.
func (inst *instance) info() Info {
	m := inst.spec.Manifest
	tools := make([]string, 0, len(m.Tools))
	for _, t := range m.Tools {
		tools = append(tools, t.Name)
	}
	hooks := make([]string, 0, len(m.Hooks))
	for _, hk := range m.Hooks {
		hooks = append(hooks, string(hk))
	}
	return Info{Name: m.Name, State: inst.liveState(), Tools: tools, Hooks: hooks}
}

// liveState reports inst's current spawn state without ever taking inst.mu,
// so a slow or wedged plugin spawn can never block a concurrent read.
// start holds inst.mu for the whole, possibly uncancellable dial-plus-
// handshake (see instance.start's doc comment); Host is a box-scoped
// singleton shared by every session on the box, so a state read gated on
// inst.mu would let one session's plugin spawn stall GET /session and the
// session_info tool for every OTHER session too — exactly what AGENTS.md's
// "a hung plugin can't wedge a session" rule forbids. start and stop are
// the only writers of inst.state/inst.liveConn; both happen to hold
// inst.mu when they store, which is fine, since a plain atomic store never
// blocks on anything. What must hold is the read side: this function loads
// the atomics directly and never takes inst.mu itself, so it can never be
// made to wait on a start that is still holding inst.mu through a slow or
// wedged spawn.
func (inst *instance) liveState() string {
	switch reportedState(inst.state.Load()) {
	case stateNotSpawned:
		return PluginNotSpawned
	case stateStopped:
		return PluginStopped
	case stateErrored:
		return PluginErrored
	case stateRunning:
		// A plugin that spawned successfully can still die later (crash,
		// killed externally). conn.closed is closed both by an explicit
		// stop and by the read loop's own error path (conn.fail, called
		// from conn.run when the stream ends) — reusing that existing
		// signal here needs no new death-detection goroutine.
		if c := inst.liveConn.Load(); c != nil {
			select {
			case <-c.Done():
				return PluginErrored
			default:
			}
		}
		return PluginRunning
	default:
		// Unreachable given today's four-value enum. An explicit case
		// per value (rather than falling through to running by default)
		// makes a future added-but-unhandled reportedState a visible
		// omission here instead of a silent "running" misreport.
		return PluginNotSpawned
	}
}

// Close shuts down all running plugin processes.
func (h *Host) Close() {
	for _, inst := range h.instances {
		inst.stop()
	}
}

// Emit fans an event batch out to subscribed plugins, fire-and-forget. It
// never blocks the caller: each subscribed plugin instance has its own
// bounded queue, and Emit enqueues onto it with a non-blocking send. When a
// queue is full the event is dropped (counted; see Host.EventsDropped) and
// the first drop for that instance is reported once via Options.OnError so
// operators notice — see PROTOCOL.md for the documented drop semantics.
//
// Delivery to a single plugin instance is FIFO in the order Emit was called
// for it: one dedicated sender goroutine per instance drains its queue, so
// concurrent Emit callers can never race for the connection write and
// reorder events (the original bug this replaces: a fresh goroutine per
// plugin per event, racing for the write mutex). Callers that need
// happens-before ordering between two events for the same plugin (e.g.
// tool.execute.start before tool.execute.end for one call id) must call
// Emit for them, in order, from the same goroutine — which is how the
// runtime emits events.
//
// Plugins subscribed to events are started (lazily) by their sender
// goroutine on its first dequeued event.
func (h *Host) Emit(events []pub.Event) {
	batch := &pub.EventBatch{Events: events}
	for _, inst := range h.instances {
		if !inst.subscribes(pub.HookEvent) {
			continue
		}
		inst.enqueueEvent(h, batch)
	}
}

// EventsDropped returns the number of events dropped for pluginName because
// its event queue was full when Emit tried to enqueue. Zero for plugins
// that were never throttled, or that don't exist.
func (h *Host) EventsDropped(pluginName string) uint64 {
	for _, inst := range h.instances {
		if inst.spec.Manifest.Name == pluginName {
			return inst.eventDropped.Load()
		}
	}
	return 0
}

// NotificationsDropped returns the number of low-level JSON-RPC
// notifications dropped on pluginName's connection because its receive
// queue (conn.notifyCh) was saturated when they arrived — see
// conn.dispatchNotification. Zero for plugins that were never throttled,
// that don't exist, or whose connection was never started.
//
// This stays at zero under normal operation: of the two notification
// methods this protocol defines, hook/event and shutdown flow from the
// harness to the plugin, never the other way (see PROTOCOL.md's method
// tables), so a well-behaved plugin process never sends the harness a
// notification at all. NotificationsDropped exists as the defensive,
// symmetric counterpart to EventsDropped anyway: dispatchNotification's
// non-blocking-send-or-drop rule applies to any conn regardless of which
// side of the protocol it serves, so even a malformed or adversarial
// plugin that emits its own notification-shaped messages back at the
// harness cannot wedge Host's read loop by flooding it — and this makes
// that fact observable and testable rather than merely asserted.
func (h *Host) NotificationsDropped(pluginName string) uint64 {
	for _, inst := range h.instances {
		if inst.spec.Manifest.Name != pluginName {
			continue
		}
		inst.mu.Lock()
		c := inst.conn
		inst.mu.Unlock()
		if c == nil {
			return 0
		}
		return c.NotifyDropped()
	}
	return 0
}

// SystemTransform collects system prompt segments from all subscribed
// plugins, in chain order.
func (h *Host) SystemTransform(ctx context.Context, req *pub.SystemTransformRequest) []string {
	var segments []string
	dispatchChain(ctx, h, pub.HookSystemTransform, req, func(_ *pub.SystemTransformRequest, resp *pub.SystemTransformResponse) bool {
		segments = append(segments, resp.Segments...)
		return true
	}, nil)
	return segments
}

// ShellEnv merges env additions from all subscribed plugins. Later plugins
// override earlier ones on key conflicts.
func (h *Host) ShellEnv(ctx context.Context, req *pub.ShellEnvRequest) map[string]string {
	env := make(map[string]string)
	dispatchChain(ctx, h, pub.HookShellEnv, req, func(_ *pub.ShellEnvRequest, resp *pub.ShellEnvResponse) bool {
		for k, v := range resp.Env {
			env[k] = v
		}
		return true
	}, nil)
	return env
}

// ErrNoTool says that no plugin of the Host provides the tool.
var ErrNoTool = errors.New("plugin: no plugin provides tool")

// StartTool starts the plugin of tool and returns the sender of its tool/execute request, so a deadline can cover the request only.
func (h *Host) StartTool(ctx context.Context, tool string) (func(context.Context, *pub.ToolExecuteRequest) (*pub.ToolExecuteResponse, error), error) {
	for _, inst := range h.instances {
		for _, def := range inst.spec.Manifest.Tools {
			if def.Name == tool {
				sctx, cancel := context.WithTimeout(ctx, startTimeout)
				defer cancel()
				c, err := inst.start(sctx)
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context, req *pub.ToolExecuteRequest) (*pub.ToolExecuteResponse, error) {
					var resp toolExecuteResponse
					if err := c.Call(ctx, wire.MethodToolExecute, req, &resp); err != nil {
						return nil, err
					}
					return &resp.ToolExecuteResponse, nil
				}, nil
			}
		}
	}
	return nil, fmt.Errorf("%w %q", ErrNoTool, tool)
}

// reportedState is instance.state's value space: the spawn lifecycle a
// Host.Plugins read reports, published lock-free (see instance.liveState).
// The zero value is stateNotSpawned, matching a freshly constructed
// instance before anything has dispatched to it.
type reportedState int32

const (
	stateNotSpawned reportedState = iota
	stateRunning
	stateErrored
	stateStopped
)

// instance is one plugin process, spawned lazily and kept warm.
type instance struct {
	host *Host
	spec Spec

	mu      sync.Mutex
	started bool
	stopped bool
	err     error
	conn    *wire.Conn
	cmd     *exec.Cmd

	// state is inst's reportedState. start and stop are its only writers,
	// and both happen to hold mu when they store it — that part is
	// incidental, not required. What matters is the READ side:
	// instance.liveState (Host.Plugins) loads this atomic directly and
	// never takes mu, so it can never be blocked by a start that is still
	// holding mu through a slow or wedged spawn. See liveState's doc
	// comment for why that matters.
	state atomic.Int32
	// liveConn mirrors conn once a spawn succeeds, published by the same
	// atomic store as state, under the same "writers may hold mu, the
	// reader never does" rule. liveState uses it to notice a POST-spawn
	// death (conn.closed, closed by conn.fail when the read loop errors).
	liveConn atomic.Pointer[wire.Conn]
	// liveDeny is the deny_on_failure list of the live manifest, stored at
	// spawn before the manifest comparison, so a mismatch still flags.
	liveDeny atomic.Pointer[[]pub.Hook]

	// Event delivery: a bounded queue drained by one dedicated sender
	// goroutine, created on the first Emit for this instance and exited
	// when the instance is stopped. Guarded by its own eventMu — never mu
	// — because the sender goroutine calls start(), which holds mu for
	// the entire (possibly slow, possibly wedged) handshake; Emit must be
	// able to enqueue without ever waiting on that.
	eventMu      sync.Mutex
	eventCh      chan *pub.EventBatch
	eventStop    chan struct{}
	eventStopped bool
	eventDropped atomic.Uint64
}

func (inst *instance) subscribes(hook pub.Hook) bool {
	for _, h := range inst.spec.Manifest.Hooks {
		if h == hook {
			return true
		}
	}
	return false
}

// flags reports whether the cached manifest or the live manifest seen at
// spawn marks hook as deny-on-failure.
func (inst *instance) flags(hook pub.Hook) bool {
	if slices.Contains(inst.spec.Manifest.DenyOnFailure, hook) {
		return true
	}
	live := inst.liveDeny.Load()
	return live != nil && slices.Contains(*live, hook)
}

// enqueueEvent enqueues batch for delivery to inst, lazily starting its
// dedicated sender goroutine on first use. The send is always non-blocking:
// on a full queue (or once the instance has been stopped) the event is
// dropped and counted, and the first drop is reported via Host.OnError.
func (inst *instance) enqueueEvent(h *Host, batch *pub.EventBatch) {
	inst.eventMu.Lock()
	if inst.eventStopped {
		// Host.Close was already called: never (re)spawn a sender.
		inst.eventMu.Unlock()
		inst.recordEventDrop(h)
		return
	}
	if inst.eventCh == nil {
		inst.eventCh = make(chan *pub.EventBatch, h.opts.EventQueueSize)
		inst.eventStop = make(chan struct{})
		go inst.runEventSender(h, inst.eventCh, inst.eventStop)
	}
	ch := inst.eventCh
	inst.eventMu.Unlock()

	select {
	case ch <- batch:
	default:
		inst.recordEventDrop(h)
	}
}

func (inst *instance) recordEventDrop(h *Host) {
	if inst.eventDropped.Add(1) == 1 {
		h.onError(inst, pub.HookEvent, fmt.Errorf(
			"plugin: event queue full (capacity %d): dropping events, see Host.EventsDropped for a running count",
			h.opts.EventQueueSize))
	}
}

// runEventSender drains inst's event queue one batch at a time, in enqueue
// order, so a single connection write mutex is never raced by concurrent
// goroutines and FIFO delivery per plugin follows directly from FIFO
// enqueue order. It exits once eventStop is closed (on Host.Close /
// instance.stop).
func (inst *instance) runEventSender(h *Host, ch chan *pub.EventBatch, stop chan struct{}) {
	for {
		select {
		case batch := <-ch:
			ctx, cancel := context.WithTimeout(context.Background(), h.opts.HookTimeout)
			c, err := inst.start(ctx)
			if err != nil {
				h.onError(inst, pub.HookEvent, err)
			} else if err := c.Notify(hookMethod(pub.HookEvent), batch); err != nil {
				h.onError(inst, pub.HookEvent, err)
			}
			cancel()
		case <-stop:
			return
		}
	}
}

// startTimeout bounds a start that a tool call triggers.
const startTimeout = 30 * time.Second

// errInstanceStopped is returned by start once the instance has been (or is
// being) stopped, so a start racing a stop gets a definitive error instead
// of a stale success paired with a nil conn.
var errInstanceStopped = errors.New("plugin: instance stopped")

// start spawns and initializes the plugin process on first use. A failed
// start is sticky: the plugin is skipped for the rest of the session rather
// than respawned on every dispatch. Only a start that fails with a canceled
// ctx is not sticky: the next dispatch starts again. A deadline is still
// latched, because the start ctx always has a deadline, so a parent deadline
// cannot be told from a hung start. Once stopped, an instance never
// (re)spawns: start returns errInstanceStopped instead.
//
// It returns the conn established under inst.mu, alongside any error.
// Callers must use that returned handle for the RPC that follows — never
// re-read inst.conn afterwards, since inst.mu is released by the time start
// returns and a concurrent stop() (or a future start()) can nil or replace
// it out from under an unsynchronized read.
func (inst *instance) start(ctx context.Context) (*wire.Conn, error) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.stopped {
		return nil, errInstanceStopped
	}
	if inst.started {
		return inst.conn, inst.err
	}
	err := inst.startLocked(ctx)
	if err != nil && errors.Is(ctx.Err(), context.Canceled) {
		return nil, err
	}
	inst.started = true
	inst.err = err
	if inst.err != nil {
		inst.state.Store(int32(stateErrored))
	} else {
		inst.state.Store(int32(stateRunning))
		inst.liveConn.Store(inst.conn)
	}
	return inst.conn, inst.err
}

func (inst *instance) startLocked(ctx context.Context) error {
	rwc, err := inst.dial()
	if err != nil {
		return fmt.Errorf("plugin %s: %w", inst.spec.Manifest.Name, err)
	}
	c := wire.NewConn(rwc, inst.host.clientHandler(inst.spec.Manifest.Name))
	go c.Run() //nolint:errcheck // stream end is surfaced via pending calls

	var live pub.Manifest
	init := &pub.InitializeParams{
		ProtocolVersion: pub.ProtocolVersion,
		HarnessVersion:  inst.host.opts.HarnessVersion,
		WorkspaceDir:    inst.host.opts.WorkspaceDir,
		HTTPHeaders:     inst.host.opts.HTTPHeaders,
		Config:          inst.spec.Config,
		ServeURL:        inst.host.opts.ServeURL,
		RunToken:        inst.host.opts.RunToken,
	}
	if err := c.Call(ctx, wire.MethodInitialize, init, &live); err != nil {
		_ = c.Close()
		return fmt.Errorf("plugin %s: initialize: %w", inst.spec.Manifest.Name, err)
	}
	inst.liveDeny.Store(&live.DenyOnFailure)
	if !sameManifest(inst.spec.Manifest, live) {
		_ = c.Close()
		return fmt.Errorf("plugin %s: live manifest disagrees with cache in %s — reinstall the plugin",
			inst.spec.Manifest.Name, diffManifest(inst.spec.Manifest, live))
	}
	inst.conn = c
	return nil
}

func (inst *instance) dial() (io.ReadWriteCloser, error) {
	if inst.spec.dial != nil {
		return inst.spec.dial()
	}
	if len(inst.spec.Command) == 0 {
		return nil, fmt.Errorf("no command configured")
	}
	resolved, err := ResolveExecutable(inst.spec.Command, inst.spec.Dir)
	if err != nil {
		return nil, err
	}
	// Path is the resolved executable (guaranteed to be the same file the
	// manifest cache hashed — see ResolveExecutable); Args[0] stays the
	// configured name, matching exec.Command's own convention ("Args[0] is
	// always name, not the possibly resolved Path").
	cmd := &exec.Cmd{
		Path: resolved,
		Args: append([]string{inst.spec.Command[0]}, inst.spec.Command[1:]...),
	}
	cmd.Dir = inst.spec.Dir
	cmd.Env = append(os.Environ(), inst.spec.Env...)
	cmd.Stderr = os.Stderr // plugin logging
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	inst.cmd = cmd
	return &procConn{stdin: stdin, stdout: stdout, cmd: cmd}, nil
}

func (inst *instance) stop() {
	// Signal the event sender first, and via a lock independent of mu, so
	// a stop can never be stuck behind a wedged handshake.
	inst.eventMu.Lock()
	inst.eventStopped = true
	if inst.eventStop != nil {
		close(inst.eventStop)
	}
	inst.eventMu.Unlock()

	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.stopped = true
	// Only a plugin that was actually RUNNING transitions to stopped here.
	// A never-started instance stays not-spawned (spawn state outranks the
	// box-wide stopped flag: a plugin no turn ever used is never
	// misreported as having run and then stopped), and a plugin whose
	// spawn itself failed stays errored (Close didn't stop anything —
	// there was nothing running to stop — so relabeling it stopped would
	// erase the failure this state tracking exists to preserve).
	//
	// A running instance can ALSO already be dead: liveState's running
	// case computes that lazily, by folding conn.closed at read time
	// (closed both by an explicit stop and by the read loop's own error
	// path, conn.fail), rather than writing the death back to inst.state.
	// Check that same signal here, before liveConn is nil'd out below —
	// once it is, liveState can never fire that fold again — so a crash
	// that happened before this Close call still lands on stateErrored
	// instead of being relabeled a clean stateStopped.
	if reportedState(inst.state.Load()) == stateRunning {
		next := stateStopped
		if c := inst.liveConn.Load(); c != nil {
			select {
			case <-c.Done():
				next = stateErrored
			default:
			}
		}
		inst.state.Store(int32(next))
	}
	inst.liveConn.Store(nil)
	if inst.conn != nil {
		_ = inst.conn.Notify(wire.MethodShutdown, struct{}{})
		_ = inst.conn.Close()
		inst.conn = nil
	}
}

// procConn adapts a child process's stdin/stdout to an io.ReadWriteCloser.
type procConn struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	cmd    *exec.Cmd
}

func (p *procConn) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *procConn) Write(b []byte) (int, error) { return p.stdin.Write(b) }

func (p *procConn) Close() error {
	p.stdin.Close()
	done := make(chan struct{})
	go func() {
		p.cmd.Wait() //nolint:errcheck
		close(done)
	}()
	kill := time.NewTimer(2 * time.Second)
	defer kill.Stop()
	select {
	case <-done:
	case <-kill.C:
		_ = p.cmd.Process.Kill()
		<-done
	}
	return nil
}

// ProbeSpec spawns a plugin binary using the full spec — command, Env, Dir,
// and Config — performs the initialize handshake, and returns its manifest.
// This is the bounded probe used by `harness plugin probe` and by a run/serve
// cache miss. The command layer records executable identity and plugin-spec
// identity so a changed binary or spec is re-probed.
//
// Probing with the full spec (rather than the bare command — see Probe)
// matters: a plugin can behave differently (even report a different
// manifest) depending on Env, Dir, or Config, and the real spawn (Host's
// instance.dial/startLocked) always supplies all three. Probing with less
// than the real spawn does risks caching a manifest the live plugin
// wouldn't actually advertise.
func ProbeSpec(ctx context.Context, spec Spec) (pub.Manifest, error) {
	inst := &instance{
		host: &Host{opts: Options{HookTimeout: 5 * time.Second}},
		spec: Spec{
			Command:  spec.Command,
			Env:      spec.Env,
			Dir:      spec.Dir,
			Config:   spec.Config,
			Manifest: pub.Manifest{Name: "probe", ProtocolVersion: pub.ProtocolVersion},
			dial:     spec.dial,
		},
	}
	rwc, err := inst.dial()
	if err != nil {
		return pub.Manifest{}, err
	}
	c := wire.NewConn(rwc, inst.host.clientHandler(inst.spec.Manifest.Name))
	go c.Run() //nolint:errcheck
	defer func() { _ = c.Close() }()

	var m pub.Manifest
	init := &pub.InitializeParams{ProtocolVersion: pub.ProtocolVersion, Config: spec.Config}
	if err := c.Call(ctx, wire.MethodInitialize, init, &m); err != nil {
		return pub.Manifest{}, fmt.Errorf("plugin probe: %w", err)
	}
	_ = c.Notify(wire.MethodShutdown, struct{}{})
	return m, nil
}

// Probe spawns a plugin binary given only its command — no Env, Dir, or
// Config — performs the initialize handshake, and returns its manifest.
// Kept for backward compatibility; prefer ProbeSpec, which probes with the
// same Env/Dir/Config a real spawn uses so the cached manifest can't diverge
// from what the live, fully-configured plugin actually advertises.
func Probe(ctx context.Context, command []string) (pub.Manifest, error) {
	return ProbeSpec(ctx, Spec{Command: command})
}
