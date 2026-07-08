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

func writeInstr(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func TestLoadInstructionsFoundInWorkDir(t *testing.T) {
	dir := t.TempDir()
	writeInstr(t, filepath.Join(dir, "AGENTS.md"), "be terse")
	content, path, err := loadInstructions(dir)
	if err != nil {
		t.Fatalf("loadInstructions: %v", err)
	}
	if content != "be terse" {
		t.Errorf("content = %q, want %q", content, "be terse")
	}
	if path != "AGENTS.md" {
		t.Errorf("path = %q, want AGENTS.md", path)
	}
}

func TestLoadInstructionsWalksUp(t *testing.T) {
	root := t.TempDir()
	writeInstr(t, filepath.Join(root, "AGENTS.md"), "root rules")
	sub := filepath.Join(root, "a", "b")
	mkdirAll(t, sub)
	content, path, err := loadInstructions(sub)
	if err != nil {
		t.Fatalf("loadInstructions: %v", err)
	}
	if content != "root rules" {
		t.Errorf("content = %q, want %q", content, "root rules")
	}
	if want := filepath.Join("..", "..", "AGENTS.md"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestLoadInstructionsGitRoot(t *testing.T) {
	t.Run("stops above git root", func(t *testing.T) {
		outer := t.TempDir()
		writeInstr(t, filepath.Join(outer, "AGENTS.md"), "outer rules")
		repo := filepath.Join(outer, "repo")
		mkdirAll(t, filepath.Join(repo, ".git"))
		sub := filepath.Join(repo, "pkg")
		mkdirAll(t, sub)
		content, path, err := loadInstructions(sub)
		if err != nil {
			t.Fatalf("loadInstructions: %v", err)
		}
		if content != "" || path != "" {
			t.Errorf("expected no instructions (walk stopped at git root), got content=%q path=%q", content, path)
		}
	})
	t.Run("found at git root", func(t *testing.T) {
		outer := t.TempDir()
		writeInstr(t, filepath.Join(outer, "AGENTS.md"), "outer rules")
		repo := filepath.Join(outer, "repo")
		mkdirAll(t, filepath.Join(repo, ".git"))
		writeInstr(t, filepath.Join(repo, "AGENTS.md"), "repo rules")
		sub := filepath.Join(repo, "pkg")
		mkdirAll(t, sub)
		content, _, err := loadInstructions(sub)
		if err != nil {
			t.Fatalf("loadInstructions: %v", err)
		}
		if content != "repo rules" {
			t.Errorf("content = %q, want repo rules (git-root AGENTS.md checked before stopping)", content)
		}
	})
}

func TestLoadInstructionsMissing(t *testing.T) {
	dir := t.TempDir()
	// Bound the walk with a .git so it cannot escape to a real AGENTS.md.
	mkdirAll(t, filepath.Join(dir, ".git"))
	content, path, err := loadInstructions(dir)
	if err != nil {
		t.Fatalf("loadInstructions: %v", err)
	}
	if content != "" || path != "" {
		t.Errorf("missing file gave content=%q path=%q, want empty", content, path)
	}
}

func TestLoadInstructionsAgentMdFallback(t *testing.T) {
	t.Run("AGENT.md used when AGENTS.md absent", func(t *testing.T) {
		dir := t.TempDir()
		mkdirAll(t, filepath.Join(dir, ".git"))
		writeInstr(t, filepath.Join(dir, "AGENT.md"), "singular fallback")
		content, path, err := loadInstructions(dir)
		if err != nil {
			t.Fatalf("loadInstructions: %v", err)
		}
		if content != "singular fallback" {
			t.Errorf("content = %q, want singular fallback", content)
		}
		if path != "AGENT.md" {
			t.Errorf("path = %q, want AGENT.md", path)
		}
	})
	t.Run("AGENTS.md preferred when both exist", func(t *testing.T) {
		dir := t.TempDir()
		mkdirAll(t, filepath.Join(dir, ".git"))
		writeInstr(t, filepath.Join(dir, "AGENTS.md"), "plural wins")
		writeInstr(t, filepath.Join(dir, "AGENT.md"), "singular loses")
		content, path, err := loadInstructions(dir)
		if err != nil {
			t.Fatalf("loadInstructions: %v", err)
		}
		if content != "plural wins" || path != "AGENTS.md" {
			t.Errorf("content=%q path=%q, want plural wins / AGENTS.md", content, path)
		}
	})
}

func TestLoadInstructionsFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, ".git"))
	real := filepath.Join(dir, "real.md")
	writeInstr(t, real, "via symlink")
	if err := os.Symlink(real, filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	content, _, err := loadInstructions(dir)
	if err != nil {
		t.Fatalf("loadInstructions: %v", err)
	}
	if content != "via symlink" {
		t.Errorf("content = %q, want via symlink (ReadFile must follow symlinks)", content)
	}
}

func TestLoadInstructionsTruncatesAtCap(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("x", 70*1024)
	writeInstr(t, filepath.Join(dir, "AGENTS.md"), body)
	content, _, err := loadInstructions(dir)
	if err != nil {
		t.Fatalf("loadInstructions: %v", err)
	}
	if !strings.HasPrefix(content, strings.Repeat("x", 64*1024)) {
		t.Errorf("expected 64 KiB of body before the marker")
	}
	if !strings.HasSuffix(content, "\n"+truncationMarker) {
		t.Errorf("expected trailing truncation marker, got %d bytes", len(content))
	}
	capped := strings.TrimSuffix(content, "\n"+truncationMarker)
	if len(capped) != 64*1024 {
		t.Errorf("body not capped at 64 KiB: got %d bytes before marker", len(capped))
	}
}

func TestLoadInstructionsMalformed(t *testing.T) {
	t.Run("invalid UTF-8 errors", func(t *testing.T) {
		dir := t.TempDir()
		mkdirAll(t, filepath.Join(dir, ".git"))
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte{0xff, 0xfe, 0xfd}, 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := loadInstructions(dir)
		if err == nil {
			t.Fatal("expected error for invalid UTF-8")
		}
		if !strings.Contains(err.Error(), "AGENTS.md") || !strings.Contains(err.Error(), "UTF-8") {
			t.Errorf("error %q should name the path and the reason", err)
		}
	})
	t.Run("whitespace-only errors", func(t *testing.T) {
		dir := t.TempDir()
		mkdirAll(t, filepath.Join(dir, ".git"))
		writeInstr(t, filepath.Join(dir, "AGENTS.md"), "  \n\t  \n")
		_, _, err := loadInstructions(dir)
		if err == nil {
			t.Fatal("expected error for whitespace-only file")
		}
		if !strings.Contains(err.Error(), "AGENTS.md") {
			t.Errorf("error %q should name the path", err)
		}
	})
}

// instrSession builds a session over a scripted provider with a workDir and
// instructions config, runs one prompt, and returns the provider (whose
// captured requests hold the assembled system prompt).
func instrSession(t *testing.T, cfg Config, turns int) *scriptedProvider {
	t.Helper()
	var evs [][]provider.Event
	for i := 0; i < turns; i++ {
		evs = append(evs, asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}))
	}
	prov := &scriptedProvider{name: "test", turns: evs}
	cfg.Providers = provider.Registry{"test": prov}
	cfg.Model = message.ModelRef{Provider: "test", Model: "m1"}
	if cfg.System == nil {
		cfg.System = []string{"base"}
	}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	return prov
}

