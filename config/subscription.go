package config

import (
	"os"
)

// SubscriptionCodexModel is the default model once the ChatGPT login exists. It must be a listed Codex model.
const SubscriptionCodexModel = "codex/gpt-6.1-sol"

// SubscriptionClaudeModel is the default model once the Claude Code program is installed and signed in.
const SubscriptionClaudeModel = "claude-code/sonnet"

// defaultModel is the model of a config that sets no model: the subscription of the user when SubscriptionDefault is set, the API key of DefaultModel is absent, and a subscription is in use, else DefaultModel.
// It makes one environment lookup, one stat of the Codex login file, and one PATH lookup. It reads no file content, makes no network call, and starts no process.
func (c *Config) defaultModel() string {
	if c == nil || !c.SubscriptionDefault {
		return DefaultModel
	}
	entries := c.ProviderEntries()
	if apiKeyPresent(entries) {
		return DefaultModel
	}
	if entries["codex"].UsesChatGPTLogin() && nonEmptyFile(c.CodexAuthFile) {
		return SubscriptionCodexModel
	}
	if p := entries["claude-code"]; p.UsesClaudeCLI() && onPath(p.CLIBinary()) {
		return SubscriptionClaudeModel
	}
	return DefaultModel
}

// apiKeyPresent says that the credential of the built-in default is set, so the default works and stays. A server holds
// the credential of an entry with auth injected.
func apiKeyPresent(entries map[string]Provider) bool {
	if entries["anthropic"].UsesInjectedAuth() {
		return true
	}
	env := entries["anthropic"].KeyEnv("anthropic")
	return env != "" && os.Getenv(env) != ""
}

func nonEmptyFile(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}
