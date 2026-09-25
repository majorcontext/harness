package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// Prompt-cache stability of the CLI-visible system prompt.
//
// The delegated lane cannot suffer the native lane's ambient-boundary
// problem (see markAmbientBoundary in provider/anthropic): harness sends the
// CLI only the newest user message and resumes the child's own session, so
// it never re-renders an earlier message and cannot change bytes the CLI has
// already cached. What it CAN change is the system prompt it passes on every
// turn, and Claude Code's own cached prefix starts there — a per-turn
// difference in --append-system-prompt re-processes that session's whole
// conversation uncached, silently, with no error and only a larger bill.
//
// claudeCodeCacheTestSession mirrors claudeCodeTestSession with operator
// segments configured, the shape a box ships (images/shared/box-runtime.sh
// writes Config.AppendSystemPrompt into harness.json).
func claudeCodeCacheTestSession(t *testing.T, segments []string) (*Session, string) {
	t.Helper()
	bin := buildFakeClaude(t)
	t.Setenv("FAKE_CLAUDE_MODE", "normal")
	logPath := filepath.Join(t.TempDir(), "invocations.jsonl")
	t.Setenv("FAKE_CLAUDE_LOG", logPath)
	s := NewSession(Config{
		SessionDir:         t.TempDir(),
		Model:              message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
		ClaudeCode:         ClaudeCodeConfig{BinaryPath: bin},
		AppendSystemPrompt: segments,
	})
	return s, logPath
}

// argvValuesAfter returns every value following flag in argv — the flag can
// legitimately repeat (operator segments plus the history directive).
func argvValuesAfter(argv []string, flag string) []string {
	var out []string
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			out = append(out, argv[i+1])
		}
	}
	return out
}

// TestClaudeCodeAppendSystemPromptStableAcrossTurns: two consecutive
// delegated turns pass byte-identical --append-system-prompt text, so the
// CLI's cached prefix survives the turn boundary. Both turns here are
// directive-free by construction (a first-ever message, then a consecutive
// caught-up turn — see claudeCodeHistoryDirectiveArgs), which isolates the
// operator segments as the only contributor.
func TestClaudeCodeAppendSystemPromptStableAcrossTurns(t *testing.T) {
	segments := []string{
		"Lifecycle: this box has durable disk and disposable compute.",
		"Preview URLs: a server you run in this box is reachable at https://example.invalid-{port}.",
	}
	s, logPath := claudeCodeCacheTestSession(t, segments)

	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatalf("Prompt (turn 1): %v", err)
	}
	if _, err := s.Prompt(context.Background(), "second"); err != nil {
		t.Fatalf("Prompt (turn 2): %v", err)
	}

	invocations := readInvocations(t, logPath)
	if len(invocations) != 2 {
		t.Fatalf("invocations = %d, want 2: %+v", len(invocations), invocations)
	}

	want := strings.Join(segments, "\n\n")
	first := argvValuesAfter(invocations[0], "--append-system-prompt")
	second := argvValuesAfter(invocations[1], "--append-system-prompt")

	if len(first) != 1 || first[0] != want {
		t.Errorf("turn 1 --append-system-prompt = %q, want exactly one value %q", first, want)
	}
	if len(second) != len(first) {
		t.Fatalf("--append-system-prompt count changed between turns: %d then %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("--append-system-prompt[%d] changed between turns:\nturn 1: %q\nturn 2: %q", i, first[i], second[i])
		}
	}
}