func TestInstructionsInjectedIntoSystem(t *testing.T) {
	dir := t.TempDir()
	writeInstr(t, filepath.Join(dir, "AGENTS.md"), "project says hi")
	prov := instrSession(t, Config{WorkDir: dir}, 1)
	sys := prov.requests[0].System
	if len(sys) != 2 {
		t.Fatalf("system = %v, want 2 segments", sys)
	}
	if sys[0] != "base" {
		t.Errorf("sys[0] = %q, want base", sys[0])
	}
	if !strings.HasPrefix(sys[1], "Project instructions from AGENTS.md:") {
		t.Errorf("sys[1] header = %q", sys[1])
	}
	if !strings.Contains(sys[1], "project says hi") {
		t.Errorf("sys[1] body = %q", sys[1])
	}
}

func TestInstructionsDisabled(t *testing.T) {
	dir := t.TempDir()
	writeInstr(t, filepath.Join(dir, "AGENTS.md"), "should be ignored")
	prov := instrSession(t, Config{WorkDir: dir, Instructions: &InstructionsConfig{Disabled: true}}, 1)
	sys := prov.requests[0].System
	if len(sys) != 1 || sys[0] != "base" {
		t.Errorf("system = %v, want only [base] when disabled", sys)
	}
}

func TestInstructionsMissingNoSegment(t *testing.T) {
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, ".git"))
	prov := instrSession(t, Config{WorkDir: dir}, 1)
	sys := prov.requests[0].System
	if len(sys) != 1 || sys[0] != "base" {
		t.Errorf("system = %v, want only [base] when no AGENTS.md", sys)
	}
}

