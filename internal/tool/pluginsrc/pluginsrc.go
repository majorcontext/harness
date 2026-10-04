// Package pluginsrc gives the tools and tool hooks of the configured
// plugins to each session, and sends them the events of each session.
package pluginsrc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/protocol"
)

const probeTimeout = 30 * time.Second

// History returns the conversation of a session.
type History func(ctx context.Context, sessionID string) ([]eventlog.Message, error)

// Plugins starts the configured plugins once for each runtime.
type Plugins struct {
	specs   []config.PluginSpec
	opts    plugin.Options
	workDir string

	mu     sync.Mutex
	host   *plugin.Host
	closed bool
}

// New returns the Plugins of cfg.Plugins, or nil when it is empty. It does no I/O.
func New(cfg config.Config, workDir string, history History) *Plugins {
	if len(cfg.Plugins) == 0 {
		return nil
	}
	return &Plugins{specs: cfg.Plugins, workDir: workDir,
		opts: plugin.Options{WorkspaceDir: workDir, HTTPHeaders: cfg.PluginHTTPHeaders, Client: client{history}}}
}

// Start reads the manifest of each plugin with one bounded probe, once. The
// warm process of a plugin starts on its first hook or tool call. taken
// reports a tool name that another tool has. After an error, the next call
// tries again.
func (p *Plugins) Start(ctx context.Context, taken func(string) bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host != nil {
		return nil
	}
	specs := make([]plugin.Spec, len(p.specs))
	names := map[string]bool{}
	for i, ps := range p.specs {
		specs[i] = plugin.Spec{Command: ps.Command, Env: ps.Env, Dir: ps.Dir, Config: ps.Config}
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		m, err := plugin.ProbeSpec(pctx, specs[i])
		cancel()
		if err != nil {
			return fmt.Errorf("plugin %s: %w", ps.Name, err)
		}
		if m.Name != ps.Name {
			return fmt.Errorf("plugin %s: manifest name %q does not match the config", ps.Name, m.Name)
		}
		for _, d := range m.Tools {
			if d.Name == "" || names[d.Name] || taken(d.Name) {
				return fmt.Errorf("plugin %s: tool name %q is empty or taken", ps.Name, d.Name)
			}
			names[d.Name] = true
		}
		specs[i].Manifest = m
	}
	host, err := plugin.NewHost(p.opts, specs...)
	if err != nil {
		return err
	}
	p.host = host
	return nil
}

// Tools returns the tools of the started plugins, for their names. Call it
// after Start.
func (p *Plugins) Tools() []turn.Tool { return p.Session("").tools }

// Session returns the plugins of session id. Call it after Start.
func (p *Plugins) Session(id string) *Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := &Session{id: id, host: p.host, workDir: p.workDir}
	for _, d := range p.host.Tools() {
		s.tools = append(s.tools, tool{s, d})
	}
	return s
}

// Info returns the state of each started plugin, or nil before Start.
func (p *Plugins) Info() []protocol.Plugin {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host == nil {
		return nil
	}
	var out []protocol.Plugin
	for _, i := range p.host.Plugins() {
		out = append(out, protocol.Plugin{Name: i.Name, State: i.State, Tools: i.Tools, Hooks: i.Hooks})
	}
	return out
}

// Close stops every plugin process. A second Close does nothing.
func (p *Plugins) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host != nil && !p.closed {
		p.host.Close()
	}
	p.closed = true
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
func (s *Session) Toolset(ctx context.Context, _ []eventlog.Message, allowed []string, model string) turn.Toolset {
	ref, _ := message.ParseModelRef(model)
	segs := s.host.SystemTransform(ctx, &plugin.SystemTransformRequest{SessionID: s.id, Model: ref})
	return turn.Toolset{Tools: turn.Restrict(s.tools, allowed), Prompt: strings.Join(segs, "\n\n"), Hooks: s}
}

// Before runs the tool.execute.before chain and announces a call that runs.
func (s *Session) Before(ctx context.Context, c protocol.ToolCall) (protocol.ToolCall, string) {
	args, deny := s.host.ToolExecuteBefore(ctx, &plugin.ToolExecuteBeforeRequest{SessionID: s.id, CallID: c.ID, Tool: c.Name, Args: c.Arguments})
	if deny == "" {
		c.Arguments = args
		s.emit(plugin.EventToolExecuteStart, plugin.ToolExecuteStartProperties{Tool: c.Name, CallID: c.ID})
	}
	return c, deny
}

