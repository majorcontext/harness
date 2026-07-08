package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// writeSkill creates a skill directory <root>/<name> with a SKILL.md whose
// frontmatter uses name and description.
func writeSkill(t *testing.T, root, name, description string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	mkdirAll(t, dir)
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nDo the thing.\n"
	writeInstr(t, filepath.Join(dir, "SKILL.md"), body)
	return dir
}

func TestSkillsInjectedIntoSystem(t *testing.T) {
	work := t.TempDir()
	skills := filepath.Join(work, "skills")
	writeSkill(t, skills, "brewing", "Make coffee")
	writeSkill(t, skills, "alpha", "First alphabetically")

	prov := instrSession(t, Config{WorkDir: work, SkillsDirs: []string{skills}, Instructions: &InstructionsConfig{Disabled: true}}, 1)
	sys := prov.requests[0].System
	if len(sys) != 2 {
		t.Fatalf("system = %v, want [base, skills]", sys)
	}
	seg := sys[1]
	// Header must instruct reading SKILL.md before use.
	if !strings.Contains(seg, "read_file") || !strings.Contains(strings.ToLower(seg), "skill.md") {
		t.Errorf("skills header must mention reading SKILL.md with read_file: %q", seg)
	}
	if !strings.Contains(seg, "alpha — First alphabetically") {
		t.Errorf("missing alpha line: %q", seg)
	}
	if !strings.Contains(seg, "brewing — Make coffee") {
		t.Errorf("missing brewing line: %q", seg)
	}
	alphaPath := filepath.Join(skills, "alpha", "SKILL.md")
	if !strings.Contains(seg, alphaPath) {
		t.Errorf("missing absolute path %q: %q", alphaPath, seg)
	}
	// Sorted order: alpha before brewing.
	if strings.Index(seg, "alpha — ") > strings.Index(seg, "brewing — ") {
		t.Errorf("skills not sorted by name: %q", seg)
	}
}

func TestSkillsSegmentOrder(t *testing.T) {
	work := t.TempDir()
	writeInstr(t, filepath.Join(work, "AGENTS.md"), "instr body")
	skills := filepath.Join(work, "skills")
	writeSkill(t, skills, "one", "Skill one")
	hooks := &fakeHooks{segments: []string{"hook seg"}}

	prov := instrSession(t, Config{WorkDir: work, SkillsDirs: []string{skills}, Hooks: hooks}, 1)
	sys := prov.requests[0].System
	if len(sys) != 4 {
		t.Fatalf("system = %v, want [base, instructions, skills, hook seg]", sys)
	}
	if sys[0] != "base" {
		t.Errorf("sys[0] = %q, want base", sys[0])
	}
	if !strings.Contains(sys[1], "instr body") {
		t.Errorf("sys[1] = %q, want instructions", sys[1])
	}
	if !strings.Contains(sys[2], "one — Skill one") {
		t.Errorf("sys[2] = %q, want skills", sys[2])
	}
	if sys[3] != "hook seg" {
		t.Errorf("sys[3] = %q, want hook seg", sys[3])
	}
}

func TestSkillsDefaultDir(t *testing.T) {
	work := t.TempDir()
	def := filepath.Join(work, ".agents", "skills")
	writeSkill(t, def, "deflt", "Default dir skill")
	// nil SkillsDirs uses <WorkDir>/.agents/skills when it exists.
	prov := instrSession(t, Config{WorkDir: work, Instructions: &InstructionsConfig{Disabled: true}}, 1)
	sys := prov.requests[0].System
	if len(sys) != 2 {
		t.Fatalf("system = %v, want [base, skills] from default dir", sys)
	}
	if !strings.Contains(sys[1], "deflt — Default dir skill") {
		t.Errorf("sys[1] = %q, want default-dir skill", sys[1])
	}
}

func TestSkillsEmptySliceDisables(t *testing.T) {
	work := t.TempDir()
	def := filepath.Join(work, ".agents", "skills")
	writeSkill(t, def, "deflt", "Default dir skill")
	// Explicit empty slice disables discovery even though the default exists.
	prov := instrSession(t, Config{WorkDir: work, SkillsDirs: []string{}, Instructions: &InstructionsConfig{Disabled: true}}, 1)
	sys := prov.requests[0].System
	if len(sys) != 1 || sys[0] != "base" {
		t.Errorf("system = %v, want only [base] when skills explicitly disabled", sys)
	}
}

