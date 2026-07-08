// Package engine is the headless core: the session loop that streams model
// turns, executes tool calls, and appends everything to the session's
// message history. Every frontend (CLI, TUI, server) is a client of this
// package; none of them are imported by it.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/andybons/harness/message"
	"github.com/andybons/harness/plugin"
	"github.com/andybons/harness/provider"
)

// Hooks is the slice of the plugin host the engine uses. *plugin.Host
// satisfies it; tests use fakes. A nil Hooks disables all hook dispatch.
type Hooks interface {
	ChatParams(ctx context.Context, req *plugin.ChatParamsRequest) plugin.ChatParams
	SystemTransform(ctx context.Context, req *plugin.SystemTransformRequest) []string
	ShellEnv(ctx context.Context, req *plugin.ShellEnvRequest) map[string]string
	ToolExecuteBefore(ctx context.Context, req *plugin.ToolExecuteBeforeRequest) (json.RawMessage, string)
	ToolExecuteAfter(ctx context.Context, req *plugin.ToolExecuteAfterRequest) message.Parts
	ExecuteTool(ctx context.Context, req *plugin.ToolExecuteRequest) (*plugin.ToolExecuteResponse, error)
	Emit(events []plugin.Event)
	Tools() []plugin.ToolDef
}

// Tool is a built-in (in-process) tool.
type Tool struct {
	Def provider.ToolDef
	Run func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error)
}

// Event is one entry in the session's event stream. Event types follow ACP
// naming where a choice is arbitrary (see AGENTS.md).
type Event struct {
	Type       string              `json:"type"`
	SessionID  string              `json:"session_id"`
	Text       string              `json:"text,omitempty"`
	Message    *message.Message    `json:"message,omitempty"`
	ToolCall   *message.ToolCall   `json:"tool_call,omitempty"`
	Output     message.Parts       `json:"output,omitempty"`
	IsError    bool                `json:"is_error,omitempty"`
	Usage      *provider.Usage     `json:"usage,omitempty"`
	StopReason provider.StopReason `json:"stop_reason,omitempty"`
}

// Event types.
const (
	EventTextDelta      = "text.delta"
	EventReasoningDelta = "reasoning.delta"
	EventMessage        = "message"
	EventToolStart      = "tool.start"
	EventToolEnd        = "tool.end"
)

// Config configures a Session.
type Config struct {
	Providers provider.Registry
	Model     message.ModelRef // initial model; swap any time with SetModel
	System    []string         // base system prompt segments
	MaxTokens int              // per-response cap; defaults to 8192
	WorkDir   string           // working directory for built-in tools

	Hooks   Hooks       // optional plugin host
	OnEvent func(Event) // optional; called synchronously, keep it fast

	// Tools are additional built-in tools. The bash tool is always
	// installed.
	Tools       []Tool
	BashTimeout time.Duration // defaults to 2m
}

// Session is one conversation: an in-memory history plus the agent loop.
// Methods are safe for one caller at a time; Prompt must not be called
// concurrently with itself.
type Session struct {
	ID string

	cfg   Config
	tools map[string]Tool

	mu      sync.Mutex
	model   message.ModelRef
	history []message.Message
	usage   provider.Usage
}

// NewSession creates a session. Nothing touches the network or spawns
// processes here — provider auth and plugin spawns happen on first use.
func NewSession(cfg Config) *Session {
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 8192
	}
	if cfg.BashTimeout <= 0 {
		cfg.BashTimeout = 2 * time.Minute
	}
	s := &Session{
		ID:    newID("ses"),
		cfg:   cfg,
		model: cfg.Model,
		tools: make(map[string]Tool),
	}
	bash := bashTool(cfg.BashTimeout)
	s.tools[bash.Def.Name] = bash
	for _, t := range cfg.Tools {
		s.tools[t.Def.Name] = t
	}
	return s
}

// SetModel swaps the model for subsequent requests. History transcodes
// automatically; there is no migration step.
func (s *Session) SetModel(ref message.ModelRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.model = ref
}