// After announces the end of a call and runs the tool.execute.after chain.
// A successful write_file or edit_file call also announces file.edited.
func (s *Session) After(ctx context.Context, c protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
	s.emit(plugin.EventToolExecuteEnd, plugin.ToolExecuteEndProperties{Tool: c.Name, CallID: c.ID, OK: !r.IsError})
	var edit struct{ Path string }
	if (c.Name == "write_file" || c.Name == "edit_file") && !r.IsError && json.Unmarshal(c.Arguments, &edit) == nil && edit.Path != "" {
		if !filepath.IsAbs(edit.Path) {
			edit.Path = filepath.Join(s.workDir, edit.Path)
		}
		if abs, err := filepath.Abs(edit.Path); err == nil {
			s.emit(plugin.EventFileEdited, plugin.FileEditedProperties{Path: abs})
		}
	}
	out := s.host.ToolExecuteAfter(ctx, &plugin.ToolExecuteAfterRequest{SessionID: s.id, CallID: c.ID, Tool: c.Name, Args: c.Arguments,
		Output: message.Parts{&message.Text{Text: r.Text}}})
	return protocol.ToolResult{Text: out.Text(), IsError: r.IsError}
}

// Appended announces the start and the end of each turn in events.
func (s *Session) Appended(events []eventlog.Event) {
	for _, e := range events {
		switch e := e.(type) {
		case eventlog.TurnStarted, eventlog.TurnResumed:
			s.emit(plugin.EventSessionStatus, map[string]string{"status": "busy"})
		case eventlog.TurnSuspended:
			s.emit(plugin.EventSessionStatus, map[string]string{"status": "idle"})
		case eventlog.TurnEnded:
			if e.StopReason == eventlog.StopFailed {
				s.emit(plugin.EventSessionError, plugin.SessionErrorProperties{Message: plugin.SanitizeSessionError(e.Error)})
			}
			s.emit(plugin.EventSessionStatus, map[string]string{"status": "idle"})
		}
	}
}

func (s *Session) emit(typ string, props any) {
	b, _ := json.Marshal(props)
	s.host.Emit([]plugin.Event{{Type: typ, SessionID: s.id, Properties: b}})
}

type tool struct {
	s   *Session
	def plugin.ToolDef
}

func (t tool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: t.def.Name, Description: t.def.Description, InputSchema: t.def.InputSchema}
}

func (t tool) Run(ctx context.Context, c protocol.ToolCall) (protocol.ToolResult, error) {
	resp, err := t.s.host.ExecuteTool(ctx, &plugin.ToolExecuteRequest{SessionID: t.s.id, CallID: c.ID, Tool: c.Name, Args: c.Arguments})
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return protocol.ToolResult{Text: resp.Output.Text(), IsError: resp.IsError}, nil
}

// client serves the plugin calls of the harness client API.
type client struct{ history History }

func (c client) SessionMessages(ctx context.Context, req *plugin.SessionMessagesRequest) (*plugin.SessionMessagesResponse, error) {
	h, err := c.history(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	msgs := make([]message.Message, 0, len(h))
	for _, m := range h {
		out := message.Message{Role: message.Role(m.Role)}
		for _, p := range m.Parts {
			switch p.Type {
			case eventlog.PartText:
				out.Parts = append(out.Parts, &message.Text{Text: p.Text})
			case eventlog.PartReasoning:
				out.Parts = append(out.Parts, &message.Reasoning{Text: p.Text})
			case eventlog.PartToolCall:
				out.Parts = append(out.Parts, &message.ToolCall{CallID: p.CallID, Name: p.Name, Arguments: p.Arguments})
			case eventlog.PartToolResult:
				out.Parts = append(out.Parts, &message.ToolResult{CallID: p.CallID, IsError: p.IsError, Content: message.Parts{&message.Text{Text: p.Text}}})
			}
		}
		msgs = append(msgs, out)
	}
	return &plugin.SessionMessagesResponse{Messages: msgs}, nil
}

func (client) MCPCall(context.Context, *plugin.MCPCallRequest) (*plugin.MCPCallResult, error) {
	return nil, errors.New("pluginsrc: client/mcp.call is not served")
}

func (client) Generate(context.Context, *plugin.GenerateRequest) (*plugin.GenerateResponse, error) {
	return nil, plugin.ErrGenerateNotImplemented
}
