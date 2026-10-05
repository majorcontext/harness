package e2e

import (
	"testing"
)

func TestContractModels(t *testing.T) {
	key := func(extra map[string]any) map[string]any {
		p := map[string]any{"api_key_env": "ANTHROPIC_API_KEY"}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	creates := func(models ...string) []action {
		var out []action
		for _, m := range models {
			out = append(out, tryCreate{model: m})
		}
		return out
	}
	runScenarios(t, []scenario{
		{
			name: "create_checks_the_model",
			config: map[string]any{
				"context_window_tokens": 0,
				"model":                 "fast",
				"aliases":               map[string]any{"fast": "nope/gpt-5"},
				"providers": map[string]any{
					"anthropic": key(nil),
					"codex":     key(map[string]any{"type": "openai", "base_url": "https://codex.test"}),
					"openai":    key(nil),
					"bifrost":   key(map[string]any{"type": "openai-compat", "base_url": "https://bifrost.test"}),
				},
			},
			actions: creates("anthropic/claude-opus-5", "anthropic/no-such-model", "codex/gpt-5", "codex/no-such-model", "openai/gpt-5",
				"bifrost/fireworks/accounts/fireworks/routers/firerouter", "nope/gpt-5", "gpt-5", "fast", ""),
		},
		{
			name:    "create_without_a_model_takes_the_default_model",
			config:  map[string]any{"model": ""},
			actions: creates(""),
		},
		{
			name: "create_takes_an_unknown_model_with_a_configured_window",
			config: map[string]any{
				"context_window_tokens": 1000,
				"providers":             map[string]any{"anthropic": key(nil), "openrouter": key(nil)},
			},
			actions: creates("anthropic/no-such-model", "openrouter/vendor/model"),
		},
		{
			name:    "create_takes_an_unknown_model_when_no_window_is_required",
			config:  map[string]any{"context_window_tokens": 0, "context_window_required": false},
			actions: creates("anthropic/no-such-model"),
		},
		{
			name: "models_lists_the_configured_providers",
			config: map[string]any{
				"model":                "codex/gpt-6-sol",
				"goal_evaluator_model": "",
				"providers": map[string]any{
					"codex":       key(map[string]any{"type": "openai", "base_url": "https://codex.test"}),
					"claude-code": map[string]any{"type": "claude-code-cli"},
					"work":        key(map[string]any{"type": "openai", "base_url": "https://work.test"}),
				},
			},
			actions: append([]action{models{}}, creates("codex/gpt-6-luna", "claude-code/sonnet")...),
		},
	})
}
