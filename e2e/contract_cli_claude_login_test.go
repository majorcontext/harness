package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func claudeHost(t *testing.T, mode, auth string) *cliHost {
	t.Helper()
	h := newCLIHost(t, nil)
	writeJSON(t, h.config, map[string]any{"model": "claude-code/sonnet", "providers": map[string]any{"claude-code": map[string]any{"type": "claude-code-cli", "binary_path": fakeClaudePath()}}})
	state := filepath.Join(h.dir, "state")
	h.env = map[string]string{"FAKE_CLAUDE_AUTH": state, "FAKE_CLAUDE_MODE": mode}
	if err := os.WriteFile(state, []byte(auth), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestContractCLIRunNamesTheLoginWhenClaudeCodeIsLoggedOut(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h := claudeHost(t, "logged_out", "out")
	out, errOut, code := h.run("run", "-json", "-p", "hi")
	if code == 0 || !strings.Contains(out+errOut, "harness login claude-code") || !strings.Contains(out, "login_required") {
		t.Errorf("run = %d\n%s%s\nwant a failure with login_required that names harness login claude-code", code, out, errOut)
	}
}

func TestContractCLIRunKeepsTheClaudeErrorWhenTheCLIIsLoggedIn(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h := claudeHost(t, "credential_error", "in")
	out, errOut, code := h.run("run", "-json", "-p", "hi")
	if code == 0 || strings.Contains(out+errOut, "login_required") {
		t.Errorf("run = %d\n%s%s\nwant a failure that is not login_required", code, out, errOut)
	}
}
