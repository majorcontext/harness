package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

func TestBuiltinAgentDefsMatchDesignTable(t *testing.T) {
	gp := builtinAgentDefs[AgentGeneralPurpose]
	if gp.Tools != nil {
		t.Errorf("general-purpose Tools = %v, want nil (full set)", gp.Tools)
	}

	for _, name := range []string{AgentExplore, AgentPlan} {
		def := builtinAgentDefs[name]
		if len(def.Tools) == 0 {
			t.Fatalf("%s: Tools empty, want a read-only restriction", name)
		}
		for _, forbidden := range []string{"bash", "write_file", "edit_file", "task"} {
			for _, tool := range def.Tools {
				if tool == forbidden {
					t.Errorf("%s: Tools contains %q, want a read-only leaf", name, forbidden)
				}
			}
		}
	}

	if !strings.Contains(builtinAgentDefs[AgentPlan].SystemAppend, "plan") {
		t.Errorf("plan SystemAppend = %q, want it to mention returning a plan", builtinAgentDefs[AgentPlan].SystemAppend)
	}
}

func writeAgentDef(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAgentDefsMissingDirIsNotError(t *testing.T) {
	defs, err := LoadAgentDefs(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("LoadAgentDefs: %v", err)
	}
	if defs != nil {
		t.Errorf("defs = %v, want nil", defs)
	}
}

func TestLoadAgentDefsParsesFrontmatterAndBody(t *testing.T) {
	dir := t.TempDir()
	writeAgentDef(t, dir, "code-reviewer.md", `---
name: code-reviewer
description: Reviews diffs for correctness and convention adherence
tools: read_file, glob, grep
model: anthropic/claude-fable-5
---
You are a meticulous reviewer. Check for correctness and style.
`)
	defs, err := LoadAgentDefs(dir)
	if err != nil {
		t.Fatalf("LoadAgentDefs: %v", err)
	}
	def, ok := defs["code-reviewer"]
	if !ok {
		t.Fatalf("code-reviewer not loaded: %v", defs)
	}
	if def.Description != "Reviews diffs for correctness and convention adherence" {
		t.Errorf("description = %q", def.Description)
	}
	wantTools := []string{"read_file", "glob", "grep"}
	if len(def.Tools) != len(wantTools) {
		t.Fatalf("tools = %v, want %v", def.Tools, wantTools)
	}
	for i, tool := range wantTools {
		if def.Tools[i] != tool {
			t.Errorf("tools[%d] = %q, want %q", i, def.Tools[i], tool)
		}
	}
	if def.Model != (message.ModelRef{Provider: "anthropic", Model: "claude-fable-5"}) {
		t.Errorf("model = %+v", def.Model)
	}
	if !strings.Contains(def.SystemAppend, "meticulous reviewer") {
		t.Errorf("SystemAppend = %q", def.SystemAppend)
	}
	if def.Source != filepath.Join(dir, "code-reviewer.md") {
		t.Errorf("source = %q", def.Source)
	}
}

func TestLoadAgentDefsOmittedToolsMeansFullSet(t *testing.T) {
	dir := t.TempDir()
	writeAgentDef(t, dir, "helper.md", `---
name: helper
description: A helper with the full tool set
---
Be helpful.
`)
	defs, err := LoadAgentDefs(dir)
	if err != nil {
		t.Fatalf("LoadAgentDefs: %v", err)
	}
	if defs["helper"].Tools != nil {
		t.Errorf("Tools = %v, want nil", defs["helper"].Tools)
	}
}

func TestLoadAgentDefsIgnoresSkillsSubdirAndNonMdFiles(t *testing.T) {
	dir := t.TempDir()
	writeAgentDef(t, dir, "real.md", `---
name: real
description: A real definition
---
body
`)
	writeAgentDef(t, filepath.Join(dir, "skills", "some-skill"), "SKILL.md", `---
name: some-skill
description: This lives under skills/ and must never be treated as an agent def
---
body
`)
	writeAgentDef(t, dir, "notes.txt", "not a definition")

	defs, err := LoadAgentDefs(dir)
	if err != nil {
		t.Fatalf("LoadAgentDefs: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("defs = %v, want exactly [real]", defs)
	}
	if _, ok := defs["real"]; !ok {
		t.Errorf("real definition missing: %v", defs)
	}
}

func TestLoadAgentDefsUnknownToolIsLoadError(t *testing.T) {
	dir := t.TempDir()
	writeAgentDef(t, dir, "bad.md", `---
name: bad
description: References a tool that does not exist
tools: read_file, teleport
---
body
`)
	if _, err := LoadAgentDefs(dir); err == nil {
		t.Error("LoadAgentDefs with unknown tool: want error, got nil")
	} else if !strings.Contains(err.Error(), "teleport") {
		t.Errorf("error = %v, want it to name the unknown tool", err)
	}
}

func TestLoadAgentDefsMissingNameIsLoadError(t *testing.T) {
	dir := t.TempDir()
	writeAgentDef(t, dir, "bad.md", `---
description: No name given
---
body
`)
	if _, err := LoadAgentDefs(dir); err == nil {
		t.Error("LoadAgentDefs with missing name: want error, got nil")
	}
}

func TestLoadAgentDefsCollisionWithBuiltinIsLoadError(t *testing.T) {
	dir := t.TempDir()
	writeAgentDef(t, dir, "explore.md", `---
name: explore
description: Tries to shadow the built-in explore agent
---
body
`)
	if _, err := LoadAgentDefs(dir); err == nil {
		t.Error("LoadAgentDefs colliding with a builtin: want error, got nil")
	}
}

func TestLoadAgentDefsDuplicateNameIsLoadError(t *testing.T) {
	dir := t.TempDir()
	writeAgentDef(t, dir, "a.md", `---
name: dup
description: First
---
body
`)
	writeAgentDef(t, dir, "b.md", `---
name: dup
description: Second
---
body
`)
	if _, err := LoadAgentDefs(dir); err == nil {
		t.Error("LoadAgentDefs with duplicate name: want error, got nil")
	}
}

func TestLoadAgentDefsUnknownFrontmatterKeyIsLoadError(t *testing.T) {
	dir := t.TempDir()
	writeAgentDef(t, dir, "bad.md", `---
name: bad
description: has a bogus key
color: blue
---
body
`)
	if _, err := LoadAgentDefs(dir); err == nil {
		t.Error("LoadAgentDefs with unknown frontmatter key: want error, got nil")
	}
}

func TestLoadAgentDefsMissingDelimiterIsLoadError(t *testing.T) {
	dir := t.TempDir()
	writeAgentDef(t, dir, "bad.md", "not frontmatter at all\n")
	if _, err := LoadAgentDefs(dir); err == nil {
		t.Error("LoadAgentDefs with no frontmatter: want error, got nil")
	}
}

func TestResolveAgentDefsMergesBuiltinsAndCustom(t *testing.T) {
	root := t.TempDir()
	writeAgentDef(t, agentDefsDir(root), "custom.md", `---
name: custom
description: A custom one
---
body
`)
	defs, err := ResolveAgentDefs(root)
	if err != nil {
		t.Fatalf("ResolveAgentDefs: %v", err)
	}
	for _, name := range []string{AgentGeneralPurpose, AgentExplore, AgentPlan, "custom"} {
		if _, ok := defs[name]; !ok {
			t.Errorf("ResolveAgentDefs missing %q: %v", name, defs)
		}
	}
}