// Model returns the session's current model.
func (s *Session) Model() message.ModelRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}

// Usage returns cumulative token usage across all turns.
func (s *Session) Usage() provider.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *Session) addUsage(u provider.Usage) {
	s.mu.Lock()
	s.usage.InputTokens += u.InputTokens
	s.usage.OutputTokens += u.OutputTokens
	s.usage.CacheReadTokens += u.CacheReadTokens
	s.usage.CacheWriteTokens += u.CacheWriteTokens
	s.mu.Unlock()
}

// History returns a copy of the session's message history.
func (s *Session) History() []message.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]message.Message(nil), s.history...)
}

func (s *Session) append(m message.Message) {
	s.mu.Lock()
	s.history = append(s.history, m)
	s.mu.Unlock()
}

func (s *Session) emit(ev Event) {
	ev.SessionID = s.ID
	if s.cfg.OnEvent != nil {
		s.cfg.OnEvent(ev)
	}
}

func (s *Session) emitStatus(status string) {
	if s.cfg.Hooks == nil {
		return
	}
	props, _ := json.Marshal(map[string]string{"status": status})
	s.cfg.Hooks.Emit([]plugin.Event{{
		Type:       plugin.EventSessionStatus,
		SessionID:  s.ID,
		Properties: props,
	}})
}

// Prompt appends a user message and runs the agent loop — stream a turn,
// execute any tool calls, feed results back — until the model ends its turn.
// It returns the final assistant message.
func (s *Session) Prompt(ctx context.Context, text string) (*message.Message, error) {
	s.append(message.Message{
		ID:        newID("msg"),
		Role:      message.RoleUser,
		Parts:     message.Parts{&message.Text{Text: text}},
		CreatedAt: time.Now().UTC(),
	})
	s.emitStatus("busy")
	defer s.emitStatus("idle")

	for {
		asst, stop, usage, err := s.streamTurn(ctx)
		if err != nil {
			return nil, err
		}
		s.addUsage(usage)
		s.append(*asst)
		s.emit(Event{Type: EventMessage, Message: asst, StopReason: stop, Usage: &usage})

		if stop != provider.StopToolUse {
			return asst, nil
		}
		results := s.runToolCalls(ctx, asst)
		if len(results) == 0 {
			// tool_use stop with no tool calls: treat as end of turn
			// rather than looping forever.
			return asst, nil
		}
		s.append(message.Message{
			ID:        newID("msg"),
			Role:      message.RoleTool,
			Parts:     results,
			CreatedAt: time.Now().UTC(),
		})
	}
}

// streamTurn makes one model call and returns the assembled assistant
// message.
func (s *Session) streamTurn(ctx context.Context) (*message.Message, provider.StopReason, provider.Usage, error) {
	params := plugin.ChatParams{Model: s.Model()}
	system := append([]string(nil), s.cfg.System...)
	if s.cfg.Hooks != nil {
		params = s.cfg.Hooks.ChatParams(ctx, &plugin.ChatParamsRequest{SessionID: s.ID, Params: params})
		if params.Model.IsZero() {
			params.Model = s.Model()
		}
		system = append(system, s.cfg.Hooks.SystemTransform(ctx, &plugin.SystemTransformRequest{
			SessionID: s.ID,
			Model:     params.Model,
		})...)
	}

	prov, err := s.cfg.Providers.For(params.Model)
	if err != nil {
		return nil, "", provider.Usage{}, err
	}

	maxTokens := s.cfg.MaxTokens
	if params.MaxTokens != nil {
		maxTokens = *params.MaxTokens
	}
	req := &provider.Request{
		Model:       params.Model,
		System:      system,
		Messages:    s.History(),
		Tools:       s.toolDefs(),
		Temperature: params.Temperature,
		TopP:        params.TopP,
		MaxTokens:   maxTokens,
	}

	stream, err := prov.Stream(ctx, req)
	if err != nil {
		return nil, "", provider.Usage{}, err
	}
	defer stream.Close()

	for {
		ev, err := stream.Next()
		if err != nil {
			return nil, "", provider.Usage{}, err
		}
		switch ev.Type {
		case provider.EventTextDelta:
			s.emit(Event{Type: EventTextDelta, Text: ev.Text})
		case provider.EventReasoningDelta:
			s.emit(Event{Type: EventReasoningDelta, Text: ev.Text})
		case provider.EventDone:
			return ev.Message, ev.StopReason, ev.Usage, nil
		}
	}
}

