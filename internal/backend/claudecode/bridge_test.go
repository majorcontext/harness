package claudecode_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// bridgeRuntime serves claude-code through fakeclaude and anthropic through
// a scripted model server.
func bridgeRuntime(t *testing.T, native *harnesstest.Server) *harness.Runtime {
	t.Helper()
	bin, err := fakeClaudeBin()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_TEST_KEY", "k")
	retries := 0
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: config.Config{PromptRetries: &retries, Providers: map[string]config.Provider{
		"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: bin},
		"anthropic":   {APIKeyEnv: "HARNESS_TEST_KEY", BaseURL: native.URL()}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	return r
}

func switchTo(t *testing.T, s *harness.Session, model string) {
	t.Helper()
	if _, err := s.Update(bg, protocol.SettingsPatch{Model: &model}); err != nil {
		t.Fatalf("Update(%s): %v", model, err)
	}
}

const priorHistory = "The following is PRIOR conversation history for this session that already happened. It is context for you to read, not a new message to respond to.\n\n" +
	"Showing messages 1-12 of 12 total (oldest first).\n\n" +
	"User: run it\nAssistant: Let me check that.\nAssistant called tool Bash({\"command\":\"echo hi\"})\nTool result (Bash, ok): hi\n\nAssistant: Done — it printed hi.\n" +
	"User: native\nAssistant: native reply\nUser: back\nAssistant: Let me check that.\nAssistant called tool Bash({\"command\":\"echo hi\"})\nTool result (Bash, ok): hi\n\nAssistant: Done — it printed hi.\n"

func TestClaudeCodeReadsTheHistoryOfAnotherModel(t *testing.T) {
	native := harnesstest.New(t, harnesstest.Step{Name: "native", Match: harnesstest.LastUserText("native"), Reply: harnesstest.Reply{Text: "native reply"}})
	argvLog := fakeClaude(t, "", "FAKE_CLAUDE_CALL_TOOL", "get_conversation_history", "FAKE_CLAUDE_TOOL_LOG", filepath.Join(t.TempDir(), "tool"))
	r := bridgeRuntime(t, native)
	s := createClaude(t, r, nil)
	turnOf(t, s, text("a", "run it"))
	switchTo(t, s, "anthropic/claude-fable-5")
	turnOf(t, s, text("b", "native"))
	switchTo(t, s, "claude-code/sonnet")
	turnOf(t, s, text("c", "back"))
	turnOf(t, s, text("d", "again"))
	directive := func(argv []string) bool {
		for i, a := range argv {
			if a == "--append-system-prompt" && strings.Contains(argv[i+1], "get_conversation_history") {
				return true
			}
		}
		return false
	}
	argv := jsonLines[[]string](t, argvLog)
	if len(argv) != 3 {
		t.Fatalf("CLI runs = %d, want 3", len(argv))
	}
	for i, want := range []bool{false, true, false} {
		if got := directive(argv[i]); got != want {
			t.Errorf("run %d names the history tool in its system prompt = %t, want %t", i+1, got, want)
		}
	}
	calls := jsonLines[struct {
		Result struct{ Content []struct{ Text string } }
	}](t, os.Getenv("FAKE_CLAUDE_TOOL_LOG"))
	if len(calls) != 3 {
		t.Fatalf("history tool calls = %d, want one at the end of each run", len(calls))
	}
	if got := calls[1].Result.Content[0].Text; got != priorHistory {
		t.Errorf("history after the second run =\n%s\nwant\n%s", got, priorHistory)
	}
}
