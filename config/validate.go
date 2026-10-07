package config

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
)

// Validate returns the first rule that c breaks. It never changes c.
func (c *Config) Validate() error {
	providers := maps.Clone(c.Providers)
	applyProviderDefaults(providers)
	if err := cmp.Or(c.validateFile(), validateProviders(providers), validateAppendSystemPromptArgs(c)); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// validateFile holds the rules that one config file must meet before merge.
func (c *Config) validateFile() error {
	return cmp.Or(
		validatePlugins(c.Plugins),
		validateMCPServers(c.MCPServers),
		validateProcesses(c.Processes),
		validateSessionSync(c.SessionSync),
		validateSync(c.OwnerEpoch, c.Sync),
		validateMCPToolLoading(c.MCPToolLoading, c.MCPToolLoadingThreshold),
		c.validateTaskLimits(),
	)
}

// Validate returns an error for a process entry that cannot be spawned.
func (p ProcessSpec) Validate() error {
	if len(p.Command) == 0 {
		return errors.New("command is required (non-empty argv)")
	}
	for _, port := range p.Ports {
		if err := validatePort(port); err != nil {
			return fmt.Errorf("invalid ports entry: %w", err)
		}
	}
	gates := 0
	if p.ReadyRegex != "" {
		gates++
	}
	if p.ReadyPort != 0 {
		gates++
	}
	if p.ReadyHTTP != "" {
		gates++
	}
	if gates > 1 {
		return errors.New("at most one of ready_regex, ready_port, ready_http may be set")
	}
	if p.ReadyRegex != "" {
		if _, err := regexp.Compile(p.ReadyRegex); err != nil {
			return fmt.Errorf("invalid ready_regex: %w", err)
		}
	}
	if p.ReadyPort != 0 {
		if err := validatePort(p.ReadyPort); err != nil {
			return fmt.Errorf("invalid ready_port: %w", err)
		}
	}
	if p.ReadyHTTP != "" {
		// ParseRequestURI accepts a URL with no scheme or host, and the gate then spins to its timeout.
		u, err := url.ParseRequestURI(p.ReadyHTTP)
		if err != nil {
			return fmt.Errorf("invalid ready_http: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("invalid ready_http %q: scheme must be http or https (did you forget the http:// prefix?)", p.ReadyHTTP)
		}
		if u.Host == "" {
			return fmt.Errorf("invalid ready_http %q: missing host", p.ReadyHTTP)
		}
	}
	return nil
}

func validatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port %d out of range (1-65535)", port)
	}
	return nil
}

func (c *Config) validateTaskLimits() error {
	switch {
	case c.MaxTaskDepth < 0:
		return fmt.Errorf("max_task_depth must not be negative, got %d", c.MaxTaskDepth)
	case c.MaxConcurrentTasks < 0:
		return fmt.Errorf("max_concurrent_tasks must not be negative, got %d", c.MaxConcurrentTasks)
	case c.MaxTreeTokens < 0:
		return fmt.Errorf("max_tree_tokens must not be negative, got %d", c.MaxTreeTokens)
	}
	return nil
}
