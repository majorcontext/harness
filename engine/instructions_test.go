package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// loadInstructionChainDeepest drives the production loadInstructionChain
// entry point and returns the DEEPEST file's — the one nearest workDir —
// content and display path. It is the migration seam for suites written
// against the retired single-file loadInstructions/loadInstructionsMode: a
// test that only ever populated one file on the chain (its own workDir) sees
// the exact same content and path from loadInstructionChain, because that one
// file is both the chain's root and its deepest entry.
func loadInstructionChainDeepest(workDir string, maxBytes int, mode InstructionsMode) (content, path string, err error) {
	files, err := loadInstructionChain(workDir, maxBytes, mode)
	if err != nil {
		return "", "", err
	}
	if len(files) == 0 {
		return "", "", nil
	}
	deepest := files[len(files)-1]
	return deepest.body, deepest.path, nil
}

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
	content, path, err := loadInstructionChainDeepest(dir, defaultMaxInstructionsBytes, InstructionsModeAuto)
	if err != nil {
		t.Fatalf("loadInstructionChainDeepest: %v", err)
	}
	if content != "be terse" {
		t.Errorf("content = %q, want %q", content, "be terse")
	}
	if path != "AGENTS.md" {
		t.Errorf("path = %q, want AGENTS.md", path)
	}
}

