package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

type fakeAmbientSkills struct {
	skills    []AmbientSkill
	listCalls int
	loadCalls []struct{ id, revision string }
}

func (f *fakeAmbientSkills) ListAdoptedSkills(context.Context) ([]AmbientSkill, error) {
	f.listCalls++
	return append([]AmbientSkill(nil), f.skills...), nil
}

func (f *fakeAmbientSkills) LoadSkill(_ context.Context, id, revision string) (message.Parts, bool, error) {
	f.loadCalls = append(f.loadCalls, struct{ id, revision string }{id, revision})
	return message.Parts{&message.Text{Text: "loaded " + id + "@" + revision}}, false, nil
}

func (f *fakeAmbientSkills) DelegatedLoadSkillToolName() string { return "mcp__boxes__load_skill" }

func TestAmbientSkillsPinsOneDiscoverySnapshotPerPrompt(t *testing.T) {
	source := &fakeAmbientSkills{skills: []AmbientSkill{{ID: "coffee", Revision: "r1", Name: "Coffee", Description: "Make coffee"}}}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("load", loadSkillToolName, `{"id":"coffee","revision":"r1"}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done again"}),
	}}
	s := NewSession(Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		AmbientSkills: source,
	})

	if _, err := s.Prompt(context.Background(), "use my skill"); err != nil {
		t.Fatal(err)
	}
	if source.listCalls != 1 {
		t.Fatalf("list calls = %d, want one for the whole tool loop", source.listCalls)
	}
	if len(source.loadCalls) != 1 || source.loadCalls[0].id != "coffee" || source.loadCalls[0].revision != "r1" {
		t.Fatalf("load calls = %+v, want coffee@r1", source.loadCalls)
	}
	if len(prov.requests) != 2 {
		t.Fatalf("requests = %d, want two tool-loop calls", len(prov.requests))
	}
	for i, req := range prov.requests {
		if !strings.Contains(strings.Join(req.System, "\n"), "Coffee — Make coffee (id: coffee, revision: r1)") {
			t.Errorf("request %d missing pinned catalog: %v", i, req.System)
		}
	}

	source.skills = []AmbientSkill{{ID: "tea", Revision: "r2", Name: "Tea", Description: "Make tea"}}
	if _, err := s.Prompt(context.Background(), "use my new skill"); err != nil {
		t.Fatal(err)
	}
	if source.listCalls != 2 {
		t.Errorf("list calls = %d, want one fresh discovery for each Prompt", source.listCalls)
	}
	if got := strings.Join(prov.requests[2].System, "\n"); !strings.Contains(got, "Tea — Make tea (id: tea, revision: r2)") || strings.Contains(got, "Coffee — Make coffee") {
		t.Errorf("second prompt catalog = %q, want only refreshed tea skill", got)
	}
}

func TestLoadSkillRejectsUnadvertisedRevision(t *testing.T) {
	source := &fakeAmbientSkills{skills: []AmbientSkill{{ID: "coffee", Revision: "r1", Name: "Coffee", Description: "Make coffee"}}}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("load", loadSkillToolName, `{"id":"coffee","revision":"r2"}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		AmbientSkills: source,
	})
	if _, err := s.Prompt(context.Background(), "use my skill"); err != nil {
		t.Fatal(err)
	}
	if len(source.loadCalls) != 0 {
		t.Fatalf("source loaded an unadvertised revision: %+v", source.loadCalls)
	}
	result := prov.requests[1].Messages[len(prov.requests[1].Messages)-1].Parts[0].(*message.ToolResult)
	if !result.IsError || !strings.Contains(result.Content.Text(), "was not advertised") {
		t.Errorf("tool result = %+v, want unadvertised revision error", result)
	}
}

func TestClaudeCodeDelegationPinsAmbientSkillsAcrossResume(t *testing.T) {
	source := &fakeAmbientSkills{skills: []AmbientSkill{{ID: "coffee", Revision: "r1", Name: "Coffee", Description: "Make coffee"}}}
	s, _ := claudeCodeTestSession(t, "normal")
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", stdinLog)
	s.cfg.AmbientSkills = source
	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatal(err)
	}
	if source.listCalls != 2 {
		t.Errorf("list calls = %d, want one per delegated Prompt", source.listCalls)
	}
	if strings.Count(string(stdin), "Coffee — Make coffee") != 1 {
		t.Errorf("unchanged catalog was added more than once: %s", stdin)
	}
	if !strings.Contains(string(stdin), "mcp__boxes__load_skill") {
		t.Errorf("delegated input does not name the MCP load tool: %s", stdin)
	}
}

