// Package pluginsrc gives the tools and tool hooks of the configured
// plugins to each session, and sends them the events of each session.
package pluginsrc

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/internal/plugin"
	"github.com/majorcontext/harness/internal/tool/mcpsrc"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/internal/turn"
	pub "github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/protocol"
)

const probeTimeout = 30 * time.Second

// History returns the conversation of a session.
type History func(ctx context.Context, sessionID string) ([]protocol.LogMessage, error)

// Blob opens the bytes that a session stores under key.
type Blob func(ctx context.Context, sessionID, key string) (io.ReadCloser, error)

// Plugins owns the live set of plugins of a runtime: the plugins of the config, which Set replaces, and the
// in-process plugins, which never change. All methods are safe for concurrent use.
type Plugins struct {
	log       *slog.Logger
	start     []config.PluginSpec
	inproc    []*pub.Plugin
	opts      plugin.Options
	workDir   string
	cachePath string

	// live is the set that sessions read. A Start or a Set replaces it whole.
	live   atomic.Pointer[live]
	mu     sync.Mutex
	closed bool

	// dmu guards seen and base. seen holds the first definition that the runtime read of each tool, and base
	// the tools that a session sent at its first tool read, both for the life of the process.
	dmu  sync.Mutex
	seen map[string]pub.ToolDef
	base map[string]firstRead

	serving sync.WaitGroup
	smu     sync.Mutex
	stopped bool
}

// live is one immutable plugin set.
type live struct {
	host *plugin.Host
	// specs are the config plugins of host.
	specs []config.PluginSpec
	// limits holds the tool_timeout_s of each config plugin tool.
	limits map[string]time.Duration
}

func (p *Plugins) spawn(fn func()) error {
	p.smu.Lock()
	defer p.smu.Unlock()
	if p.stopped {
		return errors.New("plugins are closed")
	}
	p.serving.Go(fn)
	return nil
}

// New returns the Plugins of cfg.Plugins and inproc. It does no I/O.
func New(cfg config.Config, inproc []*pub.Plugin, workDir string, calls Calls, serveURL, runToken string, log *slog.Logger) *Plugins {
	log = cmp.Or(log, slog.Default())
	onError := func(name string, hook pub.Hook, err error) {
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Warn("plugin: hook failed", "plugin", name, "hook", string(hook), "error", plugin.SanitizeSessionError(toolresult.Mask(err.Error())))
	}
	return &Plugins{log: log, start: cfg.Plugins, inproc: inproc, workDir: workDir, cachePath: cfg.PluginCache,
		seen: map[string]pub.ToolDef{}, base: map[string]firstRead{},
		opts: plugin.Options{WorkspaceDir: workDir, HTTPHeaders: cfg.PluginHTTPHeaders, Client: client{calls}, Log: log,
			ServeURL: serveURL, RunToken: runToken, OnError: onError}}
}

// Start reads the manifest of each configured plugin once: from the manifest
// cache when the plugin has not changed since its last probe, else with one
// bounded probe. With refresh, it always probes. The in-process plugins follow
// the configured ones; they have no probe and no cache entry. The warm process
// of a plugin starts on its first hook or tool call. taken reports a tool name
// that another tool has. After an error, the next call tries again. Start does
// nothing when a Set made the live set first.
func (p *Plugins) Start(ctx context.Context, taken func(string) bool, refresh bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.live.Load() != nil {
		return nil
	}
	return p.swap(ctx, p.start, taken, refresh)
}

// Set makes specs the config plugins of the live set. A plugin whose spec is the same as before keeps its
// instance, a new or changed plugin gets a new instance that starts on its next use, and a plugin that specs
// leaves out stops. Set validates the manifests and the names first, and changes nothing when anything fails.
// It returns the plugins of the new set.
func (p *Plugins) Set(ctx context.Context, specs []config.PluginSpec, taken func(string) bool) ([]protocol.Plugin, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("plugins are closed")
	}
	if err := p.swap(ctx, specs, taken, false); err != nil {
		return nil, err
	}
	return append([]protocol.Plugin{}, p.Info()...), nil
}

