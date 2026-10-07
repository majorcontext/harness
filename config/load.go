package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load reads one config file. A missing file is an empty Config. A key that Config has no field for is an
// error that names the path and the key. Providers are validated after the merge, not here.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	c.SessionDir = expandHome(c.SessionDir)
	if err := c.validateFile(); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	return &c, nil
}

// Path resolves the effective user config path: $HARNESS_CONFIG if set, otherwise
// ~/.harness/config.json.
func Path() string {
	if p := os.Getenv("HARNESS_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".harness", "config.json")
	}
	return filepath.Join(home, ".harness", "config.json")
}

// LoadProject merges <dir>/.harness.json over the user config and validates the result once.
// A non-empty project value overrides; maps merge by key; AppendSystemPrompt adds the project
// segments after the user segments.
func LoadProject(dir string) (*Config, error) {
	cfg, _, err := LoadProjectWithInfo(dir)
	return cfg, err
}

// LoadInfo names the config file that LoadProjectWithInfo found and counts what it declares.
type LoadInfo struct {
	// Path is the project file when it exists, else the user file when it exists, else empty.
	Path string
	// Processes, MCPServers, and Plugins count the merged declarations.
	Processes  int
	MCPServers int
	Plugins    int
	// SessionSync is the merged value, verbatim.
	SessionSync string
}

// LoadProjectWithInfo is LoadProject plus the LoadInfo an operator-facing boot log needs.
func LoadProjectWithInfo(dir string) (*Config, LoadInfo, error) {
	userPath := Path()
	userExists := fileExists(userPath)
	user, err := Load(userPath)
	if err != nil {
		return nil, LoadInfo{}, err
	}
	projPath := filepath.Join(dir, ".harness.json")
	projExists := fileExists(projPath)
	proj, err := Load(projPath)
	if err != nil {
		return nil, LoadInfo{}, err
	}
	cfg, err := mergeAndValidate(user, proj)
	if err != nil {
		return nil, LoadInfo{}, err
	}
	info := LoadInfo{
		Processes:   len(cfg.Processes),
		MCPServers:  len(cfg.MCPServers),
		Plugins:     len(cfg.Plugins),
		SessionSync: cfg.SessionSync,
	}
	switch {
	case projExists:
		info.Path = projPath
	case userExists:
		info.Path = userPath
	}
	return cfg, info, nil
}

// fileExists reports whether os.Stat sees path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// mergeAndValidate merges, fills provider defaults, and validates.
func mergeAndValidate(base, over *Config) (*Config, error) {
	out := merge(base, over)
	applyProviderDefaults(out.Providers)
	return out, out.Validate()
}

// validateAppendSystemPromptArgs prevents ExtraArgs from replacing the managed value or selecting
// the mutually exclusive file option.
func validateAppendSystemPromptArgs(cfg *Config) error {
	if len(cfg.AppendSystemPrompt) == 0 {
		return nil
	}
	for name, p := range cfg.Providers {
		if p.Type != TypeClaudeCodeCLI {
			continue
		}
		for _, arg := range p.ExtraArgs {
			if claudeCodeAppendPromptArg(arg) {
				return fmt.Errorf("providers.%s.extra_args: %q conflicts with append_system_prompt", name, arg)
			}
		}
	}
	return nil
}

func claudeCodeAppendPromptArg(arg string) bool {
	return arg == "--append-system-prompt" ||
		strings.HasPrefix(arg, "--append-system-prompt=") ||
		arg == "--append-system-prompt-file" ||
		strings.HasPrefix(arg, "--append-system-prompt-file=")
}

// expandHome expands a leading "~/" (or a lone "~") against $HOME.
func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
