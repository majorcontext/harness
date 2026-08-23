// Self-inspection: the built-in session_info tool lets the model ask what it is
// actually running with. It reports the session's identity, current model,
// reasoning-effort/thinking level, cumulative token usage, the exact
// system-prompt segments assembled for the current turn, the active tool
// names, the provenance of injected project instructions and Agent Skills,
// and the configured plugins (name, spawn state, registered tools, subscribed
// hooks). It takes no arguments and touches no disk or network — it reflects
// state the engine already holds in memory.

package engine

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// skillInfo is one entry in the session_info skill catalog: a skill's name and
// the absolute path to its SKILL.md.
type skillInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// sessionInfoResult is the JSON payload the session_info tool returns.
type sessionInfoResult struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	// Effort is the session's current reasoning-effort level: "off", "minimal",
	// "low", "medium", or "high". Empty string is EffortUnset — no reasoning
	// control has been sent, so the provider runs its own default — and is
	// reported as "" rather than omitted or guessed, the same convention
	// setThinkingResponseJSON uses for POST /session/{id}/thinking (see
	// server/handlers.go). Swapped only via Session.SetEffort, the same
	// choke point handleSetThinking calls.
	Effort       message.Effort `json:"effort"`
	Usage        provider.Usage `json:"usage"`
	System       []string       `json:"system"`
	Tools        []string       `json:"tools"`
	Instructions string         `json:"instructions"` // source path, or "none"
	Skills       []skillInfo    `json:"skills"`
	// Plugins lists the configured plugins (name, spawn state, registered
	// tools, subscribed hooks). It reports CONFIGURED plugins, not only
	// spawned ones — a plugin spawns lazily, so a not-yet-spawned plugin
	// still appears. Empty (not null) when no plugins are configured.
	Plugins []plugin.Info `json:"plugins"`
}

// sessionInfoTool is the built-in self-inspection tool, registered in
// NewSession alongside bash and the file tools.
func sessionInfoTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name: "session_info",
			Description: "Report this session's own configuration: session id, current model, " +
				"the reasoning-effort/thinking level (\"off\", \"minimal\", \"low\", \"medium\", \"high\", " +
				"or \"\" if unset — the provider default), cumulative token usage, the exact " +
				"system-prompt segments you received this turn, the active tool names, the " +
				"provenance of any injected project instructions (AGENTS.md path or \"none\"), " +
				"the discovered Agent Skills (names and SKILL.md paths), and the configured plugins " +
				"(name, spawn state, registered tools, subscribed hooks). Takes no arguments.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		Run: func(ctx context.Context, s *Session, _ json.RawMessage) (message.Parts, error) {
			info := s.sessionInfo(ctx)
			b, err := json.MarshalIndent(info, "", "  ")
			if err != nil {
				return nil, err
			}
			return message.Parts{&message.Text{Text: string(b)}}, nil
		},
	}
}

// sessionInfo snapshots the session's current self-description. Tool names are
// gathered outside the lock (toolDefs may call into the plugin host), then the
// mutable state is read under mu.
func (s *Session) sessionInfo(ctx context.Context) sessionInfoResult {
	tools := make([]string, 0, len(s.tools))
	for _, d := range s.toolDefs(ctx) {
		tools = append(tools, d.Name)
	}
	sort.Strings(tools)

	// Plugins is read outside mu, like tool names above, but for a
	// different reason. Host.Tools reads a cached manifest and takes no
	// lock. Host.Plugins also takes no lock: each instance's spawn state
	// comes from a lock-free atomic snapshot (instance.liveState in
	// plugin/host.go), never from inst.mu. A plugin spawn holds inst.mu
	// for the whole dial-plus-handshake, and Host is a box-scoped
	// singleton shared by every session on the box. A state read gated on
	// inst.mu would let one session's spawn stall this call for every
	// other session too.
	plugins := s.Plugins()

	s.mu.Lock()
	defer s.mu.Unlock()
	instr := s.instrPath
	if instr == "" {
		instr = "none"
	}
	skills := append([]skillInfo(nil), s.skills...)
	if skills == nil {
		skills = []skillInfo{}
	}
	system := append([]string(nil), s.lastSystem...)
	if system == nil {
		system = []string{}
	}
	return sessionInfoResult{
		SessionID:    s.ID,
		Model:        s.model.String(),
		Effort:       s.effort,
		Usage:        s.usage,
		System:       system,
		Tools:        tools,
		Instructions: instr,
		Skills:       skills,
		Plugins:      plugins,
	}
}
