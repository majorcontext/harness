package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/majorcontext/harness/message"
)

// defaultEventQueueSize is the per-plugin-instance bound on queued
// hook/event batches when Options.EventQueueSize is unset. See
// Host.Emit and PROTOCOL.md for drop semantics.
const defaultEventQueueSize = 256

// ClientAPI is implemented by the engine to serve plugin → harness calls.
type ClientAPI interface {
	SessionMessages(ctx context.Context, req *SessionMessagesRequest) (*SessionMessagesResponse, error)
	MCPCall(ctx context.Context, req *MCPCallRequest) (*MCPCallResult, error)
	Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error)
}

// ErrMCPNotImplemented is the error a ClientAPI.MCPCall implementation
// should return until MCP engine integration lands (tracked as a separate
// PR): a clear, typed failure rather than a panic or a silently empty
// result. Both bundled ClientAPI implementations (server-backed, and the
// direct engine-backed adapter used by `harness run`) return it verbatim.
var ErrMCPNotImplemented = errors.New("plugin: MCP call not implemented yet (MCP engine integration is a separate PR)")

// ErrGenerateNotImplemented is the analogous sentinel for
// ClientAPI.Generate: routing a plugin's LLM call through the harness
// provider layer is not yet wired. Returned instead of panicking or
// silently returning an empty message.
var ErrGenerateNotImplemented = errors.New("plugin: generate not implemented yet (provider-layer integration is a separate PR)")

// Spec configures one plugin for a Host.
//
// Manifest comes from the install-time cache (see Probe), which is what lets
// the Host route hooks and advertise tools without spawning anything: a
// plugin process starts on its first hook dispatch or tool call, then stays
// warm.
type Spec struct {
	Command []string
	Env     []string // appended to the harness environment
	Dir     string
	// Config is this plugin's block from the harness config, passed
	// verbatim in InitializeParams.
	Config   json.RawMessage
	Manifest Manifest

	// dial overrides process spawning; used by tests.
	dial func() (io.ReadWriteCloser, error)
}

// Options configures a Host.
type Options struct {
	HarnessVersion string
	WorkspaceDir   string
	// HTTPHeaders are stamped on all plugin outbound HTTP traffic via
	// Client.HTTPClient (e.g. workspace attribution headers).
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
	OnError func(plugin string, hook Hook, err error)
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
	h := &Host{opts: opts}
	seen := make(map[string]bool)
	for _, spec := range specs {
		if spec.Manifest.Name == "" {
			return nil, fmt.Errorf("plugin: spec missing manifest (run Probe at install time)")
		}
		if spec.Manifest.ProtocolVersion != ProtocolVersion {
			return nil, fmt.Errorf("plugin %s: protocol version %d, harness speaks %d",
				spec.Manifest.Name, spec.Manifest.ProtocolVersion, ProtocolVersion)
		}
		if seen[spec.Manifest.Name] {
			return nil, fmt.Errorf("plugin %s: duplicate name", spec.Manifest.Name)
		}
		seen[spec.Manifest.Name] = true
		h.instances = append(h.instances, &instance{host: h, spec: spec})
	}
	return h, nil
}