// TestLoadInstructionsNoRepoRootOnlyWorkDirOwnFile pins the SHOULD-1 rule: with
// no .git anywhere in WorkDir's ancestry, the walk must NOT climb to the
// filesystem root looking for a first hit — it would inject an ancestor
// outside any repository (a $HOME AGENTS.md, or a stray file on a developer
// machine or box image) into every session rooted below it, and would cost
// every ENGINE test with WorkDir: t.TempDir() and no .git a stat/read at each
// ancestor up to /. Only WorkDir's own file counts in that case.
func TestLoadInstructionsNoRepoRootOnlyWorkDirOwnFile(t *testing.T) {
	t.Run("ancestor file is not injected", func(t *testing.T) {
		root := t.TempDir()
		writeInstr(t, filepath.Join(root, "AGENTS.md"), "root rules")
		sub := filepath.Join(root, "a", "b")
		mkdirAll(t, sub)
		content, path, err := loadInstructionChainDeepest(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
		if err != nil {
			t.Fatalf("loadInstructionChainDeepest: %v", err)
		}
		if content != "" || path != "" {
			t.Errorf("content=%q path=%q, want empty (no repo boundary, and WorkDir has no file of its own)", content, path)
		}
	})
	t.Run("WorkDir's own file is still injected", func(t *testing.T) {
		root := t.TempDir()
		writeInstr(t, filepath.Join(root, "AGENTS.md"), "root rules")
		sub := filepath.Join(root, "a", "b")
		mkdirAll(t, sub)
		writeInstr(t, filepath.Join(sub, "AGENTS.md"), "sub rules")
		content, path, err := loadInstructionChainDeepest(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
		if err != nil {
			t.Fatalf("loadInstructionChainDeepest: %v", err)
		}
		if content != "sub rules" {
			t.Errorf("content = %q, want sub rules (WorkDir's own file, not root's, with no repo boundary)", content)
		}
		if path != "AGENTS.md" {
			t.Errorf("path = %q, want AGENTS.md", path)
		}
	})
}

func TestLoadInstructionsGitRoot(t *testing.T) {
	t.Run("stops above git root", func(t *testing.T) {
		outer := t.TempDir()
		writeInstr(t, filepath.Join(outer, "AGENTS.md"), "outer rules")
		repo := filepath.Join(outer, "repo")
		mkdirAll(t, filepath.Join(repo, ".git"))
		sub := filepath.Join(repo, "pkg")
		mkdirAll(t, sub)
		content, path, err := loadInstructionChainDeepest(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
		if err != nil {
			t.Fatalf("loadInstructionChainDeepest: %v", err)
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
		content, _, err := loadInstructionChainDeepest(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
		if err != nil {
			t.Fatalf("loadInstructionChainDeepest: %v", err)
		}
		if content != "repo rules" {
			t.Errorf("content = %q, want repo rules (git-root AGENTS.md checked before stopping)", content)
		}
	})
	t.Run("git as a file stops the walk (worktree or submodule)", func(t *testing.T) {
		// BLOCKING-1: a git worktree or submodule checkout uses a .git FILE
		// ("gitdir: ..."), not a directory. isDir(join(dir, ".git")) is false
		// for a file, so a check that only recognizes a directory walks past
		// the worktree root and injects whatever AGENTS.md lies above it. This
		// must be asserted against the CHAIN (every file found), not just the
		// deepest file's own content: the deepest file (repo's) is unaffected
		// either way, and the bug's only symptom is an EXTRA file the chain
		// should never have reached.
		outer := t.TempDir()
		writeInstr(t, filepath.Join(outer, "AGENTS.md"), "outer stranger rules")
		repo := filepath.Join(outer, "repo")
		mkdirAll(t, repo)
		writeInstr(t, filepath.Join(repo, ".git"), "gitdir: /elsewhere/.git/worktrees/repo\n")
		writeInstr(t, filepath.Join(repo, "AGENTS.md"), "worktree rules")
		sub := filepath.Join(repo, "pkg")
		mkdirAll(t, sub)
		files, err := loadInstructionChain(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
		if err != nil {
			t.Fatalf("loadInstructionChain: %v", err)
		}
		if len(files) != 1 || files[0].body != "worktree rules" {
			t.Errorf("files = %+v, want exactly one file (worktree rules); the walk must stop at the .git FILE, not climb past it", files)
		}
	})
}

func TestLoadInstructionsMissing(t *testing.T) {
	dir := t.TempDir()
	// Bound the walk with a .git so it cannot escape to a real AGENTS.md.
	mkdirAll(t, filepath.Join(dir, ".git"))
	content, path, err := loadInstructionChainDeepest(dir, defaultMaxInstructionsBytes, InstructionsModeAuto)
	if err != nil {
		t.Fatalf("loadInstructionChainDeepest: %v", err)
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
		content, path, err := loadInstructionChainDeepest(dir, defaultMaxInstructionsBytes, InstructionsModeAuto)
		if err != nil {
			t.Fatalf("loadInstructionChainDeepest: %v", err)
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
		content, path, err := loadInstructionChainDeepest(dir, defaultMaxInstructionsBytes, InstructionsModeAuto)
		if err != nil {
			t.Fatalf("loadInstructionChainDeepest: %v", err)
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
	content, _, err := loadInstructionChainDeepest(dir, defaultMaxInstructionsBytes, InstructionsModeAuto)
	if err != nil {
		t.Fatalf("loadInstructionChainDeepest: %v", err)
	}
	if content != "via symlink" {
		t.Errorf("content = %q, want via symlink (ReadFile must follow symlinks)", content)
	}
}

func TestLoadInstructionsMalformed(t *testing.T) {
	t.Run("invalid UTF-8 errors", func(t *testing.T) {
		dir := t.TempDir()
		mkdirAll(t, filepath.Join(dir, ".git"))
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte{0xff, 0xfe, 0xfd}, 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := loadInstructionChainDeepest(dir, defaultMaxInstructionsBytes, InstructionsModeAuto)
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
		_, _, err := loadInstructionChainDeepest(dir, defaultMaxInstructionsBytes, InstructionsModeAuto)
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
	if len(sys) != 3 {
		t.Fatalf("system = %v, want 3 segments", sys)
	}
	if sys[0] != "base" {
		t.Errorf("sys[0] = %q, want base", sys[0])
	}
	if !isBatchingSegment(sys[1]) {
		t.Errorf("sys[1] = %q, want the tool-batching segment", sys[1])
	}
	if !strings.HasPrefix(sys[2], "Project instructions from AGENTS.md:") {
		t.Errorf("sys[2] header = %q", sys[2])
	}
	if !strings.Contains(sys[2], "project says hi") {
		t.Errorf("sys[2] body = %q", sys[2])
	}
}

// TestInstructionsInjectsEveryFileRootToWorkDir pins the AGENTS.md
// multi-file precedence gap: the retired single-file walk-up-from-WorkDir
// search stopped at the FIRST AGENTS.md it found, so a workDir several
// directories below the repo root never saw the root file at all. With a
// fixture tree root/AGENTS.md and root/sub/AGENTS.md and WorkDir=root/sub,
// the injected segment must carry BOTH files' content, root's before sub's
// (root to working directory; the deepest file wins on conflict) — before
// this fix it carried only "sub rules".
func TestInstructionsInjectsEveryFileRootToWorkDir(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, ".git"))
	writeInstr(t, filepath.Join(root, "AGENTS.md"), "root rules")
	sub := filepath.Join(root, "sub")
	mkdirAll(t, sub)
	writeInstr(t, filepath.Join(sub, "AGENTS.md"), "sub rules")

	prov := instrSession(t, Config{WorkDir: sub}, 1)
	sys := prov.requests[0].System
	if len(sys) != 3 {
		t.Fatalf("system = %v, want 3 segments", sys)
	}
	seg := sys[2]
	if !strings.Contains(seg, "root rules") {
		t.Errorf("segment missing root AGENTS.md content: %q", seg)
	}
	if !strings.Contains(seg, "sub rules") {
		t.Errorf("segment missing sub AGENTS.md content: %q", seg)
	}
	if ir, is := strings.Index(seg, "root rules"), strings.Index(seg, "sub rules"); ir < 0 || is < 0 || ir > is {
		t.Errorf("segment must inject the root file before the sub file: %q", seg)
	}
	if !strings.Contains(seg, "deepest file wins") {
		t.Errorf("segment should state precedence when it carries more than one file: %q", seg)
	}
}

// TestInstructionsChainMalformedAncestorIsSkipped pins BLOCKING-2: a malformed
// (empty/whitespace-only or invalid-UTF-8) file above the file NEAREST
// WorkDir is skipped with a logged warning naming its path, and the chain
// still injects the nearest file — an unrelated ancestor's broken file must
// not fail every session rooted below it, which is what happened when
// loadInstructionChain validated every file and returned the first error: a
// whitespace-only root AGENTS.md broke every request anywhere in the repo,
// though the single-file walk it replaced only ever broke a session started
// in that same directory.
func TestInstructionsChainMalformedAncestorIsSkipped(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, ".git"))
	writeInstr(t, filepath.Join(root, "AGENTS.md"), "   \n\t  \n") // malformed: whitespace-only
	sub := filepath.Join(root, "sub")
	mkdirAll(t, sub)
	writeInstr(t, filepath.Join(sub, "AGENTS.md"), "sub rules")

	buf := captureLogs(t)
	files, err := loadInstructionChain(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
	if err != nil {
		t.Fatalf("loadInstructionChain: %v (a malformed ANCESTOR must not fail the chain)", err)
	}
	if len(files) != 1 || files[0].body != "sub rules" {
		t.Fatalf("files = %+v, want exactly the nearest file (sub rules)", files)
	}
	rootPath := filepath.Join(root, "AGENTS.md")
	if out := buf.String(); !strings.Contains(out, "WARN") || !strings.Contains(out, rootPath) {
		t.Errorf("expected a WARN log line naming %s, got:\n%s", rootPath, out)
	}
}

// TestInstructionsChainMalformedNearestStillFails is the BLOCKING-2 mirror:
// a malformed file in the directory NEAREST WorkDir — the file the retired
// single-file loader would have found and failed on — still fails the whole
// chain, so this keeps that loader's existing hard-failure contract for the
// one file whose provenance a session controls directly.
func TestInstructionsChainMalformedNearestStillFails(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, ".git"))
	writeInstr(t, filepath.Join(root, "AGENTS.md"), "root rules")
	sub := filepath.Join(root, "sub")
	mkdirAll(t, sub)
	writeInstr(t, filepath.Join(sub, "AGENTS.md"), "   \n\t  \n") // malformed: whitespace-only

	_, err := loadInstructionChain(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
	if err == nil {
		t.Fatal("expected the nearest file's malformed content to fail the chain")
	}
	if !strings.Contains(err.Error(), filepath.Join(sub, "AGENTS.md")) {
		t.Errorf("error %q should name the nearest file's path", err)
	}
}

// TestInstructionsChainMalformedNearestFailsFirstPrompt drives the same
// nearest-file failure through a real session, so the hard-failure contract
// TestInstructionsMalformedFailsFirstPrompt already pins for a single file
// also holds through the chain loader: no provider call, no history mutation.
func TestInstructionsChainMalformedNearestFailsFirstPrompt(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, ".git"))
	writeInstr(t, filepath.Join(root, "AGENTS.md"), "root rules")
	sub := filepath.Join(root, "sub")
	mkdirAll(t, sub)
	writeInstr(t, filepath.Join(sub, "AGENTS.md"), "   \n\t  \n")

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		System:    []string{"base"},
		WorkDir:   sub,
	})
	if _, err := s.Prompt(context.Background(), "go"); err == nil {
		t.Fatal("expected first Prompt to fail on the nearest file's malformed content")
	}
	if len(prov.requests) != 0 {
		t.Errorf("provider called despite instructions failure: %d requests", len(prov.requests))
	}
	if len(s.History()) != 0 {
		t.Errorf("history mutated on failed prompt: %d messages", len(s.History()))
	}
}

// TestInstructionsChainByteCeilingDropsMiddleFiles pins SHOULD-2: a per-file
// cap alone does not bound the CHAIN, so a deep monorepo path could put
// N*MaxBytes bytes into every request's system prompt. capChainTotal makes a
// best-effort pass toward chainCeilingMultiplier*maxBytes by dropping middle
// files — never the root, which carries the routing table, and never the
// deepest, which names WorkDir's own rules. With 7 same-size files and a
// ceiling of 4*maxBytes, exactly 3 middle files must be dropped, leaving 4.
func TestInstructionsChainByteCeilingDropsMiddleFiles(t *testing.T) {
	const maxBytes = 100
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, ".git"))
	writeInstr(t, filepath.Join(root, "AGENTS.md"), strings.Repeat("r", maxBytes))
	dir := root
	var levels []string
	for i := 0; i < 6; i++ {
		dir = filepath.Join(dir, fmt.Sprintf("lvl%d", i))
		mkdirAll(t, dir)
		writeInstr(t, filepath.Join(dir, "AGENTS.md"), strings.Repeat("m", maxBytes))
		levels = append(levels, dir)
	}
	workDir := levels[len(levels)-1]
	// Overwrite the deepest file's body so it is distinguishable from a
	// dropped middle file.
	writeInstr(t, filepath.Join(workDir, "AGENTS.md"), strings.Repeat("d", maxBytes))

	buf := captureLogs(t)
	files, err := loadInstructionChain(workDir, maxBytes, InstructionsModeAuto)
	if err != nil {
		t.Fatalf("loadInstructionChain: %v", err)
	}
	total := 0
	for _, f := range files {
		total += len(f.body)
	}
	ceiling := maxBytes * chainCeilingMultiplier
	if total > ceiling {
		t.Errorf("chain total = %d bytes, want at most the ceiling %d", total, ceiling)
	}
	if files[0].body != strings.Repeat("r", maxBytes) {
		t.Errorf("root file was dropped; want it always kept")
	}
	if last := files[len(files)-1]; last.body != strings.Repeat("d", maxBytes) {
		t.Errorf("deepest file was dropped; want it always kept")
	}
	const wantFiles = 4 // root + deepest + 2 of the 5 middle files (3 dropped)
	if len(files) != wantFiles {
		t.Errorf("files = %d, want %d (middle files dropped to fit the ceiling)", len(files), wantFiles)
	}
	if out := buf.String(); !strings.Contains(out, "WARN") || !strings.Contains(out, "ceiling") {
		t.Errorf("expected a WARN log line naming the ceiling, got:\n%s", out)
	}
}

// assertChainBodies checks files' bodies, in order, against want (root to
// WorkDir), so a table case names the CONTENT it expects rather than a byte
// offset or a path.
func assertChainBodies(t *testing.T, files []instructionFile, want ...string) {
	t.Helper()
	got := make([]string, len(files))
	for i, f := range files {
		got[i] = f.body
	}
	if len(got) != len(want) {
		t.Fatalf("bodies = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bodies[%d] = %q, want %q (got=%v want=%v)", i, got[i], want[i], got, want)
		}
	}
}

