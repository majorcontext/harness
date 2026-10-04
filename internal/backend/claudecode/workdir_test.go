package claudecode_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
)

func TestClaudeCodeRunsInTheWorkDir(t *testing.T) {
	fakeClaude(t, "thinking")
	cwdLog := filepath.Join(t.TempDir(), "cwd")
	t.Setenv("FAKE_CLAUDE_CWD_LOG", cwdLog)
	bin, err := fakeClaudeBin()
	if err != nil {
		t.Fatal(err)
	}
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), WorkDir: work, Config: config.Config{
		Providers: map[string]config.Provider{"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: bin}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntime(t, r)
	turnOf(t, createClaude(t, r, nil), text("a", "hi"))
	got, _ := os.ReadFile(cwdLog)
	if strings.TrimSpace(string(got)) != work {
		t.Errorf("CLI working directory = %q, want %q", got, work)
	}
}
