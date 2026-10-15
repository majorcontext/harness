// Package login configures an example to run on the logins of this machine.
package login

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/majorcontext/harness/config"
)

// Config sets the subscription default and the login file of "harness login
// codex". With no model set, the runtime keeps the API default when
// ANTHROPIC_API_KEY is set, else it uses the Codex login, else the claude
// program on PATH.
func Config() config.Config {
	return config.Config{SubscriptionDefault: true, CodexAuthFile: CodexAuthFile()}
}

// CodexAuthFile is auth/codex.json beside $HARNESS_CONFIG, else in ~/.harness.
// The Go API has no default for it.
func CodexAuthFile() string {
	if p := os.Getenv("HARNESS_CONFIG"); p != "" {
		return filepath.Join(filepath.Dir(p), "auth", "codex.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".harness", "auth", "codex.json")
}

// Alternatives lists the models other than current that can run now: the Codex
// login, the claude program on PATH, and the API haiku model with
// ANTHROPIC_API_KEY.
func Alternatives(current string) []string {
	var models []string
	if info, err := os.Stat(CodexAuthFile()); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		models = append(models, config.SubscriptionCodexModel)
	}
	if _, err := exec.LookPath("claude"); err == nil {
		models = append(models, config.SubscriptionClaudeModel)
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		models = append(models, "anthropic/claude-haiku-4-5-20251001")
	}
	var out []string
	for _, m := range models {
		if m != current {
			out = append(out, m)
		}
	}
	return out
}
