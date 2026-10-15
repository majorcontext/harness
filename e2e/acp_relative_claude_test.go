package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The session directory differs from the directory of the process, so a relative binary_path is found only from the session directory.
func TestACPPromptChecksTheLoginOfARelativeClaudeProgramFromTheSessionDirectory(t *testing.T) {
	skipShort(t)
	t.Parallel()
	schema := loadACPSchema(t)
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	writeJSON(t, config, map[string]any{"model": "claude-code/sonnet", "providers": map[string]any{"claude-code": map[string]any{"type": "claude-code-cli", "binary_path": "./bin/claude"}}})
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fakeClaudePath(), filepath.Join(work, "bin", "claude")); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	if err := os.WriteFile(state, []byte("out"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(harnessBin, "acp")
	cmd.Dir = t.TempDir()
	cmd.Env = cleanEnv(map[string]string{"HARNESS_SESSION_DIR": t.TempDir(), "HARNESS_CONFIG": config, "ANTHROPIC_API_KEY": "", "PATH": t.TempDir(),
		"FAKE_CLAUDE_AUTH": state, "FAKE_CLAUDE_MODE": "logged_out"})
	in, out, g, stderr := startACPCmd(t, cmd)
	c := newACPConn(t, schema, in, out)
	c.dirs["main"] = work
	acpNew{as: "a"}.run(t, c)
	acpPrompt{as: "a", text: "go", code: acpAuthRequired, dataCode: "login_required"}.run(t, c)
	c.finish(func() { t.Helper(); waitACPExit(t, g, stderr) })
}
