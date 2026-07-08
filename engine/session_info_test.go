package engine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// decodedSessionInfo mirrors the JSON the session_info tool emits.
type decodedSessionInfo struct {
	SessionID    string   `json:"session_id"`
	Model        string   `json:"model"`
	System       []string `json:"system"`
	Tools        []string `json:"tools"`
	Instructions string   `json:"instructions"`
	Skills       []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"skills"`
	Usage provider.Usage `json:"usage"`
}

// callSessionInfo runs a session whose model calls session_info on the first
// turn, then returns the decoded tool result.
func callSessionInfo(t *testing.T, cfg Config) decodedSessionInfo {
	t.Helper()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "session_info", `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	cfg.Providers = provider.Registry{"test": prov}
	cfg.Model = message.ModelRef{Provider: "test", Model: "m1"}
	if cfg.System == nil {
		cfg.System = []string{"base"}
	}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	h := s.History()
	// user, assistant(tool call), tool(result), assistant.
	if len(h) != 4 {
		t.Fatalf("history = %d messages, want 4", len(h))
	}
	tr, ok := h[2].Parts[0].(*message.ToolResult)
	if !ok {
		t.Fatalf("h[2].Parts[0] = %T, want ToolResult", h[2].Parts[0])
	}
	if tr.IsError {
		t.Fatalf("session_info returned an error: %s", tr.Content.Text())
	}
	var info decodedSessionInfo
	if err := json.Unmarshal([]byte(tr.Content.Text()), &info); err != nil {
		t.Fatalf("decoding session_info result %q: %v", tr.Content.Text(), err)
	}
	if info.SessionID != s.ID {
		t.Errorf("session_id = %q, want %q", info.SessionID, s.ID)
	}
	return info
}

func TestSessionInfoReportsInjectedContext(t *testing.T) {
	work := t.TempDir()
	writeInstr(t, filepath.Join(work, "AGENTS.md"), "PROJECT_RULE_XYZ applies here")
	skills := filepath.Join(work, "skills")
	writeSkill(t, skills, "demo", "A demo skill")

	info := callSessionInfo(t, Config{WorkDir: work, SkillsDirs: []string{skills}})

	if info.Model != "test/m1" {
		t.Errorf("model = %q, want test/m1", info.Model)
	}
	joined := strings.Join(info.System, "\n")
	if !strings.Contains(joined, "PROJECT_RULE_XYZ applies here") {
		t.Errorf("system missing AGENTS.md content:\n%s", joined)
	}
	if !strings.Contains(joined, "demo — A demo skill") {
		t.Errorf("system missing skill catalog line:\n%s", joined)
	}
	if !strings.Contains(info.Instructions, "AGENTS.md") {
		t.Errorf("instructions provenance = %q, want it to name AGENTS.md", info.Instructions)
	}
	if len(info.Skills) != 1 || info.Skills[0].Name != "demo" {
		t.Fatalf("skills = %+v, want one named demo", info.Skills)
	}
	wantPath := filepath.Join(skills, "demo", "SKILL.md")
	if info.Skills[0].Path != wantPath {
		t.Errorf("skill path = %q, want %q", info.Skills[0].Path, wantPath)
	}
	if !containsStr(info.Tools, "session_info") {
		t.Errorf("tools = %v, want to include session_info", info.Tools)
	}
	if !containsStr(info.Tools, "bash") {
		t.Errorf("tools = %v, want to include bash", info.Tools)
	}
}

func TestSessionInfoNothingInjected(t *testing.T) {
	work := t.TempDir()
	mkdirAll(t, filepath.Join(work, ".git")) // bound the AGENTS.md walk

	info := callSessionInfo(t, Config{
		WorkDir:      work,
		Instructions: &InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{}, // explicit disable
	})

	if info.Instructions != "none" {
		t.Errorf("instructions = %q, want none", info.Instructions)
	}
	if len(info.Skills) != 0 {
		t.Errorf("skills = %+v, want empty", info.Skills)
	}
	// System still carries the base segment.
	if len(info.System) != 1 || info.System[0] != "base" {
		t.Errorf("system = %v, want [base]", info.System)
	}
}