func TestClaudeCodeDelegationAppendsChangedAndEmptyAmbientCatalog(t *testing.T) {
	source := &fakeAmbientSkills{skills: []AmbientSkill{{ID: "coffee", Revision: "r1", Name: "Coffee", Description: "Make coffee"}}}
	s, _ := claudeCodeTestSession(t, "normal")
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", stdinLog)
	s.cfg.AmbientSkills = source
	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	source.skills = []AmbientSkill{{ID: "tea", Revision: "r2", Name: "Tea", Description: "Make tea"}}
	if _, err := s.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	source.skills = nil
	if _, err := s.Prompt(context.Background(), "third"); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatal(err)
	}
	got := string(stdin)
	if !strings.Contains(got, "Coffee — Make coffee") || !strings.Contains(got, "Tea — Make tea") || !strings.Contains(got, "no adopted skills are available") {
		t.Errorf("delegated input = %s, want initial, changed, and empty catalog notices", got)
	}
}

func TestClaudeCodeDelegationFailsBeforeSpawnWhenAmbientDiscoveryFails(t *testing.T) {
	s, logPath := claudeCodeTestSession(t, "normal")
	s.cfg.AmbientSkills = failingAmbientSkills{}
	if _, err := s.Prompt(context.Background(), "hello"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("Prompt error = %v, want unavailable discovery error", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("Claude Code started after failed discovery; stat error = %v", err)
	}
}

type failingAmbientSkills struct{}

func (failingAmbientSkills) ListAdoptedSkills(context.Context) ([]AmbientSkill, error) {
	return nil, errors.New("source unavailable")
}
func (failingAmbientSkills) LoadSkill(context.Context, string, string) (message.Parts, bool, error) {
	return nil, false, nil
}
func (failingAmbientSkills) DelegatedLoadSkillToolName() string { return "mcp__boxes__load_skill" }

type fakeAmbientMCP struct {
	tool string
	args json.RawMessage
}

func (f *fakeAmbientMCP) Tools(context.Context) []provider.ToolDef { return nil }
func (f *fakeAmbientMCP) CallTool(context.Context, string, json.RawMessage) (message.Parts, bool, error) {
	return nil, false, nil
}
func (f *fakeAmbientMCP) CallServerTool(_ context.Context, _ string, tool string, args json.RawMessage) (message.Parts, bool, error) {
	f.tool, f.args = tool, append(f.args[:0], args...)
	if tool == "list_adopted_skills" {
		return message.Parts{&message.Text{Text: `{"skills":[{"id":"coffee","revision":"r1","name":"Coffee","description":"Make coffee"}]}`}}, false, nil
	}
	return message.Parts{&message.Text{Text: "body"}}, false, nil
}

func TestMCPAmbientSkillSourceUsesAdoptedSkillContract(t *testing.T) {
	mcp := &fakeAmbientMCP{}
	source := MCPAmbientSkillSource{MCP: mcp, Server: "boxes"}
	skills, err := source.ListAdoptedSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mcp.tool != "list_adopted_skills" || string(mcp.args) != "{}" || len(skills) != 1 || skills[0].Revision != "r1" {
		t.Fatalf("list call = %q %s, skills = %+v", mcp.tool, mcp.args, skills)
	}
	if _, _, err := source.LoadSkill(context.Background(), "coffee", "r1"); err != nil {
		t.Fatal(err)
	}
	var args map[string]string
	if err := json.Unmarshal(mcp.args, &args); err != nil {
		t.Fatal(err)
	}
	if mcp.tool != "load_skill" || args["id"] != "coffee" || args["revision"] != "r1" || len(args) != 2 {
		t.Errorf("load call = %q %s", mcp.tool, mcp.args)
	}
}
