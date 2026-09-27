package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCmdRepositoryPromptExpandsBeforeDelegation(t *testing.T) {
	bin := buildFakeClaudeForHarness(t)
	work, home, sessions := t.TempDir(), t.TempDir(), t.TempDir()
	t.Chdir(work)
	t.Setenv("HOME", home)
	t.Setenv("HARNESS_SESSION_DIR", sessions)
	writeDelegatedConfig(t, work, bin)
	path := filepath.Join(work, ".agents", "commands", "review.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: Review changes\n---\nReview $1 against $ARGUMENTS"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLAUDE_MODE", "normal")
	log := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", log)
	var runErr error
	captureStdout(t, func() { runErr = runCmd([]string{"-p", "/review HEAD~1"}) })
	if runErr != nil {
		t.Fatal(runErr)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"content":"Review HEAD~1 against HEAD~1"`) || strings.Contains(string(data), `"content":"/review HEAD~1"`) {
		t.Fatalf("delegated prompt = %q, want only expanded text", data)
	}
}