// swap makes specs and the in-process plugins the live set. The caller holds p.mu.
func (p *Plugins) swap(ctx context.Context, specs []config.PluginSpec, taken func(string) bool, refresh bool) error {
	cur := p.live.Load()
	before, have, own := p.current(cur)
	c := newCache()
	if p.cachePath != "" && slices.ContainsFunc(specs, func(ps config.PluginSpec) bool { return !reflect.DeepEqual(before[ps.Name], ps) }) {
		var err error
		if c, err = loadCache(p.cachePath, p.log); err != nil {
			return fmt.Errorf("plugin cache: %w", err)
		}
	}
	probe := func(spec plugin.Spec) (pub.Manifest, error) {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		return plugin.ProbeSpec(pctx, spec)
	}
	names := map[string]bool{}
	hostSpecs := make([]plugin.Spec, 0, len(specs)+len(p.inproc))
	keep := map[string]bool{}
	limits := map[string]time.Duration{}
	dirty := false
	for _, ps := range specs {
		if slices.ContainsFunc(p.inproc, func(ip *pub.Plugin) bool { return ip.Name == ps.Name }) {
			return fmt.Errorf("plugin %s: the name is the name of an in-process plugin", ps.Name)
		}
		spec, toolNames := plugin.Spec{Manifest: pub.Manifest{Name: ps.Name}}, have[ps.Name]
		if reflect.DeepEqual(before[ps.Name], ps) {
			keep[ps.Name] = true
		} else {
			m, changed, err := c.manifest(probe, ps, refresh)
			if err != nil {
				return fmt.Errorf("plugin %s: %w", ps.Name, err)
			}
			dirty = dirty || changed
			spec = plugin.Spec{Command: ps.Command, Env: ps.Env, Dir: ps.Dir, Config: ps.Config, Manifest: m}
			toolNames = nil
			for _, d := range m.Tools {
				toolNames = append(toolNames, d.Name)
			}
		}
		for _, t := range toolNames {
			if t == "" || names[t] || taken(t) && !own[t] {
				return fmt.Errorf("plugin %s: tool name %q is empty or taken", ps.Name, t)
			}
			names[t] = true
			limits[t] = ps.ToolTimeout()
		}
		hostSpecs = append(hostSpecs, spec)
	}
	for _, ip := range p.inproc {
		spec := plugin.Spec{Manifest: pub.Manifest{Name: ip.Name}}
		for _, d := range ip.Manifest().Tools {
			if d.Name == "" || names[d.Name] || cur == nil && taken(d.Name) {
				return fmt.Errorf("plugin %s: tool name %q is empty or taken", ip.Name, d.Name)
			}
			names[d.Name] = true
		}
		if cur == nil {
			spec = plugin.InProcess(ip, p.spawn)
		}
		keep[ip.Name] = true
		hostSpecs = append(hostSpecs, spec)
	}
	host, stop, err := p.replace(cur, hostSpecs, keep)
	if err != nil {
		return err
	}
	if dirty && p.cachePath != "" {
		if err := c.save(p.cachePath); err != nil {
			return fmt.Errorf("plugin cache: %w", err)
		}
	}
	p.remember(host.Tools())
	p.live.Store(&live{host: host, specs: specs, limits: limits})
	stop()
	return nil
}

// current describes the live set cur: the config spec and the tool names of each config plugin, and the set of
// those tool names. A nil cur is empty.
func (p *Plugins) current(cur *live) (before map[string]config.PluginSpec, have map[string][]string, own map[string]bool) {
	before, have, own = map[string]config.PluginSpec{}, map[string][]string{}, map[string]bool{}
	if cur == nil {
		return
	}
	for _, ps := range cur.specs {
		before[ps.Name] = ps
	}
	for _, i := range cur.host.Plugins() {
		if _, ok := before[i.Name]; ok {
			have[i.Name] = i.Tools
			for _, t := range i.Tools {
				own[t] = true
			}
		}
	}
	return
}

// replace builds the Host of hostSpecs from cur. It returns the func that stops what the new Host drops.
func (p *Plugins) replace(cur *live, hostSpecs []plugin.Spec, keep map[string]bool) (*plugin.Host, func(), error) {
	if cur == nil {
		h, err := plugin.NewHost(p.opts, hostSpecs...)
		return h, func() {}, err
	}
	return cur.host.Replace(hostSpecs, func(n string) bool { return keep[n] })
}

// remember keeps the first definition of each tool.
func (p *Plugins) remember(defs []pub.ToolDef) {
	p.dmu.Lock()
	defer p.dmu.Unlock()
	for _, d := range defs {
		if _, ok := p.seen[d.Name]; !ok {
			p.seen[d.Name] = d
		}
	}
}

