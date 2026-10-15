package config

// DefaultModel is the model ref used when neither a flag nor config names one.
const DefaultModel = "anthropic/claude-fable-5"

// Defaults returns the built-in value of every defaulted key.
func Defaults() Config {
	return Config{
		Model:                   DefaultModel,
		RequestMaxRetries:       new(4),
		StreamMaxRetries:        new(5),
		StreamIdleTimeoutS:      300,
		CompactionThreshold:     0.8,
		CompactionKeepTurns:     2,
		ModelTool:               new(true),
		InstructionsMaxBytes:    64 << 10,
		MCPToolLoadingThreshold: 20,
		MaxTaskDepth:            3,
		MaxConcurrentTasks:      20,
	}
}

// MaxRetries is the largest value of request_max_retries and stream_max_retries.
const MaxRetries = 100

// RequestMaxRetriesValue returns request_max_retries, or its default, at most MaxRetries.
func (c *Config) RequestMaxRetriesValue() int {
	return min(valueOr(c, func(c *Config) *int { return c.RequestMaxRetries }), MaxRetries)
}

// StreamMaxRetriesValue returns stream_max_retries, or its default, at most MaxRetries.
func (c *Config) StreamMaxRetriesValue() int {
	return min(valueOr(c, func(c *Config) *int { return c.StreamMaxRetries }), MaxRetries)
}

// ModelToolEnabled returns model_tool, or its default.
func (c *Config) ModelToolEnabled() bool {
	return valueOr(c, func(c *Config) *bool { return c.ModelTool })
}

// WebFetchEnabled returns web.fetch, which is off when unset.
func (c *Config) WebFetchEnabled() bool {
	return c != nil && c.Web.Fetch != nil && *c.Web.Fetch
}

func valueOr[T any](c *Config, field func(*Config) *T) T {
	if c != nil && field(c) != nil {
		return *field(c)
	}
	d := Defaults()
	return *field(&d)
}