// Tools returns all plugin-provided tool definitions, for the model's tool
// list. It reads cached manifests only.
func (h *Host) Tools() []ToolDef {
	var defs []ToolDef
	for _, inst := range h.instances {
		defs = append(defs, inst.spec.Manifest.Tools...)
	}
	return defs
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
// engine already emits events.
//
// Plugins subscribed to events are started (lazily) by their sender
// goroutine on its first dequeued event.
func (h *Host) Emit(events []Event) {
	batch := &EventBatch{Events: events}
	for _, inst := range h.instances {
		if !inst.subscribes(HookEvent) {
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
		return c.notifyDropped.Load()
	}
	return 0
}

// ChatParams runs the chat.params chain and returns the final params.
func (h *Host) ChatParams(ctx context.Context, req *ChatParamsRequest) ChatParams {
	dispatchChain(ctx, h, HookChatParams, req, func(req *ChatParamsRequest, resp *ChatParamsResponse) bool {
		req.Params = resp.Params
		return true
	})
	return req.Params
}

// ChatMessage runs the chat.message chain and returns the final message.
func (h *Host) ChatMessage(ctx context.Context, req *ChatMessageRequest) message.Message {
	dispatchChain(ctx, h, HookChatMessage, req, func(req *ChatMessageRequest, resp *ChatMessageResponse) bool {
		req.Message = resp.Message
		return true
	})
	return req.Message
}

// SystemTransform collects system prompt segments from all subscribed
// plugins, in chain order.
func (h *Host) SystemTransform(ctx context.Context, req *SystemTransformRequest) []string {
	var segments []string
	dispatchChain(ctx, h, HookSystemTransform, req, func(_ *SystemTransformRequest, resp *SystemTransformResponse) bool {
		segments = append(segments, resp.Segments...)
		return true
	})
	return segments
}

// ShellEnv merges env additions from all subscribed plugins. Later plugins
// override earlier ones on key conflicts.
func (h *Host) ShellEnv(ctx context.Context, req *ShellEnvRequest) map[string]string {
	env := make(map[string]string)
	dispatchChain(ctx, h, HookShellEnv, req, func(_ *ShellEnvRequest, resp *ShellEnvResponse) bool {
		for k, v := range resp.Env {
			env[k] = v
		}
		return true
	})
	return env
}

// ToolExecuteBefore runs the tool.execute.before chain. It returns the
// (possibly rewritten) args, or a non-empty deny message if a plugin blocked
// the call — in which case the chain stops and the tool must not run.
func (h *Host) ToolExecuteBefore(ctx context.Context, req *ToolExecuteBeforeRequest) (args json.RawMessage, deny string) {
	dispatchChain(ctx, h, HookToolExecuteBefore, req, func(req *ToolExecuteBeforeRequest, resp *ToolExecuteBeforeResponse) bool {
		if resp.Deny != "" {
			deny = resp.Deny
			return false
		}
		if resp.Args != nil {
			req.Args = resp.Args
		}
		return true
	})
	return req.Args, deny
}

// ToolExecuteAfter runs the tool.execute.after chain and returns the final
// output.
func (h *Host) ToolExecuteAfter(ctx context.Context, req *ToolExecuteAfterRequest) message.Parts {
	dispatchChain(ctx, h, HookToolExecuteAfter, req, func(req *ToolExecuteAfterRequest, resp *ToolExecuteAfterResponse) bool {
		if resp.Output != nil {
			req.Output = resp.Output
		}
		return true
	})
	return req.Output
}

// ExecuteTool routes a plugin-provided tool call to the plugin that declares
// it in its manifest.
func (h *Host) ExecuteTool(ctx context.Context, req *ToolExecuteRequest) (*ToolExecuteResponse, error) {
	for _, inst := range h.instances {
		for _, def := range inst.spec.Manifest.Tools {
			if def.Name == req.Tool {
				c, err := inst.start(ctx)
				if err != nil {
					return nil, err
				}
				var resp ToolExecuteResponse
				if err := c.call(ctx, methodToolExecute, req, &resp); err != nil {
					return nil, err
				}
				return &resp, nil
			}
		}
	}
	return nil, fmt.Errorf("plugin: no plugin provides tool %q", req.Tool)
}

// dispatchChain runs one sync hook across subscribed plugins in order. apply
// folds each response into the shared request; returning false stops the
// chain. Failures (including per-dispatch deadline) skip the plugin and
// continue: hook chains fail open, matching the design rule that a plugin
// must never wedge a session.
func dispatchChain[Req, Resp any](ctx context.Context, h *Host, hook Hook, req *Req, apply func(*Req, *Resp) bool) {
	for _, inst := range h.instances {
		if !inst.subscribes(hook) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, h.opts.HookTimeout)
		c, err := inst.start(cctx)
		if err == nil {
			var resp Resp
			if err = c.call(cctx, hook.method(), req, &resp); err == nil {
				cancel()
				if !apply(req, &resp) {
					return
				}
				continue
			}
		}
		cancel()
		h.onError(inst, hook, err)
	}
}

func (h *Host) onError(inst *instance, hook Hook, err error) {
	if h.opts.OnError != nil {
		h.opts.OnError(inst.spec.Manifest.Name, hook, err)
	}
}

// instance is one plugin process, spawned lazily and kept warm.
type instance struct {
	host *Host
	spec Spec

	mu      sync.Mutex
	started bool
	stopped bool
	err     error
	conn    *conn
	cmd     *exec.Cmd

	// Event delivery: a bounded queue drained by one dedicated sender
	// goroutine, created on the first Emit for this instance and exited
	// when the instance is stopped. Guarded by its own eventMu — never mu
	// — because the sender goroutine calls start(), which holds mu for
	// the entire (possibly slow, possibly wedged) handshake; Emit must be
	// able to enqueue without ever waiting on that.
	eventMu      sync.Mutex
	eventCh      chan *EventBatch
	eventStop    chan struct{}
	eventStopped bool
	eventDropped atomic.Uint64
}

func (inst *instance) subscribes(hook Hook) bool {
	for _, h := range inst.spec.Manifest.Hooks {
		if h == hook {
			return true
		}
	}
	return false
}

// enqueueEvent enqueues batch for delivery to inst, lazily starting its
// dedicated sender goroutine on first use. The send is always non-blocking:
// on a full queue (or once the instance has been stopped) the event is
// dropped and counted, and the first drop is reported via Host.OnError.
func (inst *instance) enqueueEvent(h *Host, batch *EventBatch) {
	inst.eventMu.Lock()
	if inst.eventStopped {
		// Host.Close was already called: never (re)spawn a sender.
		inst.eventMu.Unlock()
		inst.recordEventDrop(h)
		return
	}
	if inst.eventCh == nil {
		inst.eventCh = make(chan *EventBatch, h.opts.EventQueueSize)
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
		h.onError(inst, HookEvent, fmt.Errorf(
			"plugin: event queue full (capacity %d): dropping events, see Host.EventsDropped for a running count",
			h.opts.EventQueueSize))
	}
}

// runEventSender drains inst's event queue one batch at a time, in enqueue
// order, so a single connection write mutex is never raced by concurrent
// goroutines and FIFO delivery per plugin follows directly from FIFO
// enqueue order. It exits once eventStop is closed (on Host.Close /
// instance.stop).
func (inst *instance) runEventSender(h *Host, ch chan *EventBatch, stop chan struct{}) {
	for {
		select {
		case batch := <-ch:
			ctx, cancel := context.WithTimeout(context.Background(), h.opts.HookTimeout)
			c, err := inst.start(ctx)
			if err != nil {
				h.onError(inst, HookEvent, err)
			} else if err := c.notify(HookEvent.method(), batch); err != nil {
				h.onError(inst, HookEvent, err)
			}
			cancel()
		case <-stop:
			return
		}
	}
}

// errInstanceStopped is returned by start once the instance has been (or is
// being) stopped, so a start racing a stop gets a definitive error instead
// of a stale success paired with a nil conn.
var errInstanceStopped = errors.New("plugin: instance stopped")

// start spawns and initializes the plugin process on first use. A failed
// start is sticky: the plugin is skipped for the rest of the session rather
// than respawned on every dispatch. Once stopped, an instance never
// (re)spawns: start returns errInstanceStopped instead.
//
// It returns the conn established under inst.mu, alongside any error.
// Callers must use that returned handle for the RPC that follows — never
// re-read inst.conn afterwards, since inst.mu is released by the time start
// returns and a concurrent stop() (or a future start()) can nil or replace
// it out from under an unsynchronized read.
func (inst *instance) start(ctx context.Context) (*conn, error) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.stopped {
		return nil, errInstanceStopped
	}
	if inst.started {
		return inst.conn, inst.err
	}
	inst.started = true
	inst.err = inst.startLocked(ctx)
	return inst.conn, inst.err
}

