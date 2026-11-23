package selftool

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/session"
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

// Prompt records what the newest model call of one session sent: its system prompt and its tools.
type Prompt struct {
	skills *Skills

	mu       sync.Mutex
	info     prompt.Info
	turn     []string
	reported bool
	call     modelCall
}

// modelCall is what the newest model call took from the Source: the names of
// its tools and the prompt that follows the system prompt.
type modelCall struct {
	tools  []string
	prompt string
}

// NewPrompt returns the record of a session.
func NewPrompt() *Prompt { return &Prompt{} }

// Track makes session_info report the embedded skills of sk, in place of the embedded skills that the system prompt
// lists, and makes SkillsNotice compare them.
func (p *Prompt) Track(sk *Skills) { p.skills = sk }

// SkillsNotice returns the notice of the embedded skills that follows the history of at messages, compared with the
// declared list. It returns false for a session with no embedded skills.
func (p *Prompt) SkillsNotice(declared []prompt.Embedded, at int) (turn.Notice, bool) {
	if p.skills == nil {
		return turn.Notice{}, false
	}
	return p.skills.Notice(declared, at), true
}

// Used records info as the system prompt of the newest model call.
func (p *Prompt) Used(info prompt.Info) {
	p.mu.Lock()
	p.info, p.reported = info, false
	p.mu.Unlock()
}

// skillsNow returns the skills that session_info reports: the skills of the files that the system prompt lists and the
// embedded skills of the newest list.
func (p *Prompt) skillsNow() []prompt.Skill {
	p.mu.Lock()
	listed := slices.Clone(p.info.Skills)
	p.mu.Unlock()
	if p.skills == nil {
		return listed
	}
	out := slices.DeleteFunc(listed, func(s prompt.Skill) bool { return s.Path == "" })
	for _, e := range p.skills.Current() {
		out = append(out, prompt.Skill{Name: e.Name})
	}
	slices.SortStableFunc(out, func(a, b prompt.Skill) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// instructions returns the instructions files of the system prompt of the newest model call.
func (p *Prompt) instructions() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.info.Instructions)
}

// Sent records system as the system prompt of the newest turn, in place of the
// segments of the session, for a backend that builds its own prompt.
func (p *Prompt) Sent(system string) {
	p.mu.Lock()
	p.turn, p.reported = []string{system}, true
	p.mu.Unlock()
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
	if ts.Specs != nil {
		for _, s := range ts.Specs {
			call.tools = append(call.tools, s.Name)
		}
	} else {
		for _, t := range ts.Tools {
			call.tools = append(call.tools, t.Spec().Name)
		}
	}
	c.p.mu.Lock()
	c.p.call = call
	c.p.mu.Unlock()
	return ts
}

// sent returns the system segments and the sorted tool names of the newest
// model call. A backend that reported its prompt sent no prompt of the call.
func (p *Prompt) sent() (system, tools []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reported {
		system = slices.Clone(p.turn)
	} else {
		system = slices.Clone(p.info.Segments)
	}
	if p.call.prompt != "" && !p.reported {
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
	return protocol.ToolSpec{Name: SessionInfoName, Description: sessionInfoDescription, InputSchema: json.RawMessage(sessionInfoSchema), ReadOnly: true}
}

// Late makes the call wait for the end of the model call, so that the report holds the usage of that call.
func (*SessionInfo) Late() {}

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
		Skills: append([]prompt.Skill{}, t.prompt.skillsNow()...), Plugins: append([]protocol.Plugin{}, v.Plugins...)}
	if names := t.prompt.instructions(); len(names) > 0 {
		out.Instructions = strings.Join(names, ", ")
	}
	b, err := json.MarshalIndent(out, "", "  ")
	return protocol.ToolResult{Text: string(b)}, err
}