// Has reports whether a started plugin has a tool named name.
func (p *Plugins) Has(name string) bool {
	l := p.live.Load()
	return l != nil && slices.ContainsFunc(l.host.Tools(), func(d pub.ToolDef) bool { return d.Name == name })
}

// Names returns the tool names of the started plugins. A nil Plugins has none.
func (p *Plugins) Names() []string {
	if p == nil || p.live.Load() == nil {
		return nil
	}
	var out []string
	for _, d := range p.live.Load().host.Tools() {
		out = append(out, d.Name)
	}
	return out
}

// Session returns the plugins of session id. Call it after Start.
func (p *Plugins) Session(id string) *Session {
	return &Session{id: id, p: p}
}

// Info returns the state of each started plugin, or nil before Start.
func (p *Plugins) Info() []protocol.Plugin {
	l := p.live.Load()
	if l == nil {
		return nil
	}
	var out []protocol.Plugin
	for _, i := range l.host.Plugins() {
		out = append(out, protocol.Plugin{Name: i.Name, State: i.State, Tools: i.Tools, Hooks: i.Hooks})
	}
	return out
}

// Close stops every plugin process and waits for the in-process serve loops.
// A second Close does nothing.
func (p *Plugins) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.smu.Lock()
	p.stopped = true
	p.smu.Unlock()
	if l := p.live.Load(); l != nil && !p.closed {
		l.host.Close()
	}
	p.closed = true
	p.serving.Wait()
}

// Session is a turn.Source and turn.Hooks for one session. Every dispatch reads the live set; a tool call
// keeps the Host that its before hook ran on for its after hook and its events.
type Session struct {
	id    string
	p     *Plugins
	calls sync.Map // call ID to *plugin.Host
}

// firstRead is what a session found at its first tool read: the names of the plugin tools that it sends, and
// whether the live set had a config plugin.
type firstRead struct {
	names   []string
	present bool
}

// Present reports whether the live set had a config plugin at the first tool read of the session. The first call
// decides it, and the runtime keeps the answer for the life of the process.
func (s *Session) Present() bool { return s.first().present }

func (s *Session) first() firstRead {
	l := s.p.live.Load()
	s.p.dmu.Lock()
	defer s.p.dmu.Unlock()
	f, ok := s.p.base[s.id]
	if !ok {
		f.present = len(l.specs) > 0
		for _, d := range l.host.Tools() {
			f.names = append(f.names, d.Name)
		}
		s.p.base[s.id] = f
	}
	return f
}

// toolOf returns the tool name with its first definition.
func (s *Session) toolOf(name string) turn.Tool {
	s.p.dmu.Lock()
	defer s.p.dmu.Unlock()
	return tool{s, s.p.seen[name]}
}

// Toolset gives the plugin tools that the session sent at its first tool read and allowed keeps, and the
// system.transform segments of this model call as its tail.
func (s *Session) Toolset(ctx context.Context, _ []protocol.LogMessage, allowed []string, model string) turn.Toolset {
	h := s.p.live.Load().host
	ref, _ := modelref.ParseModelRef(model)
	segs := h.SystemTransform(ctx, &pub.SystemTransformRequest{SessionID: s.id, Model: ref.String()})
	var tools []turn.Tool
	for _, n := range s.first().names {
		tools = append(tools, s.toolOf(n))
	}
	return turn.Toolset{Tools: turn.Restrict(tools, allowed), Tail: strings.Join(segs, "\n\n"), Hooks: s}
}

// Late returns the plugin tools that the first tool read of the session did not reach, in name order: the tools
// of the live set, and the tools that history loaded and the live set lost.
func (s *Session) Late(history []protocol.LogMessage, allowed []string) []mcpsrc.Late {
	h := s.p.live.Load().host
	base := s.first().names
	loaded := mcpsrc.Selected(history)
	owner := map[string]string{}
	for _, i := range h.Plugins() {
		for _, t := range i.Tools {
			owner[t] = i.Name
		}
	}
	s.p.dmu.Lock()
	names := slices.Collect(maps.Keys(owner))
	for n := range loaded {
		if _, ok := s.p.seen[n]; ok {
			names = append(names, n)
		}
	}
	s.p.dmu.Unlock()
	slices.Sort(names)
	var out []mcpsrc.Late
	for _, n := range slices.Compact(names) {
		if slices.Contains(base, n) || allowed != nil && !slices.Contains(allowed, n) {
			continue
		}
		_, isLive := owner[n]
		out = append(out, mcpsrc.Late{Plugin: owner[n], Tool: s.toolOf(n), Loaded: loaded[n], Live: isLive})
	}
	return out
}

