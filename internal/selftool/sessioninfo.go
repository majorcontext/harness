package selftool

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"github.com/majorcontext/harness/internal/process"
	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/internal/tool/proc"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// SessionInfoName is the name of the session_info tool.
const SessionInfoName = "session_info"

const sessionInfoDescription = "Report this session's own configuration: session id, current model, " +
	"the reasoning-effort/thinking level (\"off\", \"minimal\", \"low\", \"medium\", \"high\", \"xhigh\", \"max\", " +
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

// Prompt is the system prompt of one session and a record of what its model
// calls sent. The session reads the prompt files once, when it starts.
type Prompt struct {
	procs    *process.Manager
	workDir  string
	read     prompt.Info
	segments []string

	mu   sync.Mutex
	turn []string
	call modelCall
}

// modelCall is what the newest model call took from the Source: the names of
// its tools and the prompt that follows the system prompt.
type modelCall struct {
	tools  []string
	prompt string
}

// NewPrompt returns the prompt of a session that read describes. agent and p
// name its agent profile. procs is nil when the runtime has no WorkDir.
func NewPrompt(read prompt.Info, agent string, p prompt.Profile, procs *process.Manager, workDir string) *Prompt {
	segs := slices.Clone(read.Segments)
	if agent != "" {
		if t := strings.Trim(p.Prompt, "\n"); t != "" {
			segs = append(segs, t)
		}
	}
	return &Prompt{procs: procs, workDir: workDir, read: read, segments: segs}
}

// System returns the system prompt of a turn: the prompt of the session, the
// prompt of its agent profile, and the process status line. It records the
// segments as the segments of the turn.
func (p *Prompt) System() string {
	segs := slices.Clone(p.segments)
	if p.procs != nil {
		if line := proc.StatusLine(p.procs, p.workDir); line != "" {
			segs = append(segs, prompt.RenderEngineContext(line))
		}
	}
	p.mu.Lock()
	p.turn = segs
	p.mu.Unlock()
	return strings.Join(segs, "\n\n")
}

// Recording returns src and records each Toolset that it gives.
func (p *Prompt) Recording(src turn.Source) turn.Source { return recorded{p, src} }

type recorded struct {
	p   *Prompt
	src turn.Source
}

func (c recorded) Toolset(ctx context.Context, history []protocol.LogMessage, allowed []string, model string) turn.Toolset {
	ts := c.src.Toolset(ctx, history, allowed, model)
	call := modelCall{prompt: ts.Prompt}
	for _, t := range ts.Tools {
		call.tools = append(call.tools, t.Spec().Name)
	}
	c.p.mu.Lock()
	c.p.call = call
	c.p.mu.Unlock()
	return ts
}

// sent returns the system segments and the sorted tool names of the newest
// model call.
func (p *Prompt) sent() (system, tools []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	system = slices.Clone(p.turn)
	if p.call.prompt != "" {
		system = append(system, p.call.prompt)
	}
	tools = slices.Clone(p.call.tools)
	slices.Sort(tools)
	return system, tools
}

// SessionInfo is the session_info tool. It reports one session to its model.
// The runtime builds it with the session.
type SessionInfo struct {
	host    Sessions
	session string
	prompt  *Prompt
}

// NewSessionInfo returns the session_info tool of session id, which reads
// what p recorded.
func NewSessionInfo(host Sessions, id string, p *Prompt) *SessionInfo {
	return &SessionInfo{host: host, session: id, prompt: p}
}

// Spec returns the specification of the session_info tool.
func (*SessionInfo) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: SessionInfoName, Description: sessionInfoDescription, InputSchema: json.RawMessage(sessionInfoSchema)}
}

// Run reports the session.
func (t *SessionInfo) Run(_ context.Context, _ protocol.ToolCall) (protocol.ToolResult, error) {
	s, ok := t.host.Session(t.session)
	if !ok {
		return protocol.ToolResult{}, session.ErrNotOwned
	}
	v := s.Snapshot()
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
