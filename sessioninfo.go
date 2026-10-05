package harness

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/tool/proc"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/protocol"
)

const sessionInfoName = "session_info"

const sessionInfoDescription = "Report this session's own configuration: session id, current model, " +
	"the reasoning-effort/thinking level (\"off\", \"minimal\", \"low\", \"medium\", \"high\", " +
	"or \"\" if unset — the provider default), cumulative token usage, the exact " +
	"system-prompt segments you received this turn, the active tool names, the " +
	"provenance of any injected project instructions (AGENTS.md path or \"none\"), " +
	"the discovered Agent Skills (names and SKILL.md paths), and the configured plugins " +
	"(name, spawn state, registered tools, subscribed hooks). Takes no arguments."

const sessionInfoSchema = `{"type":"object","properties":{},"additionalProperties":false}`

// sessionInfoResult is the JSON that the session_info tool returns.
type sessionInfoResult struct {
	SessionID    string            `json:"session_id"`
	Model        string            `json:"model"`
	Effort       string            `json:"effort"`
	ServiceTier  string            `json:"service_tier"`
	Usage        protocol.Usage    `json:"usage"`
	System       []string          `json:"system"`
	Tools        []string          `json:"tools"`
	Instructions string            `json:"instructions"`
	Skills       []prompt.Skill    `json:"skills"`
	Plugins      []protocol.Plugin `json:"plugins"`
}

// sessionPrompt is the system prompt of one session and a record of what its
// model calls sent. The session reads the prompt files once, when it starts.
type sessionPrompt struct {
	r        *Runtime
	read     prompt.Info
	segments []string

	mu   sync.Mutex
	turn []string
	call modelCall
}

// modelCall is what the newest model call took from the Source: the names of
// its tools, the allowed tools, and the prompt that follows the system prompt.
type modelCall struct {
	tools   []string
	allowed []string
	prompt  string
}

func (r *Runtime) newSessionPrompt(agent string, p prompt.Profile) *sessionPrompt {
	read := r.prompt()
	segs := slices.Clone(read.Segments)
	if agent != "" {
		if t := strings.Trim(p.Prompt, "\n"); t != "" {
			segs = append(segs, t)
		}
	}
	return &sessionPrompt{r: r, read: read, segments: segs}
}

// system returns the system prompt of a turn: the prompt of the session, the
// prompt of its agent profile, and the process status line. It records the
// segments as the segments of the turn.
func (p *sessionPrompt) system() string {
	segs := slices.Clone(p.segments)
	if p.r.procs != nil {
		if line := proc.StatusLine(p.r.procs, p.r.workDir); line != "" {
			segs = append(segs, message.RenderEngineContext(line))
		}
	}
	p.mu.Lock()
	p.turn = segs
	p.mu.Unlock()
	return strings.Join(segs, "\n\n")
}

// recording returns src and records each Toolset that it gives.
func (p *sessionPrompt) recording(src turn.Source) turn.Source { return recorded{p, src} }

type recorded struct {
	p   *sessionPrompt
	src turn.Source
}

func (c recorded) Toolset(ctx context.Context, history []eventlog.Message, allowed []string, model string) turn.Toolset {
	ts := c.src.Toolset(ctx, history, allowed, model)
	call := modelCall{allowed: slices.Clone(allowed), prompt: ts.Prompt}
	for _, t := range ts.Tools {
		call.tools = append(call.tools, t.Spec().Name)
	}
	c.p.mu.Lock()
	c.p.call = call
	c.p.mu.Unlock()
	return ts
}

// sent returns the system segments and the sorted tool names of the newest
// model call. read_tool_result comes from the actor, so it is not in the
// recorded names.
func (p *sessionPrompt) sent() (system, tools []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	system = slices.Clone(p.turn)
	if p.call.prompt != "" {
		system = append(system, p.call.prompt)
	}
	tools = slices.Clone(p.call.tools)
	if p.call.allowed == nil || slices.Contains(p.call.allowed, toolresult.ToolName) {
		tools = append(tools, toolresult.ToolName)
	}
	slices.Sort(tools)
	return system, tools
}

// sessionInfoTool reports one session to its model. The runtime builds it
// with the session.
type sessionInfoTool struct {
	r       *Runtime
	session string
	prompt  *sessionPrompt
}

func (*sessionInfoTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: sessionInfoName, Description: sessionInfoDescription, InputSchema: json.RawMessage(sessionInfoSchema)}
}

func (t *sessionInfoTool) Run(_ context.Context, _ protocol.ToolCall) (protocol.ToolResult, error) {
	s := t.r.running(t.session)
	if s == nil {
		return protocol.ToolResult{}, ErrSessionNotOwned
	}
	v := s.View()
	system, tools := t.prompt.sent()
	out := sessionInfoResult{SessionID: v.ID, Model: v.Model, Effort: v.Effort, ServiceTier: v.ServiceTier, Usage: v.Usage,
		System: system, Tools: tools, Instructions: "none",
		Skills: append([]prompt.Skill{}, t.prompt.read.Skills...), Plugins: append([]protocol.Plugin{}, v.Plugins...)}
	if len(t.prompt.read.Instructions) > 0 {
		out.Instructions = strings.Join(t.prompt.read.Instructions, ", ")
	}
	b, err := json.MarshalIndent(out, "", "  ")
	return protocol.ToolResult{Text: string(b)}, err
}