// ChatParams runs the chat.params chain for one model call. An empty model in
// the result keeps p.Model; a nil sampling value keeps the provider default.
func (s *Session) ChatParams(ctx context.Context, p turn.CallParams) turn.CallParams {
	in := pub.ChatParams{Temperature: p.Temperature, TopP: p.TopP}
	if p.MaxTokens > 0 {
		in.MaxTokens = &p.MaxTokens
	}
	ref, _ := modelref.ParseModelRef(p.Model)
	in.Model = ref.String()
	out := s.p.live.Load().host.ChatParams(ctx, &pub.ChatParamsRequest{SessionID: s.id, Params: in})
	if out.Model != "" {
		p.Model = out.Model
	}
	if out.MaxTokens != nil {
		p.MaxTokens = *out.MaxTokens
	}
	p.Temperature, p.TopP = out.Temperature, out.TopP
	return p
}

// ShellEnv returns the variables that the shell.env hooks add to a bash command.
func (s *Session) ShellEnv(ctx context.Context, command string) map[string]string {
	return s.p.live.Load().host.ShellEnv(ctx, &pub.ShellEnvRequest{SessionID: s.id, Tool: "bash", Command: command, Dir: s.p.workDir})
}

// Untransformed returns s as a Source that runs no system.transform, for a
// backend that reads no system prompt of the harness.
func (s *Session) Untransformed() turn.Source { return untransformed{s} }

type untransformed struct{ s *Session }

func (u untransformed) Toolset(_ context.Context, _ []protocol.LogMessage, allowed []string, _ string) turn.Toolset {
	var tools []turn.Tool
	for _, d := range u.s.p.live.Load().host.Tools() {
		tools = append(tools, tool{u.s, d})
	}
	return turn.Toolset{Tools: turn.Restrict(tools, allowed), Hooks: u.s}
}

// Before runs the tool.execute.before chain and announces a call that the
// chain did not deny. After announces its end, also when ctx ended.
func (s *Session) Before(ctx context.Context, c protocol.ToolCall) (protocol.ToolCall, string) {
	h := s.p.live.Load().host
	args, deny := h.ToolExecuteBefore(ctx, &pub.ToolExecuteBeforeRequest{SessionID: s.id, CallID: c.ID, Tool: c.Name, Args: c.Arguments})
	if deny == "" {
		c.Arguments = args
		s.calls.Store(c.ID, h)
		emit(h, s.id, pub.EventToolExecuteStart, pub.ToolExecuteStartProperties{Tool: c.Name, CallID: c.ID})
	}
	return c, deny
}

// After announces the end of a call and runs the tool.execute.after chain.
// A successful write_file or edit_file call also announces file.edited.
func (s *Session) After(ctx context.Context, c protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
	h := s.p.live.Load().host
	if v, ok := s.calls.LoadAndDelete(c.ID); ok {
		h = v.(*plugin.Host)
	}
	emit(h, s.id, pub.EventToolExecuteEnd, pub.ToolExecuteEndProperties{Tool: c.Name, CallID: c.ID, OK: !r.IsError})
	var edit struct{ Path string }
	if (c.Name == "write_file" || c.Name == "edit_file") && !r.IsError && json.Unmarshal(c.Arguments, &edit) == nil && edit.Path != "" {
		if !filepath.IsAbs(edit.Path) {
			edit.Path = filepath.Join(s.p.workDir, edit.Path)
		}
		if abs, err := filepath.Abs(edit.Path); err == nil {
			emit(h, s.id, pub.EventFileEdited, pub.FileEditedProperties{Path: abs})
		}
	}
	if ctx.Err() != nil {
		return r
	}
	out := h.ToolExecuteAfter(ctx, &pub.ToolExecuteAfterRequest{SessionID: s.id, CallID: c.ID, Tool: c.Name, Args: c.Arguments,
		Output: []pub.Part{pub.TextPart(r.Text)}})
	return protocol.ToolResult{Text: partsText(out), IsError: r.IsError, Blobs: r.Blobs}
}

