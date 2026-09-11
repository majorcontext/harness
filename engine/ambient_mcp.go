package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// AmbientSkill is one adopted skill advertised by an AmbientSkillSource.
type AmbientSkill struct {
	ID          string `json:"id"`
	Revision    string `json:"revision"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// AmbientSkillSource supplies adopted personal skills outside the workspace.
// ListAdoptedSkills runs once before each native Prompt and LoadSkill receives
// only an id and revision from that run's advertised snapshot.
type AmbientSkillSource interface {
	ListAdoptedSkills(ctx context.Context) ([]AmbientSkill, error)
	LoadSkill(ctx context.Context, id, revision string) (message.Parts, bool, error)
}

// MCPAmbientSkillSource adapts the adopted-skill MCP contract for the engine.
// Server names the configured MCP server that implements list_adopted_skills
// and load_skill.
type MCPAmbientSkillSource struct {
	MCP    MCPRegistry
	Server string
}

func (s MCPAmbientSkillSource) ListAdoptedSkills(ctx context.Context) ([]AmbientSkill, error) {
	if s.MCP == nil || s.Server == "" {
		return nil, fmt.Errorf("ambient skills: MCP server is not configured")
	}
	out, isErr, err := s.MCP.CallServerTool(ctx, s.Server, "list_adopted_skills", json.RawMessage(`{}`))
	if err != nil {
		return nil, fmt.Errorf("ambient skills: list_adopted_skills: %w", err)
	}
	if isErr {
		return nil, fmt.Errorf("ambient skills: list_adopted_skills returned an error")
	}
	var response struct {
		Skills []AmbientSkill `json:"skills"`
	}
	if err := json.Unmarshal([]byte(out.Text()), &response); err != nil {
		return nil, fmt.Errorf("ambient skills: invalid list_adopted_skills result: %w", err)
	}
	return response.Skills, nil
}

func (s MCPAmbientSkillSource) LoadSkill(ctx context.Context, id, revision string) (message.Parts, bool, error) {
	if s.MCP == nil || s.Server == "" {
		return nil, false, fmt.Errorf("ambient skills: MCP server is not configured")
	}
	args, err := json.Marshal(struct {
		ID       string `json:"id"`
		Revision string `json:"revision"`
	}{id, revision})
	if err != nil {
		return nil, false, fmt.Errorf("ambient skills: encoding load_skill arguments: %w", err)
	}
	return s.MCP.CallServerTool(ctx, s.Server, "load_skill", args)
}

type delegatedAmbientSkillSource interface {
	DelegatedLoadSkillToolName() string
}

func (s MCPAmbientSkillSource) DelegatedLoadSkillToolName() string {
	return mcpToolName(s.Server, "load_skill")
}

const (
	loadSkillToolName   = "load_skill"
	ambientSkillsHeader = "Available personal skills. Each skill below is a capability you can use, " +
		"but only its name and description are shown here. To activate a skill, call " +
		"the load_skill tool with its exact id and revision from this list."
)

// refreshAmbientSkills reads a live source once for the next native Prompt
// run. The snapshot stays unchanged while that Prompt's tool rounds run.
func (s *Session) refreshAmbientSkills(ctx context.Context) error {
	if s.cfg.AmbientSkills == nil {
		return nil
	}
	skills, err := s.cfg.AmbientSkills.ListAdoptedSkills(ctx)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(skills))
	for _, skill := range skills {
		if skill.ID == "" || skill.Revision == "" || skill.Name == "" || skill.Description == "" {
			return fmt.Errorf("ambient skills: list_adopted_skills returned a skill with a missing id, revision, name, or description")
		}
		key := skill.ID + "\x00" + skill.Revision
		if seen[key] {
			return fmt.Errorf("ambient skills: list_adopted_skills returned duplicate skill id %q revision %q", skill.ID, skill.Revision)
		}
		seen[key] = true
	}
	sort.Slice(skills, func(i, j int) bool {
		if skills[i].Name != skills[j].Name {
			return skills[i].Name < skills[j].Name
		}
		if skills[i].ID != skills[j].ID {
			return skills[i].ID < skills[j].ID
		}
		return skills[i].Revision < skills[j].Revision
	})

	var segment strings.Builder
	if len(skills) > 0 {
		segment.WriteString(ambientSkillsHeader)
		for _, skill := range skills {
			fmt.Fprintf(&segment, "\n%s — %s (id: %s, revision: %s)", skill.Name, skill.Description, skill.ID, skill.Revision)
		}
	}
	s.mu.Lock()
	s.ambientSkills = append(s.ambientSkills[:0], skills...)
	s.ambientSkillsSeg = segment.String()
	s.mu.Unlock()
	return nil
}

func (s *Session) ambientSkillsSegment() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ambientSkillsSeg
}

// delegatedAmbientSkillsSegment returns a catalog only when it differs from
// the catalog already sent to the resumed Claude Code session. The source must
// name the MCP tool Claude Code can call directly; native built-in tools do not
// exist in the delegated CLI process.
func (s *Session) delegatedAmbientSkillsSegment() (string, error) {
	if s.cfg.AmbientSkills == nil {
		return "", nil
	}
	namer, ok := s.cfg.AmbientSkills.(delegatedAmbientSkillSource)
	if !ok {
		return "", fmt.Errorf("ambient skills: delegated Claude Code requires an MCP-backed source")
	}
	tool := namer.DelegatedLoadSkillToolName()
	if tool == "" {
		return "", fmt.Errorf("ambient skills: delegated Claude Code source has no load_skill MCP tool")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ambientSkillsEqual(s.ambientSkills, s.delegatedAmbientSkills) {
		return "", nil
	}
	s.delegatedAmbientSkills = append(s.delegatedAmbientSkills[:0], s.ambientSkills...)
	if len(s.ambientSkills) == 0 {
		return "[personal skills: no adopted skills are available.]", nil
	}
	var segment strings.Builder
	fmt.Fprintf(&segment, "Available personal skills. Call %s with the exact id and revision below to activate one.", tool)
	for _, skill := range s.ambientSkills {
		fmt.Fprintf(&segment, "\n%s — %s (id: %s, revision: %s)", skill.Name, skill.Description, skill.ID, skill.Revision)
	}
	return segment.String(), nil
}

func ambientSkillsEqual(a, b []AmbientSkill) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func loadSkillTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        loadSkillToolName,
			Description: "Load the exact adopted personal skill advertised in the current system prompt.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"revision":{"type":"string"}},"required":["id","revision"],"additionalProperties":false}`),
		},
		Run: runLoadSkill,
	}
}

func runLoadSkill(ctx context.Context, s *Session, raw json.RawMessage) (message.Parts, error) {
	var in struct {
		ID       string `json:"id"`
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("load_skill: invalid arguments: %w", err)
	}
	if in.ID == "" || in.Revision == "" {
		return nil, fmt.Errorf("load_skill: both %q and %q are required", "id", "revision")
	}
	s.mu.Lock()
	advertised := false
	for _, skill := range s.ambientSkills {
		if skill.ID == in.ID && skill.Revision == in.Revision {
			advertised = true
			break
		}
	}
	s.mu.Unlock()
	if !advertised {
		return nil, fmt.Errorf("load_skill: %q at revision %q was not advertised for this run", in.ID, in.Revision)
	}
	out, isErr, err := s.cfg.AmbientSkills.LoadSkill(ctx, in.ID, in.Revision)
	if err != nil {
		return nil, fmt.Errorf("load_skill: %w", err)
	}
	if isErr {
		return out, fmt.Errorf("load_skill: source returned an error")
	}
	return out, nil
}
