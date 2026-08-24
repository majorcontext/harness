package engine

import (
	"bytes"
	"log/slog"
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

// captureSlog redirects the default slog logger to an in-memory buffer for
// the duration of the test, restoring the previous default via
// t.Cleanup — mirrors context_window_test.go's identical pattern.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestLoadAgentDefsUnknownToolIsLoadError is the regression test for a
// follow-up finding ("frontmatter leniency"): a single malformed *.md
// file used to abort discovery for the WHOLE directory (`return nil,
// err`) — and because AgentDefs caches a load failure for the session's
// entire life, one contributor's typo in ONE file broke EVERY custom
// agent type for every task call in every session rooted there. A
// malformed file is now skipped (with a logged warning), not a load
// error — proves both halves: the bad file is absent from the result,
// and loading continues rather than failing outright.
// TestLoadAgentDefsUnknownToolIsLoadError proves a design-owner decision
// narrowing "frontmatter leniency"'s scope: an unknown tool name is a
// SEMANTIC authoring mistake the design doc requires to be "an error
// surfaced at load, not spawn" — unlike a stray unknown frontmatter key
// (see TestLoadAgentDefsUnknownFrontmatterKeyIsLenient), it fails the
// WHOLE directory's load, not just this one file.
func TestLoadAgentDefsUnknownToolIsLoadError(t *testing.T) {
	dir := t.TempDir()
	writeAgentDef(t, dir, "bad.md", `---
name: bad
description: References a tool that does not exist
tools: read_file, teleport
---
body
`)
	defs, err := LoadAgentDefs(dir)
	if err == nil {
		t.Fatalf("LoadAgentDefs with unknown tool: want a load error, got defs=%v nil error", defs)
	}
	if !strings.Contains(err.Error(), "teleport") {
		t.Errorf("error does not name the unknown tool: %v", err)
	}
	if defs != nil {
		t.Errorf("defs = %v, want nil on a load error", defs)
	}
}

// TestLoadAgentDefsSemanticErrorTakesPrecedenceOverUnknownKey is the
// regression test for a live review finding on parseAgentDef's own
// ordering: an earlier version of its frontmatter-parsing loop returned
// errUnknownFrontmatterKey (the ONE lenient error class — see its own doc
// comment) the INSTANT it hit an unknown key, before ever reaching the
// tools:/model: semantic validation that only runs once the full
// frontmatter block has been collected. A file with BOTH a stray unknown
// key AND a genuine semantic mistake (here, an unknown tool name) — with
// the unknown key's line coming first in the file — reported only the
// lenient error, silently discarding the hard one LoadAgentDefs actually
// needed to fail the whole directory's load for: leniency is for unknown
// KEYS only, and must never suppress the report of a co-occurring
// semantic mistake.
func TestLoadAgentDefsSemanticErrorTakesPrecedenceOverUnknownKey(t *testing.T) {
	dir := t.TempDir()
	writeAgentDef(t, dir, "bad.md", `---
name: bad
description: has both a bogus key and a bad tool name
color: blue
tools: read_file, teleport
---
body
`)
	defs, err := LoadAgentDefs(dir)
	if err == nil {
		t.Fatalf("LoadAgentDefs with an unknown key AND an unknown tool: want a load error, got defs=%v nil error", defs)
	}
	if !strings.Contains(err.Error(), "teleport") {
		t.Errorf("error does not name the unknown tool (the unknown key error won instead): %v", err)
	}
	if defs != nil {
		t.Errorf("defs = %v, want nil on a load error", defs)
	}
}

// TestLoadAgentDefsMissingNameIsLoadError: a missing required field is a
// structural authoring mistake, not the ONE lenient "unknown frontmatter
// key" case — hard load error for the whole directory.
func TestLoadAgentDefsMissingNameIsLoadError(t *testing.T) {
	dir := t.TempDir()
	writeAgentDef(t, dir, "bad.md", `---
description: No name given
---
body
`)
	defs, err := LoadAgentDefs(dir)
	if err == nil {
		t.Fatalf("LoadAgentDefs with missing name: want a load error, got defs=%v nil error", defs)
	}
	if defs != nil {
		t.Errorf("defs = %v, want nil on a load error", defs)
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

// TestLoadAgentDefsUnknownFrontmatterKeyIsLenient is the ONE remaining
// leniency case after the design-owner narrowing — see
// errUnknownFrontmatterKey's own doc comment (agentdef.go) for the full
// reasoning: a stray typo'd frontmatter key is judged low-stakes and
// cosmetic enough to skip just this one file with a logged warning,
// unlike every other parseAgentDef error.
func TestLoadAgentDefsUnknownFrontmatterKeyIsLenient(t *testing.T) {
	buf := captureSlog(t)
	dir := t.TempDir()
	writeAgentDef(t, dir, "bad.md", `---
name: bad
description: has a bogus key
color: blue
---
body
`)
	defs, err := LoadAgentDefs(dir)
	if err != nil {
		t.Fatalf("LoadAgentDefs with unknown frontmatter key: want nil error (skip-and-warn), got %v", err)
	}
	if _, ok := defs["bad"]; ok {
		t.Error("malformed def \"bad\" present in result, want skipped")
	}
	if !strings.Contains(buf.String(), "color") {
		t.Errorf("no warning logged naming the unknown key: %s", buf.String())
	}
}

// TestLoadAgentDefsMissingDelimiterIsLoadError: a structural frontmatter
// problem (no "---" delimiter at all) is not the one lenient case — hard
// load error for the whole directory, matching the design doc's
// original, pre-leniency-fix behavior for everything except an unknown
// frontmatter key.
func TestLoadAgentDefsMissingDelimiterIsLoadError(t *testing.T) {
	dir := t.TempDir()
	writeAgentDef(t, dir, "bad.md", "not frontmatter at all\n")
	defs, err := LoadAgentDefs(dir)
	if err == nil {
		t.Fatalf("LoadAgentDefs with no frontmatter: want a load error, got defs=%v nil error", defs)
	}
	if defs != nil {
		t.Errorf("defs = %v, want nil on a load error", defs)
	}
}

// TestLoadAgentDefsSkipsMalformedFileAndLoadsRest proves the leniency
// case's own scope-of-damage guarantee still holds for the one error
// class it still covers: a file with an unknown frontmatter key does not
// just fail to load itself, it must not prevent a GOOD file sitting
// right next to it from loading.
func TestLoadAgentDefsSkipsMalformedFileAndLoadsRest(t *testing.T) {
	buf := captureSlog(t)
	dir := t.TempDir()
	writeAgentDef(t, dir, "good.md", `---
name: good
description: A perfectly valid definition
---
body
`)
	writeAgentDef(t, dir, "bad.md", `---
name: bad
description: has a bogus key
color: blue
---
body
`)

	defs, err := LoadAgentDefs(dir)
	if err != nil {
		t.Fatalf("LoadAgentDefs: %v", err)
	}
	if _, ok := defs["good"]; !ok {
		t.Errorf("valid def \"good\" missing from result: %v", defs)
	}
	if _, ok := defs["bad"]; ok {
		t.Error("malformed def \"bad\" present in result, want skipped")
	}
	if !strings.Contains(buf.String(), "bad.md") {
		t.Errorf("no warning logged naming the skipped file: %s", buf.String())
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
	defs, err := ResolveAgentDefs([]string{agentDefsDir(root)})
	if err != nil {
		t.Fatalf("ResolveAgentDefs: %v", err)
	}
	for _, name := range []string{AgentGeneralPurpose, AgentExplore, AgentPlan, "custom"} {
		if _, ok := defs[name]; !ok {
			t.Errorf("ResolveAgentDefs missing %q: %v", name, defs)
		}
	}
}

// TestResolveAgentDefsMergesAcrossDirs and
// TestResolveAgentDefsDuplicateNameAcrossDirsIsError are the regression
// tests for a follow-up finding ("def search path"): ResolveAgentDefs
// used to accept exactly one directory (implicitly, via a single workDir
// string), mirroring SkillsDirs' own multi-directory contract now that
// Config.AgentDefsDirs exists.
func TestResolveAgentDefsMergesAcrossDirs(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	writeAgentDef(t, dirA, "from-a.md", `---
name: from-a
description: Defined in dir A
---
body
`)
	writeAgentDef(t, dirB, "from-b.md", `---
name: from-b
description: Defined in dir B
---
body
`)
	defs, err := ResolveAgentDefs([]string{dirA, dirB})
	if err != nil {
		t.Fatalf("ResolveAgentDefs: %v", err)
	}
	if _, ok := defs["from-a"]; !ok {
		t.Errorf("missing from-a: %v", defs)
	}
	if _, ok := defs["from-b"]; !ok {
		t.Errorf("missing from-b: %v", defs)
	}
}

func TestResolveAgentDefsDuplicateNameAcrossDirsIsError(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	writeAgentDef(t, dirA, "a.md", `---
name: dup
description: First, in dir A
---
body
`)
	writeAgentDef(t, dirB, "b.md", `---
name: dup
description: Second, in dir B
---
body
`)
	if _, err := ResolveAgentDefs([]string{dirA, dirB}); err == nil {
		t.Error("ResolveAgentDefs with the same name defined in two dirs: want error, got nil")
	}
}