// Appended announces the start and the end of each turn in events.
func (s *Session) Appended(events []protocol.Payload) {
	h := s.p.live.Load().host
	for _, e := range events {
		switch e := e.(type) {
		case protocol.TurnStarted, protocol.TurnResumed:
			emit(h, s.id, pub.EventSessionStatus, map[string]string{"status": "busy"})
		case protocol.TurnSuspended:
			emit(h, s.id, pub.EventSessionStatus, map[string]string{"status": "idle"})
		case protocol.TurnEnded:
			if e.StopReason == protocol.StopFailed {
				emit(h, s.id, pub.EventSessionError, pub.SessionErrorProperties{Message: plugin.SanitizeSessionError(e.Error)})
			}
			emit(h, s.id, pub.EventSessionStatus, map[string]string{"status": "idle"})
		}
	}
}

func emit(h *plugin.Host, id, typ string, props any) {
	b, _ := json.Marshal(props)
	h.Emit([]pub.Event{{Type: typ, SessionID: id, Properties: b}})
}

type tool struct {
	s   *Session
	def pub.ToolDef
}

func (t tool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: t.def.Name, Description: t.def.Description, InputSchema: t.def.InputSchema}
}

func (t tool) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	l := t.s.p.live.Load()
	run, err := l.host.StartTool(ctx, c.Name)
	if errors.Is(err, plugin.ErrNoTool) {
		return protocol.ToolResult{Text: "no such tool available: " + c.Name, IsError: true}, nil
	}
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return toolresult.Limit(ctx, c.Name, cmp.Or(l.limits[c.Name], config.DefaultToolTimeout), func(ctx context.Context) (protocol.ToolResult, error) {
		resp, err := run(ctx, &pub.ToolExecuteRequest{SessionID: t.s.id, CallID: c.ID, Tool: c.Name, Args: c.Arguments})
		if err != nil {
			return protocol.ToolResult{}, err
		}
		text, blobs, err := toolOutput(resp.Output)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		return protocol.ToolResult{Text: text, IsError: resp.IsError, Blobs: blobs}, nil
	})
}

// toolOutput maps tool output to result text and blobs. A blob part with
// inline data becomes a blob, and one with only a URL becomes a text line. A
// part of another type is not result content and fails the call.
func toolOutput(parts []pub.Part) (string, []protocol.Blob, error) {
	var lines []string
	var blobs []protocol.Blob
	for _, p := range parts {
		switch {
		case p.Type == pub.PartText:
			lines = append(lines, p.Text)
		case p.Type == pub.PartBlob && len(p.Data) > 0:
			blobs = append(blobs, protocol.Blob{MediaType: p.MediaType, Data: p.Data})
		case p.Type == pub.PartBlob:
			lines = append(lines, fmt.Sprintf("blob: %s (%s)", p.URL, p.MediaType))
		default:
			return "", nil, fmt.Errorf("plugin tool output has a %q part, which is not tool result content", p.Type)
		}
	}
	return strings.Join(lines, "\n"), blobs, nil
}

// client serves the plugin calls of the harness client API.
type client struct {
	calls Calls
}

func (c client) SessionMessages(ctx context.Context, req *pub.SessionMessagesRequest) (*pub.SessionMessagesResponse, error) {
	h, err := c.calls.History(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	msgs := make([]message.Message, 0, len(h))
	for _, m := range h {
		out := message.Message{Role: message.Role(m.Role)}
		for _, p := range m.Parts {
			switch p.Type {
			case protocol.LogPartText:
				out.Parts = append(out.Parts, &message.Text{Text: p.Text})
			case protocol.LogPartReasoning:
				out.Parts = append(out.Parts, &message.Reasoning{Text: p.Text})
			case protocol.LogPartToolCall:
				out.Parts = append(out.Parts, &message.ToolCall{CallID: p.CallID, Name: p.Name, Arguments: p.Arguments})
			case protocol.LogPartToolResult:
				out.Parts = append(out.Parts, &message.ToolResult{CallID: p.CallID, IsError: p.IsError, Content: message.Parts{&message.Text{Text: p.Text}}})
			case protocol.LogPartBlob:
				data, err := c.readBlob(ctx, req.SessionID, p.BlobKey)
				if err != nil {
					return nil, err
				}
				out.Parts = append(out.Parts, &message.Blob{MediaType: p.MediaType, Data: data})
			}
		}
		msgs = append(msgs, out)
	}
	var out []pub.Message
	if err := convert(msgs, &out); err != nil {
		return nil, err
	}
	return &pub.SessionMessagesResponse{Messages: out}, nil
}

func partsText(parts []pub.Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == pub.PartText {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func (c client) readBlob(ctx context.Context, sessionID, key string) ([]byte, error) {
	rc, err := c.calls.Blob(ctx, sessionID, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}
