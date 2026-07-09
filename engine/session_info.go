// Self-inspection: the built-in session_info tool lets the model ask what it is
// actually running with. It reports the session's identity, current model,
// cumulative token usage, the exact system-prompt segments assembled for the
// current turn, the active tool names, and the provenance of injected project
// instructions and Agent Skills. It takes no arguments and touches no disk or
// network — it reflects state the engine already holds in memory.

package engine

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/majorcontext/harness/message"
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
	SessionID    string         `json:"session_id"`
	Model        string         `json:"model"`
	Usage        provider.Usage `json:"usage"`
	System       []string       `json:"system"`
	Tools        []string       `json:"tools"`
	Instructions string         `json:"instructions"` // source path, or "none"
	Skills       []skillInfo    `json:"skills"`
}

// sessionInfoTool is the built-in self-inspection tool, registered in
// NewSession alongside bash and the file tools.
func sessionInfoTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name: "session_info",
			Description: "Report this session's own configuration: session id, current model, " +
				"cumulative token usage, the exact system-prompt segments you received this turn, " +
				"the active tool names, the provenance of any injected project instructions " +
				"(AGENTS.md path or \"none\"), and the discovered Agent Skills (names and SKILL.md paths). " +
				"Takes no arguments.",
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
		Usage:        s.usage,
		System:       system,
		Tools:        tools,
		Instructions: instr,
		Skills:       skills,
	}
}
