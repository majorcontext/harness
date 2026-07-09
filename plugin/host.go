package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/majorcontext/harness/message"
)

// ClientAPI is implemented by the engine to serve plugin → harness calls.
type ClientAPI interface {
	SessionMessages(ctx context.Context, req *SessionMessagesRequest) (*SessionMessagesResponse, error)
	MCPCall(ctx context.Context, req *MCPCallRequest) (*MCPCallResult, error)
	Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error)
}

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
	// Client serves plugin → harness API calls. May be nil, in which case
	// those calls fail with method-not-found.
	Client ClientAPI
	// HookTimeout bounds each sync hook dispatch so a hung plugin cannot
	// wedge a session. Defaults to 5s.
	HookTimeout time.Duration
	// OnError observes per-plugin dispatch failures. Sync hook chains fail
	// open: the erroring plugin is skipped and the chain continues.
	OnError func(plugin string, hook Hook, err error)
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

// Emit fans an event batch out to subscribed plugins, fire-and-forget.
// Plugins subscribed to events are started on first emit.
func (h *Host) Emit(events []Event) {
	batch := &EventBatch{Events: events}
	for _, inst := range h.instances {
		if !inst.subscribes(HookEvent) {
			continue
		}
		go func(inst *instance) {
			ctx, cancel := context.WithTimeout(context.Background(), h.opts.HookTimeout)
			defer cancel()
			if err := inst.start(ctx); err != nil {
				h.onError(inst, HookEvent, err)
				return
			}
			if err := inst.conn.notify(HookEvent.method(), batch); err != nil {
				h.onError(inst, HookEvent, err)
			}
		}(inst)
	}
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
				if err := inst.start(ctx); err != nil {
					return nil, err
				}
				var resp ToolExecuteResponse
				if err := inst.conn.call(ctx, methodToolExecute, req, &resp); err != nil {
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
		err := inst.start(cctx)
		if err == nil {
			var resp Resp
			if err = inst.conn.call(cctx, hook.method(), req, &resp); err == nil {
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
	err     error
	conn    *conn
	cmd     *exec.Cmd
}

func (inst *instance) subscribes(hook Hook) bool {
	for _, h := range inst.spec.Manifest.Hooks {
		if h == hook {
			return true
		}
	}
	return false
}

// start spawns and initializes the plugin process on first use. A failed
// start is sticky: the plugin is skipped for the rest of the session rather
// than respawned on every dispatch.
func (inst *instance) start(ctx context.Context) error {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.started {
		return inst.err
	}
	inst.started = true
	inst.err = inst.startLocked(ctx)
	return inst.err
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
	inst.mu.Lock()
	defer inst.mu.Unlock()
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