func TestInstructionsPathOverride(t *testing.T) {
	dir := t.TempDir()
	writeInstr(t, filepath.Join(dir, "AGENTS.md"), "default file")
	override := filepath.Join(dir, "custom.md")
	writeInstr(t, override, "override rules")
	prov := instrSession(t, Config{WorkDir: dir, Instructions: &InstructionsConfig{Path: override}}, 1)
	sys := prov.requests[0].System
	if len(sys) != 2 {
		t.Fatalf("system = %v, want 2 segments", sys)
	}
	if !strings.Contains(sys[1], "override rules") {
		t.Errorf("sys[1] = %q, want override rules", sys[1])
	}
	if strings.Contains(sys[1], "default file") {
		t.Errorf("override ignored the discovered AGENTS.md: %q", sys[1])
	}
	if !strings.Contains(sys[1], override) {
		t.Errorf("sys[1] should name the override path %q: %q", override, sys[1])
	}
}

func TestInstructionsLoadedOncePerSession(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "AGENTS.md")
	writeInstr(t, p, "first content")
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "one"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "two"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		System:    []string{"base"},
		WorkDir:   dir,
	})
	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	// Mutating the file between prompts must not change the cached segment.
	writeInstr(t, p, "SECOND content changed entirely")
	if _, err := s.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	if len(prov.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(prov.requests))
	}
	seg0 := prov.requests[0].System[1]
	seg1 := prov.requests[1].System[1]
	if seg0 != seg1 {
		t.Errorf("segment changed between prompts:\n%q\n%q", seg0, seg1)
	}
	if !strings.Contains(seg1, "first content") {
		t.Errorf("expected cached first content, got %q", seg1)
	}
}

func TestInstructionsBeforeHookSegments(t *testing.T) {
	dir := t.TempDir()
	writeInstr(t, filepath.Join(dir, "AGENTS.md"), "instr body")
	hooks := &fakeHooks{segments: []string{"hook seg"}}
	prov := instrSession(t, Config{WorkDir: dir, Hooks: hooks}, 1)
	sys := prov.requests[0].System
	if len(sys) != 3 {
		t.Fatalf("system = %v, want [base, instructions, hook seg]", sys)
	}
	if sys[0] != "base" {
		t.Errorf("sys[0] = %q, want base", sys[0])
	}
	if !strings.Contains(sys[1], "instr body") {
		t.Errorf("sys[1] = %q, want instructions segment", sys[1])
	}
	if sys[2] != "hook seg" {
		t.Errorf("sys[2] = %q, want hook seg (hooks run after instructions)", sys[2])
	}
}

// TestInstructionsMalformedFailsFirstPrompt verifies the hard-failure
// contract: a present-but-unusable instructions file makes the first Prompt
// fail before any provider call or history mutation.
func TestInstructionsMalformedFailsFirstPrompt(t *testing.T) {
	run := func(t *testing.T, write func(dir string)) error {
		t.Helper()
		dir := t.TempDir()
		mkdirAll(t, filepath.Join(dir, ".git"))
		write(dir)
		prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
		}}
		s := NewSession(Config{
			Providers: provider.Registry{"test": prov},
			Model:     message.ModelRef{Provider: "test", Model: "m1"},
			System:    []string{"base"},
			WorkDir:   dir,
		})
		_, err := s.Prompt(context.Background(), "go")
		if err == nil {
			t.Fatal("expected first Prompt to fail on malformed instructions")
		}
		if len(prov.requests) != 0 {
			t.Errorf("provider called despite instructions failure: %d requests", len(prov.requests))
		}
		if len(s.History()) != 0 {
			t.Errorf("history mutated on failed prompt: %d messages", len(s.History()))
		}
		return err
	}

	t.Run("invalid UTF-8", func(t *testing.T) {
		err := run(t, func(dir string) {
			if werr := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte{0xff, 0xfe}, 0o644); werr != nil {
				t.Fatal(werr)
			}
		})
		if !strings.Contains(err.Error(), "AGENTS.md") {
			t.Errorf("error %q should name the path", err)
		}
	})
	t.Run("whitespace only", func(t *testing.T) {
		err := run(t, func(dir string) { writeInstr(t, filepath.Join(dir, "AGENTS.md"), "   \n\t") })
		if !strings.Contains(err.Error(), "AGENTS.md") {
			t.Errorf("error %q should name the path", err)
		}
	})
}
