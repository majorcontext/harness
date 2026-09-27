package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/engine"
)

func writeReviewCommand(t *testing.T, work, text string) {
	t.Helper()
	path := filepath.Join(work, ".agents", "commands", "review.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: Review changes\n---\n"+text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunCmdRepositoryPromptExpandsBeforeDelegation(t *testing.T) {
	bin := buildFakeClaudeForHarness(t)
	work, home, sessions := t.TempDir(), t.TempDir(), t.TempDir()
	t.Chdir(work)
	t.Setenv("HOME", home)
	t.Setenv("HARNESS_SESSION_DIR", sessions)
	writeDelegatedConfig(t, work, bin)
	writeReviewCommand(t, work, "Review $1 against $ARGUMENTS")
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

func TestRunCmdResumedRepositoryCommandUsesSessionWorkDir(t *testing.T) {
	bin := buildFakeClaudeForHarness(t)
	first, second, home, sessions := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	t.Chdir(first)
	t.Setenv("HOME", home)
	t.Setenv("HARNESS_SESSION_DIR", sessions)
	writeDelegatedConfig(t, first, bin)
	writeReviewCommand(t, first, "FIRST $1")
	writeReviewCommand(t, second, "SECOND $1")
	t.Setenv("FAKE_CLAUDE_MODE", "normal")
	log := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", log)
	captureStdout(t, func() {
		if err := runCmd([]string{"-p", "hello"}); err != nil {
			t.Fatal(err)
		}
	})
	infos, err := engine.ListSessions(sessions)
	if err != nil || len(infos) != 1 {
		t.Fatalf("sessions: %+v %v", infos, err)
	}
	t.Chdir(second)
	captureStdout(t, func() {
		if err := runCmd([]string{"-p", "/review HEAD", "-resume", infos[0].ID}); err != nil {
			t.Fatal(err)
		}
	})
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"content":"FIRST HEAD"`) || strings.Contains(string(data), `"content":"SECOND HEAD"`) {
		t.Fatalf("resumed prompt = %q, want command from first workdir", data)
	}
}