// TestInstructionChainTable is the SHOULD-4 table test: one fixture per
// boundary or precedence case loadInstructionChain must get right, driven
// directly against the production entry point.
func TestInstructionChainTable(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: ".git directory bounds the walk",
			run: func(t *testing.T) {
				root := t.TempDir()
				mkdirAll(t, filepath.Join(root, ".git"))
				writeInstr(t, filepath.Join(root, "AGENTS.md"), "root")
				sub := filepath.Join(root, "sub")
				mkdirAll(t, sub)
				writeInstr(t, filepath.Join(sub, "AGENTS.md"), "sub")
				files, err := loadInstructionChain(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
				if err != nil {
					t.Fatalf("loadInstructionChain: %v", err)
				}
				assertChainBodies(t, files, "root", "sub")
			},
		},
		{
			name: ".git FILE bounds the walk (worktree or submodule)",
			run: func(t *testing.T) {
				outer := t.TempDir()
				writeInstr(t, filepath.Join(outer, "AGENTS.md"), "outer")
				repo := filepath.Join(outer, "repo")
				mkdirAll(t, repo)
				writeInstr(t, filepath.Join(repo, ".git"), "gitdir: /elsewhere/.git/worktrees/repo\n")
				writeInstr(t, filepath.Join(repo, "AGENTS.md"), "repo")
				sub := filepath.Join(repo, "sub")
				mkdirAll(t, sub)
				writeInstr(t, filepath.Join(sub, "AGENTS.md"), "sub")
				files, err := loadInstructionChain(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
				if err != nil {
					t.Fatalf("loadInstructionChain: %v", err)
				}
				assertChainBodies(t, files, "repo", "sub")
			},
		},
		{
			name: "no .git anywhere: only WorkDir's own file",
			run: func(t *testing.T) {
				root := t.TempDir()
				writeInstr(t, filepath.Join(root, "AGENTS.md"), "root")
				sub := filepath.Join(root, "sub")
				mkdirAll(t, sub)
				writeInstr(t, filepath.Join(sub, "AGENTS.md"), "sub")
				files, err := loadInstructionChain(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
				if err != nil {
					t.Fatalf("loadInstructionChain: %v", err)
				}
				assertChainBodies(t, files, "sub")
			},
		},
		{
			name: "three levels",
			run: func(t *testing.T) {
				root := t.TempDir()
				mkdirAll(t, filepath.Join(root, ".git"))
				writeInstr(t, filepath.Join(root, "AGENTS.md"), "root")
				mid := filepath.Join(root, "mid")
				mkdirAll(t, mid)
				writeInstr(t, filepath.Join(mid, "AGENTS.md"), "mid")
				leaf := filepath.Join(mid, "leaf")
				mkdirAll(t, leaf)
				writeInstr(t, filepath.Join(leaf, "AGENTS.md"), "leaf")
				files, err := loadInstructionChain(leaf, defaultMaxInstructionsBytes, InstructionsModeAuto)
				if err != nil {
					t.Fatalf("loadInstructionChain: %v", err)
				}
				assertChainBodies(t, files, "root", "mid", "leaf")
			},
		},
		{
			name: "a gap directory (neither file) contributes nothing",
			run: func(t *testing.T) {
				root := t.TempDir()
				mkdirAll(t, filepath.Join(root, ".git"))
				writeInstr(t, filepath.Join(root, "AGENTS.md"), "root")
				mid := filepath.Join(root, "mid") // no instructions file here
				mkdirAll(t, mid)
				leaf := filepath.Join(mid, "leaf")
				mkdirAll(t, leaf)
				writeInstr(t, filepath.Join(leaf, "AGENTS.md"), "leaf")
				files, err := loadInstructionChain(leaf, defaultMaxInstructionsBytes, InstructionsModeAuto)
				if err != nil {
					t.Fatalf("loadInstructionChain: %v", err)
				}
				assertChainBodies(t, files, "root", "leaf")
			},
		},
		{
			name: "a malformed ancestor is skipped; the nearest file still injects",
			run: func(t *testing.T) {
				root := t.TempDir()
				mkdirAll(t, filepath.Join(root, ".git"))
				writeInstr(t, filepath.Join(root, "AGENTS.md"), "   \n\t  \n") // malformed
				sub := filepath.Join(root, "sub")
				mkdirAll(t, sub)
				writeInstr(t, filepath.Join(sub, "AGENTS.md"), "sub")
				files, err := loadInstructionChain(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
				if err != nil {
					t.Fatalf("loadInstructionChain: %v", err)
				}
				assertChainBodies(t, files, "sub")
			},
		},
		{
			name: "AGENT.md and AGENTS.md mixed across levels",
			run: func(t *testing.T) {
				root := t.TempDir()
				mkdirAll(t, filepath.Join(root, ".git"))
				writeInstr(t, filepath.Join(root, "AGENTS.md"), "root plural")
				sub := filepath.Join(root, "sub")
				mkdirAll(t, sub)
				writeInstr(t, filepath.Join(sub, "AGENT.md"), "sub singular")
				files, err := loadInstructionChain(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
				if err != nil {
					t.Fatalf("loadInstructionChain: %v", err)
				}
				assertChainBodies(t, files, "root plural", "sub singular")
			},
		},
		{
			name: "both names in one directory: AGENTS.md wins, one file",
			run: func(t *testing.T) {
				root := t.TempDir()
				mkdirAll(t, filepath.Join(root, ".git"))
				writeInstr(t, filepath.Join(root, "AGENTS.md"), "root")
				sub := filepath.Join(root, "sub")
				mkdirAll(t, sub)
				writeInstr(t, filepath.Join(sub, "AGENTS.md"), "plural")
				writeInstr(t, filepath.Join(sub, "AGENT.md"), "singular")
				files, err := loadInstructionChain(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
				if err != nil {
					t.Fatalf("loadInstructionChain: %v", err)
				}
				assertChainBodies(t, files, "root", "plural")
			},
		},
		{
			name: "root boundary: an inner repo's .git wins over an outer one",
			run: func(t *testing.T) {
				outer := t.TempDir()
				mkdirAll(t, filepath.Join(outer, ".git"))
				writeInstr(t, filepath.Join(outer, "AGENTS.md"), "outer root")
				inner := filepath.Join(outer, "inner")
				mkdirAll(t, filepath.Join(inner, ".git"))
				writeInstr(t, filepath.Join(inner, "AGENTS.md"), "inner root")
				sub := filepath.Join(inner, "sub")
				mkdirAll(t, sub)
				writeInstr(t, filepath.Join(sub, "AGENTS.md"), "inner sub")
				files, err := loadInstructionChain(sub, defaultMaxInstructionsBytes, InstructionsModeAuto)
				if err != nil {
					t.Fatalf("loadInstructionChain: %v", err)
				}
				assertChainBodies(t, files, "inner root", "inner sub")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, tc.run)
	}
}

// TestSessionInfoReportsCommaJoinedInstructionChain pins the SHOULD-4 table
// case for session_info's provenance field: with more than one AGENTS.md
// injected, Instructions reports every display path, comma-joined, root to
// WorkDir — not the single path it reported before this chain existed.
func TestSessionInfoReportsCommaJoinedInstructionChain(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, ".git"))
	writeInstr(t, filepath.Join(root, "AGENTS.md"), "root rules")
	sub := filepath.Join(root, "sub")
	mkdirAll(t, sub)
	writeInstr(t, filepath.Join(sub, "AGENTS.md"), "sub rules")

	info, _ := callSessionInfo(t, Config{WorkDir: sub})
	want := strings.Join([]string{filepath.Join("..", "AGENTS.md"), "AGENTS.md"}, ", ")
	if info.Instructions != want {
		t.Errorf("instructions = %q, want %q", info.Instructions, want)
	}
}

func TestInstructionsDisabled(t *testing.T) {
	dir := t.TempDir()
	writeInstr(t, filepath.Join(dir, "AGENTS.md"), "should be ignored")
	prov := instrSession(t, Config{WorkDir: dir, Instructions: &InstructionsConfig{Disabled: true}}, 1)
	sys := prov.requests[0].System
	if len(sys) != 2 || sys[0] != "base" || !isBatchingSegment(sys[1]) {
		t.Errorf("system = %v, want [base, tool-batching] when instructions are disabled", sys)
	}
}

func TestInstructionsMissingNoSegment(t *testing.T) {
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, ".git"))
	prov := instrSession(t, Config{WorkDir: dir}, 1)
	sys := prov.requests[0].System
	if len(sys) != 2 || sys[0] != "base" || !isBatchingSegment(sys[1]) {
		t.Errorf("system = %v, want [base, tool-batching] when there is no AGENTS.md", sys)
	}
}

func TestInstructionsPathOverride(t *testing.T) {
	dir := t.TempDir()
	writeInstr(t, filepath.Join(dir, "AGENTS.md"), "default file")
	override := filepath.Join(dir, "custom.md")
	writeInstr(t, override, "override rules")
	prov := instrSession(t, Config{WorkDir: dir, Instructions: &InstructionsConfig{Path: override}}, 1)
	sys := prov.requests[0].System
	if len(sys) != 3 {
		t.Fatalf("system = %v, want 3 segments", sys)
	}
	if !strings.Contains(sys[2], "override rules") {
		t.Errorf("sys[2] = %q, want override rules", sys[2])
	}
	if strings.Contains(sys[2], "default file") {
		t.Errorf("override ignored the discovered AGENTS.md: %q", sys[2])
	}
	if !strings.Contains(sys[2], override) {
		t.Errorf("sys[2] should name the override path %q: %q", override, sys[2])
	}
}

func TestInstructionsPathOverrideRelative(t *testing.T) {
	// A relative override resolves against WorkDir, not the process cwd
	// (review observation on #19).
	dir := t.TempDir()
	writeInstr(t, filepath.Join(dir, "custom.md"), "relative override rules")
	prov := instrSession(t, Config{WorkDir: dir, Instructions: &InstructionsConfig{Path: "custom.md"}}, 1)
	sys := prov.requests[0].System
	if len(sys) != 3 || !strings.Contains(sys[2], "relative override rules") {
		t.Fatalf("system = %v, want relative override injected", sys)
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
	seg0 := prov.requests[0].System[2]
	seg1 := prov.requests[1].System[2]
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
	if len(sys) != 4 {
		t.Fatalf("system = %v, want [base, tool-batching, instructions, hook seg]", sys)
	}
	if sys[0] != "base" {
		t.Errorf("sys[0] = %q, want base", sys[0])
	}
	if !isBatchingSegment(sys[1]) {
		t.Errorf("sys[1] = %q, want the tool-batching segment", sys[1])
	}
	if !strings.Contains(sys[2], "instr body") {
		t.Errorf("sys[2] = %q, want instructions segment", sys[2])
	}
	if sys[3] != "hook seg" {
		t.Errorf("sys[3] = %q, want hook seg (hooks run after instructions)", sys[3])
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