func TestSkillsMissingDirNoSegment(t *testing.T) {
	work := t.TempDir()
	// A configured dir that does not exist: no segment, no error.
	prov := instrSession(t, Config{
		WorkDir:      work,
		SkillsDirs:   []string{filepath.Join(work, "does-not-exist")},
		Instructions: &InstructionsConfig{Disabled: true},
	}, 1)
	sys := prov.requests[0].System
	if len(sys) != 1 || sys[0] != "base" {
		t.Errorf("system = %v, want only [base] when skills dir missing", sys)
	}
}

func TestSkillsInvalidFailsFirstPrompt(t *testing.T) {
	work := t.TempDir()
	skills := filepath.Join(work, "skills")
	bad := filepath.Join(skills, "bogus")
	mkdirAll(t, bad)
	// name mismatch with directory is a spec violation.
	writeInstr(t, filepath.Join(bad, "SKILL.md"), "---\nname: wrongname\ndescription: x\n---\nbody\n")

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	s := NewSession(Config{
		Providers:    provider.Registry{"test": prov},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		System:       []string{"base"},
		WorkDir:      work,
		SkillsDirs:   []string{skills},
		Instructions: &InstructionsConfig{Disabled: true},
	})
	_, err := s.Prompt(context.Background(), "go")
	if err == nil {
		t.Fatal("expected first Prompt to fail on invalid skill")
	}
	if !strings.Contains(err.Error(), "SKILL.md") {
		t.Errorf("error %q should name the offending SKILL.md", err)
	}
	if len(prov.requests) != 0 {
		t.Errorf("provider called despite skills failure: %d requests", len(prov.requests))
	}
	if len(s.History()) != 0 {
		t.Errorf("history mutated on failed prompt: %d messages", len(s.History()))
	}
}

func TestSkillsDuplicateNamesAcrossDirs(t *testing.T) {
	work := t.TempDir()
	d1 := filepath.Join(work, "d1")
	d2 := filepath.Join(work, "d2")
	writeSkill(t, d1, "dup", "in d1")
	writeSkill(t, d2, "dup", "in d2")

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	s := NewSession(Config{
		Providers:    provider.Registry{"test": prov},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		System:       []string{"base"},
		WorkDir:      work,
		SkillsDirs:   []string{d1, d2},
		Instructions: &InstructionsConfig{Disabled: true},
	})
	_, err := s.Prompt(context.Background(), "go")
	if err == nil {
		t.Fatal("expected duplicate skill name across dirs to fail")
	}
	if !strings.Contains(err.Error(), "dup") || !strings.Contains(err.Error(), d1) || !strings.Contains(err.Error(), d2) {
		t.Errorf("error %q should name the skill and both dirs", err)
	}
	if len(prov.requests) != 0 {
		t.Errorf("provider called despite duplicate skills: %d requests", len(prov.requests))
	}
}

func TestSkillsRediscoveredOnResume(t *testing.T) {
	dir := t.TempDir() // session dir
	work := t.TempDir()
	skills := filepath.Join(work, "skills")
	writeSkill(t, skills, "resumed", "Survives resume")

	cfg := Config{
		Providers:    provider.Registry{"test": &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"})}}},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		System:       []string{"base"},
		WorkDir:      work,
		SessionDir:   dir,
		SkillsDirs:   []string{skills},
		Instructions: &InstructionsConfig{Disabled: true},
	}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	id := s.ID

	// Resume: a fresh provider so we can inspect the resumed request.
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "two"})}}
	cfg.Providers = provider.Registry{"test": prov}
	s2, err := LoadSession(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	sys := prov.requests[0].System
	found := false
	for _, seg := range sys {
		if strings.Contains(seg, "resumed — Survives resume") {
			found = true
		}
	}
	if !found {
		t.Errorf("resumed session missing rediscovered skills segment: %v", sys)
	}
}

// TestSkillsNeverPersisted verifies the skills segment is not written to the
// session log (the log stores only canonical messages).
func TestSkillsNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	work := t.TempDir()
	skills := filepath.Join(work, "skills")
	writeSkill(t, skills, "secret", "Not in the log")

	cfg := Config{
		Providers:    provider.Registry{"test": &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"})}}},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		System:       []string{"base"},
		WorkDir:      work,
		SessionDir:   dir,
		SkillsDirs:   []string{skills},
		Instructions: &InstructionsConfig{Disabled: true},
	}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Errorf("skills segment leaked into the session log:\n%s", data)
	}
}
