package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestACPSessionNewReportsTheSubscriptionDefaultAsTheCurrentModel(t *testing.T) {
	skipShort(t)
	t.Parallel()
	schema := loadACPSchema(t)
	rows := []struct {
		name   string
		claude bool
		login  bool
		key    string
		want   string
	}{
		{"claude on PATH and no API key", true, false, "", "claude-code/sonnet"},
		{"codex login file and claude on PATH", true, true, "", "codex/gpt-6.1-sol"},
		{"API key keeps the built-in default", true, true, "sk-test", "anthropic/claude-fable-5"},
		{"no subscription", false, false, "", ""},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			config := filepath.Join(dir, "config.json")
			if err := os.WriteFile(config, []byte(`{"context_window_tokens":1000000}`), 0o600); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			if r.claude {
				if err := os.Symlink(fakeClaudePath(), filepath.Join(bin, "claude")); err != nil {
					t.Fatal(err)
				}
			}
			if r.login {
				writeJSON(t, filepath.Join(dir, "auth", "codex.json"), map[string]string{"access_token": "x", "account_id": "a"})
			}
			cmd := exec.Command(harnessBin, "acp")
			cmd.Dir = t.TempDir()
			cmd.Env = cleanEnv(map[string]string{"HARNESS_SESSION_DIR": t.TempDir(), "HARNESS_CONFIG": config, "ANTHROPIC_API_KEY": r.key, "PATH": bin})
			in, out, g, stderr := startACPCmd(t, cmd)
			c := newACPConn(t, schema, in, out)
			c.dirs["main"] = t.TempDir()
			if r.want == "" {
				f := c.response(c.request("session/new", map[string]any{"cwd": c.dirs["main"], "mcpServers": []any{}}))
				c.mustBeError(f, acpInternal)
				data, _ := f["error"].(map[string]any)["data"].(map[string]any)
				if data["code"] != "model_unavailable" {
					t.Errorf("error data = %v, want model_unavailable", data)
				}
			} else {
				acpNew{as: "a"}.run(t, c)
				if got := acpOptionValue(acpNewOptions(c), "model"); got != r.want {
					t.Errorf("current model = %v, want %q", got, r.want)
				}
			}
			c.finish(func() { t.Helper(); waitACPExit(t, g, stderr) })
		})
	}
}
