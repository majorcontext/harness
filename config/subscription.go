package config

import (
	"os"
)

// SubscriptionCodexModel is the default model once the ChatGPT login exists. It must be a listed Codex model.
const SubscriptionCodexModel = "codex/gpt-6.1-sol"

// SubscriptionClaudeModel is the default model once the Claude Code program is installed and signed in.
const SubscriptionClaudeModel = "claude-code/sonnet"

// defaultModel is the model of a config that sets no model: the subscription of the user when one is in use, else DefaultModel.
// It makes one stat of the Codex login file and one PATH lookup. It reads no file content, makes no network call, and starts no process.
func (c *Config) defaultModel() string {
	if c == nil {
		return DefaultModel
	}
	entries := c.ProviderEntries()
	if entries["codex"].UsesChatGPTLogin() && nonEmptyFile(c.CodexAuthFile) {
		return SubscriptionCodexModel
	}
	if p := entries["claude-code"]; p.UsesClaudeCLI() && onPath(p.CLIBinary()) {
		return SubscriptionClaudeModel
	}
	return DefaultModel
}

func nonEmptyFile(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}