func (inst *instance) startLocked(ctx context.Context) error {
	rwc, err := inst.dial()
	if err != nil {
		return fmt.Errorf("plugin %s: %w", inst.spec.Manifest.Name, err)
	}
	c := newConn(rwc, inst.host.clientHandler())
	go c.run() //nolint:errcheck // stream end is surfaced via pending calls

	var live Manifest
	init := &InitializeParams{
		ProtocolVersion: ProtocolVersion,
		HarnessVersion:  inst.host.opts.HarnessVersion,
		WorkspaceDir:    inst.host.opts.WorkspaceDir,
		HTTPHeaders:     inst.host.opts.HTTPHeaders,
		Config:          inst.spec.Config,
		ServeURL:        inst.host.opts.ServeURL,
		RunToken:        inst.host.opts.RunToken,
	}
	if err := c.call(ctx, methodInitialize, init, &live); err != nil {
		c.close()
		return fmt.Errorf("plugin %s: initialize: %w", inst.spec.Manifest.Name, err)
	}
	if live.Name != inst.spec.Manifest.Name || live.ProtocolVersion != inst.spec.Manifest.ProtocolVersion {
		c.close()
		return fmt.Errorf("plugin %s: live manifest (%s, v%d) disagrees with cache — reinstall the plugin",
			inst.spec.Manifest.Name, live.Name, live.ProtocolVersion)
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
	if inst.conn != nil {
		_ = inst.conn.notify(methodShutdown, struct{}{})
		_ = inst.conn.close()
		inst.conn = nil
	}
}

// clientHandler serves plugin → harness API calls.
func (h *Host) clientHandler() handlerFunc {
	return func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if h.opts.Client == nil {
			return nil, &rpcError{Code: codeMethodNotFound, Message: "client API not available"}
		}
		switch method {
		case methodSessionMessages:
			var req SessionMessagesRequest
			if err := json.Unmarshal(params, &req); err != nil {
				return nil, err
			}
			return h.opts.Client.SessionMessages(ctx, &req)
		case methodMCPCall:
			var req MCPCallRequest
			if err := json.Unmarshal(params, &req); err != nil {
				return nil, err
			}
			return h.opts.Client.MCPCall(ctx, &req)
		case methodGenerate:
			var req GenerateRequest
			if err := json.Unmarshal(params, &req); err != nil {
				return nil, err
			}
			return h.opts.Client.Generate(ctx, &req)
		default:
			return nil, &rpcError{Code: codeMethodNotFound, Message: fmt.Sprintf("unknown method %q", method)}
		}
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

// NewTestSpec builds a Spec that runs an in-process fake plugin — over a
// net.Pipe, no subprocess — speaking the real protocol with the given
// hooks. It exists so integration tests in other packages (e.g. server,
// which cannot reach Spec's unexported dial field) can drive a real Host
// dispatch end-to-end without spawning a binary, per the "no subprocess
// fixtures unless the subprocess machinery itself is under test" testing
// rule. serve() fills in the manifest's hooks/tools/protocol version exactly
// as a real plugin process would.
func NewTestSpec(name string, hooks *Hooks) Spec {
	m := Manifest{Name: name, ProtocolVersion: ProtocolVersion, Hooks: hooks.hookList()}
	for _, tool := range hooks.Tools {
		m.Tools = append(m.Tools, tool.Def)
	}
	return Spec{
		Manifest: m,
		dial: func() (io.ReadWriteCloser, error) {
			hostSide, pluginSide := net.Pipe()
			go serve(pluginSide, Manifest{Name: name}, hooks) //nolint:errcheck
			return hostSide, nil
		},
	}
}

// ProbeSpec spawns a plugin binary using the full spec — command, Env, Dir,
// and Config — performs the initialize handshake, and returns its manifest.
// This is the install-time step ("harness plugin install") that populates
// the manifest cache; the cache should be keyed by binary hash so a changed
// binary is re-probed.
//
// Probing with the full spec (rather than the bare command — see Probe)
// matters: a plugin can behave differently (even report a different
// manifest) depending on Env, Dir, or Config, and the real spawn (Host's
// instance.dial/startLocked) always supplies all three. Probing with less
// than the real spawn does risks caching a manifest the live plugin
// wouldn't actually advertise.
func ProbeSpec(ctx context.Context, spec Spec) (Manifest, error) {
	inst := &instance{
		host: &Host{opts: Options{HookTimeout: 5 * time.Second}},
		spec: Spec{
			Command:  spec.Command,
			Env:      spec.Env,
			Dir:      spec.Dir,
			Config:   spec.Config,
			Manifest: Manifest{Name: "probe", ProtocolVersion: ProtocolVersion},
			dial:     spec.dial,
		},
	}
	rwc, err := inst.dial()
	if err != nil {
		return Manifest{}, err
	}
	c := newConn(rwc, inst.host.clientHandler())
	go c.run() //nolint:errcheck
	defer c.close()

	var m Manifest
	init := &InitializeParams{ProtocolVersion: ProtocolVersion, Config: spec.Config}
	if err := c.call(ctx, methodInitialize, init, &m); err != nil {
		return Manifest{}, fmt.Errorf("plugin probe: %w", err)
	}
	_ = c.notify(methodShutdown, struct{}{})
	return m, nil
}

// Probe spawns a plugin binary given only its command — no Env, Dir, or
// Config — performs the initialize handshake, and returns its manifest.
// Kept for backward compatibility; prefer ProbeSpec, which probes with the
// same Env/Dir/Config a real spawn uses so the cached manifest can't diverge
// from what the live, fully-configured plugin actually advertises.
func Probe(ctx context.Context, command []string) (Manifest, error) {
	return ProbeSpec(ctx, Spec{Command: command})
}
