package config

// DefaultModel is the model ref used when neither a flag nor config names one.
const DefaultModel = "anthropic/claude-fable-5"

// Defaults returns the built-in value of every defaulted key.
func Defaults() Config {
	return Config{
		Model:                   DefaultModel,
		ContextWindowRequired:   new(true),
		PromptRetries:           new(2),
		MaxTokensContinuations:  new(3),
		StreamIdleTimeoutS:      300,
		CompactionThreshold:     0.8,
		CompactionKeepTurns:     2,
		ModelTool:               new(true),
		InstructionsMaxBytes:    64 << 10,
		MCPToolLoadingThreshold: 20,
		MaxTaskDepth:            3,
		MaxConcurrentTasks:      20,
		Providers: map[string]Provider{
			"openrouter": {Type: TypeOpenAICompat, BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"},
		},
	}
}

// EnsureProviderDefaults fills the empty fields of each Defaults().Providers entry present in providers, in place.
func EnsureProviderDefaults(providers map[string]Provider) {
	applyProviderDefaults(providers)
}

// applyProviderDefaults must run on the merged map, because one layer may set only some fields of an entry.
func applyProviderDefaults(providers map[string]Provider) {
	for name, def := range Defaults().Providers {
		p, ok := providers[name]
		if !ok {
			continue
		}
		if p.Type == "" {
			p.Type = def.Type
		}
		if p.BaseURL == "" {
			p.BaseURL = def.BaseURL
		}
		if p.APIKeyEnv == "" {
			p.APIKeyEnv = def.APIKeyEnv
		}
		if p.Family == "" {
			p.Family = def.Family
		}
		providers[name] = p
	}
}

// PromptRetriesValue returns prompt_retries, or its default.
func (c *Config) PromptRetriesValue() int {
	return valueOr(c, func(c *Config) *int { return c.PromptRetries })
}

// ContextWindowRequiredValue returns context_window_required, or its default.
func (c *Config) ContextWindowRequiredValue() bool {
	return valueOr(c, func(c *Config) *bool { return c.ContextWindowRequired })
}

// MaxTokensContinuationsValue returns max_tokens_continuations, or its default.
func (c *Config) MaxTokensContinuationsValue() int {
	return valueOr(c, func(c *Config) *int { return c.MaxTokensContinuations })
}

// ModelToolEnabled returns model_tool, or its default.
func (c *Config) ModelToolEnabled() bool {
	return valueOr(c, func(c *Config) *bool { return c.ModelTool })
}

func valueOr[T any](c *Config, field func(*Config) *T) T {
	if c != nil && field(c) != nil {
		return *field(c)
	}
	d := Defaults()
	return *field(&d)
}
