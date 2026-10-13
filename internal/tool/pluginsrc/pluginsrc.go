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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/internal/plugin"
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

// Plugins starts the configured plugins once for each runtime.
type Plugins struct {
	log       *slog.Logger
	specs     []config.PluginSpec
	inproc    []*pub.Plugin
	opts      plugin.Options
	workDir   string
	cachePath string

	// host is set once, by the first successful Start.
	host   atomic.Pointer[plugin.Host]
	mu     sync.Mutex
	closed bool

	serving sync.WaitGroup
	smu     sync.Mutex
	stopped bool
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

// New returns the Plugins of cfg.Plugins and inproc, or nil when both are
// empty. It does no I/O.
func New(cfg config.Config, inproc []*pub.Plugin, workDir string, calls Calls, serveURL, runToken string, log *slog.Logger) *Plugins {
	if len(cfg.Plugins) == 0 && len(inproc) == 0 {
		return nil
	}
	log = cmp.Or(log, slog.Default())
	onError := func(name string, hook pub.Hook, err error) {
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Warn("plugin: hook failed", "plugin", name, "hook", string(hook), "error", plugin.SanitizeSessionError(toolresult.Mask(err.Error())))
	}
	return &Plugins{log: log, specs: cfg.Plugins, inproc: inproc, workDir: workDir, cachePath: cfg.PluginCache,
		opts: plugin.Options{WorkspaceDir: workDir, HTTPHeaders: cfg.PluginHTTPHeaders, Client: client{calls}, Log: log,
			ServeURL: serveURL, RunToken: runToken, OnError: onError}}
}

// Start reads the manifest of each configured plugin once: from the manifest
// cache when the plugin has not changed since its last probe, else with one
// bounded probe. With refresh, it always probes. The in-process plugins follow
// the configured ones; they have no probe and no cache entry. The warm process
// of a plugin starts on its first hook or tool call. taken reports a tool name
// that another tool has. After an error, the next call tries again.
func (p *Plugins) Start(ctx context.Context, taken func(string) bool, refresh bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host.Load() != nil {
		return nil
	}
	c := newCache()
	if p.cachePath != "" {
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
	specs := make([]plugin.Spec, len(p.specs), len(p.specs)+len(p.inproc))
	names := map[string]bool{}
	dirty := false
	for i, ps := range p.specs {
		m, changed, err := c.manifest(probe, ps, refresh)
		if err != nil {
			return fmt.Errorf("plugin %s: %w", ps.Name, err)
		}
		dirty = dirty || changed
		for _, d := range m.Tools {
			if d.Name == "" || names[d.Name] || taken(d.Name) {
				return fmt.Errorf("plugin %s: tool name %q is empty or taken", ps.Name, d.Name)
			}
			names[d.Name] = true
		}
		specs[i] = plugin.Spec{Command: ps.Command, Env: ps.Env, Dir: ps.Dir, Config: ps.Config, Manifest: m}
	}
	for _, ip := range p.inproc {
		spec := plugin.InProcess(ip, p.spawn)
		for _, d := range spec.Manifest.Tools {
			if d.Name == "" || names[d.Name] || taken(d.Name) {
				return fmt.Errorf("plugin %s: tool name %q is empty or taken", ip.Name, d.Name)
			}
			names[d.Name] = true
		}
		specs = append(specs, spec)
	}
	host, err := plugin.NewHost(p.opts, specs...)
	if err != nil {
		return err
	}
	if dirty && p.cachePath != "" {
		if err := c.save(p.cachePath); err != nil {
			return fmt.Errorf("plugin cache: %w", err)
		}
	}
	p.host.Store(host)
	return nil
}

// Has reports whether a started plugin has a tool named name.
func (p *Plugins) Has(name string) bool {
	h := p.host.Load()
	return h != nil && slices.ContainsFunc(h.Tools(), func(d pub.ToolDef) bool { return d.Name == name })
}

// Names returns the tool names of the started plugins. A nil Plugins has none.
func (p *Plugins) Names() []string {
	if p == nil || p.host.Load() == nil {
		return nil
	}
	h := p.host.Load()
	var out []string
	for _, d := range h.Tools() {
		out = append(out, d.Name)
	}
	return out
}

// Session returns the plugins of session id. Call it after Start.
func (p *Plugins) Session(id string) *Session {
	host := p.host.Load()
	s := &Session{id: id, host: host, workDir: p.workDir}
	for _, d := range host.Tools() {
		s.tools = append(s.tools, tool{s, d})
	}
	return s
}

// Info returns the state of each started plugin, or nil before Start.
func (p *Plugins) Info() []protocol.Plugin {
	h := p.host.Load()
	if h == nil {
		return nil
	}
	var out []protocol.Plugin
	for _, i := range h.Plugins() {
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
	if h := p.host.Load(); h != nil && !p.closed {
		h.Close()
	}
	p.closed = true
	p.serving.Wait()
}

// Session is a turn.Source and turn.Hooks for one session.
type Session struct {
	id      string
	host    *plugin.Host
	workDir string
	tools   []turn.Tool
}

// Toolset gives the plugin tools that allowed keeps, and the system.transform
// segments of this model call as its prompt.
func (s *Session) Toolset(ctx context.Context, _ []protocol.LogMessage, allowed []string, model string) turn.Toolset {
	ref, _ := modelref.ParseModelRef(model)
	segs := s.host.SystemTransform(ctx, &pub.SystemTransformRequest{SessionID: s.id, Model: ref.String()})
	return turn.Toolset{Tools: turn.Restrict(s.tools, allowed), Prompt: strings.Join(segs, "\n\n"), Hooks: s}
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
	out := s.host.ChatParams(ctx, &pub.ChatParamsRequest{SessionID: s.id, Params: in})
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
	return s.host.ShellEnv(ctx, &pub.ShellEnvRequest{SessionID: s.id, Tool: "bash", Command: command, Dir: s.workDir})
}

// Untransformed returns s as a Source that runs no system.transform, for a
// backend that reads no system prompt of the harness.
func (s *Session) Untransformed() turn.Source { return untransformed{s} }

type untransformed struct{ s *Session }

func (u untransformed) Toolset(_ context.Context, _ []protocol.LogMessage, allowed []string, _ string) turn.Toolset {
	return turn.Toolset{Tools: turn.Restrict(u.s.tools, allowed), Hooks: u.s}
}

// Before runs the tool.execute.before chain and announces a call that the
// chain did not deny. After announces its end, also when ctx ended.
func (s *Session) Before(ctx context.Context, c protocol.ToolCall) (protocol.ToolCall, string) {
	args, deny := s.host.ToolExecuteBefore(ctx, &pub.ToolExecuteBeforeRequest{SessionID: s.id, CallID: c.ID, Tool: c.Name, Args: c.Arguments})
	if deny == "" {
		c.Arguments = args
		s.emit(pub.EventToolExecuteStart, pub.ToolExecuteStartProperties{Tool: c.Name, CallID: c.ID})
	}
	return c, deny
}

// After announces the end of a call and runs the tool.execute.after chain.
// A successful write_file or edit_file call also announces file.edited.
func (s *Session) After(ctx context.Context, c protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
	s.emit(pub.EventToolExecuteEnd, pub.ToolExecuteEndProperties{Tool: c.Name, CallID: c.ID, OK: !r.IsError})
	var edit struct{ Path string }
	if (c.Name == "write_file" || c.Name == "edit_file") && !r.IsError && json.Unmarshal(c.Arguments, &edit) == nil && edit.Path != "" {
		if !filepath.IsAbs(edit.Path) {
			edit.Path = filepath.Join(s.workDir, edit.Path)
		}
		if abs, err := filepath.Abs(edit.Path); err == nil {
			s.emit(pub.EventFileEdited, pub.FileEditedProperties{Path: abs})
		}
	}
	if ctx.Err() != nil {
		return r
	}
	out := s.host.ToolExecuteAfter(ctx, &pub.ToolExecuteAfterRequest{SessionID: s.id, CallID: c.ID, Tool: c.Name, Args: c.Arguments,
		Output: []pub.Part{pub.TextPart(r.Text)}})
	return protocol.ToolResult{Text: partsText(out), IsError: r.IsError, Blobs: r.Blobs}
}

// Appended announces the start and the end of each turn in events.
func (s *Session) Appended(events []protocol.Payload) {
	for _, e := range events {
		switch e := e.(type) {
		case protocol.TurnStarted, protocol.TurnResumed:
			s.emit(pub.EventSessionStatus, map[string]string{"status": "busy"})
		case protocol.TurnSuspended:
			s.emit(pub.EventSessionStatus, map[string]string{"status": "idle"})
		case protocol.TurnEnded:
			if e.StopReason == protocol.StopFailed {
				s.emit(pub.EventSessionError, pub.SessionErrorProperties{Message: plugin.SanitizeSessionError(e.Error)})
			}
			s.emit(pub.EventSessionStatus, map[string]string{"status": "idle"})
		}
	}
}

func (s *Session) emit(typ string, props any) {
	b, _ := json.Marshal(props)
	s.host.Emit([]pub.Event{{Type: typ, SessionID: s.id, Properties: b}})
}

type tool struct {
	s   *Session
	def pub.ToolDef
}

func (t tool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: t.def.Name, Description: t.def.Description, InputSchema: t.def.InputSchema}
}

func (t tool) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	resp, err := t.s.host.ExecuteTool(ctx, &pub.ToolExecuteRequest{SessionID: t.s.id, CallID: c.ID, Tool: c.Name, Args: c.Arguments})
	if err != nil {
		return protocol.ToolResult{}, err
	}
	text, blobs, err := toolOutput(resp.Output)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return protocol.ToolResult{Text: text, IsError: resp.IsError, Blobs: blobs}, nil
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