// toolDefs merges built-in tools with plugin-provided ones.
func (s *Session) toolDefs() []provider.ToolDef {
	var defs []provider.ToolDef
	for _, t := range s.tools {
		defs = append(defs, t.Def)
	}
	if s.cfg.Hooks != nil {
		for _, d := range s.cfg.Hooks.Tools() {
			defs = append(defs, provider.ToolDef{
				Name:        d.Name,
				Description: d.Description,
				InputSchema: d.InputSchema,
			})
		}
	}
	return defs
}

// runToolCalls executes every tool call in an assistant message, in order,
// and returns the ToolResult parts.
func (s *Session) runToolCalls(ctx context.Context, asst *message.Message) message.Parts {
	var results message.Parts
	for _, p := range asst.Parts {
		tc, ok := p.(*message.ToolCall)
		if !ok {
			continue
		}
		out, isErr := s.runToolCall(ctx, tc)
		results = append(results, &message.ToolResult{
			CallID:  tc.CallID,
			Content: out,
			IsError: isErr,
		})
	}
	return results
}

func (s *Session) runToolCall(ctx context.Context, tc *message.ToolCall) (message.Parts, bool) {
	s.emit(Event{Type: EventToolStart, ToolCall: tc})

	args := tc.Arguments
	if s.cfg.Hooks != nil {
		newArgs, deny := s.cfg.Hooks.ToolExecuteBefore(ctx, &plugin.ToolExecuteBeforeRequest{
			SessionID: s.ID, CallID: tc.CallID, Tool: tc.Name, Args: args,
		})
		if deny != "" {
			out := message.Parts{&message.Text{Text: deny}}
			s.emit(Event{Type: EventToolEnd, ToolCall: tc, Output: out, IsError: true})
			return out, true
		}
		if newArgs != nil {
			args = newArgs
		}
	}

	out, isErr := s.executeTool(ctx, tc, args)

	if s.cfg.Hooks != nil {
		out = s.cfg.Hooks.ToolExecuteAfter(ctx, &plugin.ToolExecuteAfterRequest{
			SessionID: s.ID, CallID: tc.CallID, Tool: tc.Name, Args: args, Output: out,
		})
	}
	s.emit(Event{Type: EventToolEnd, ToolCall: tc, Output: out, IsError: isErr})
	return out, isErr
}

func (s *Session) executeTool(ctx context.Context, tc *message.ToolCall, args json.RawMessage) (message.Parts, bool) {
	if t, ok := s.tools[tc.Name]; ok {
		out, err := t.Run(ctx, s, args)
		if err != nil {
			return message.Parts{&message.Text{Text: err.Error()}}, true
		}
		return out, false
	}
	if s.cfg.Hooks != nil {
		resp, err := s.cfg.Hooks.ExecuteTool(ctx, &plugin.ToolExecuteRequest{
			SessionID: s.ID, CallID: tc.CallID, Tool: tc.Name, Args: args,
		})
		if err != nil {
			return message.Parts{&message.Text{Text: err.Error()}}, true
		}
		return resp.Output, resp.IsError
	}
	return message.Parts{&message.Text{Text: fmt.Sprintf("unknown tool %q", tc.Name)}}, true
}

// shellEnv collects env additions from the shell.env hook chain.
func (s *Session) shellEnv(ctx context.Context, tool, command string) map[string]string {
	if s.cfg.Hooks == nil {
		return nil
	}
	return s.cfg.Hooks.ShellEnv(ctx, &plugin.ShellEnvRequest{
		SessionID: s.ID, Tool: tool, Command: command, Dir: s.cfg.WorkDir,
	})
}

func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
