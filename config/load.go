package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Load reads one config file. A missing file is an empty Config. A key that Config has no field for is an
// error that names the path and the key. Providers are validated after the merge, not here.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, err
	}
	return Parse(path, data)
}

// Parse decodes one config document like Load, and name labels its errors.
func Parse(name string, data []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", name, err)
	}
	c.SessionDir = expandHome(c.SessionDir)
	if err := c.validateFile(); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", name, err)
	}
	return &c, nil
}

// LoadProject merges <dir>/.harness.json over the config file at userPath and validates the result
// once. A non-empty project value overrides; maps merge by key; AppendSystemPrompt adds the project
// segments after the user segments; Plugins list the project plugins, then the user plugins, and a
// name in both files is an error. A missing file is an empty Config.
func LoadProject(userPath, dir string) (*Config, error) {
	user, err := Load(userPath)
	if err != nil {
		return nil, err
	}
	return MergeProject(user, userPath, dir, "")
}

// MergeProject is LoadProject for a user config that the caller holds. userName labels it in errors. A non-empty proxyBase fills the providers that set no BaseURL (see WithProxyBase) before the validation.
func MergeProject(user *Config, userName, dir, proxyBase string) (*Config, error) {
	projPath := filepath.Join(dir, ".harness.json")
	proj, err := Load(projPath)
	if err != nil {
		return nil, err
	}
	if err := rejectSharedPlugins(user, proj, userName, projPath); err != nil {
		return nil, err
	}
	if err := rejectProjectProviderKeys(user, proj, userName, projPath); err != nil {
		return nil, err
	}
	out := merge(user, proj).WithProxyBase(proxyBase)
	return &out, out.Validate()
}

// rejectProjectProviderKeys fails when the project file sets auth, or sets a key that picks the endpoint or
// the secret of a request on a provider that the user config defines or that is built in. A project file
// then cannot send the API key or the login to a URL of its own.
func rejectProjectProviderKeys(user, proj *Config, userName, projPath string) error {
	for _, name := range slices.Sorted(maps.Keys(proj.Providers)) {
		p := proj.Providers[name]
		if p.Auth != "" {
			return fmt.Errorf("providers.%s.auth is set in %s, but only the user config %s sets auth", name, projPath, userName)
		}
		_, defined := user.Providers[name]
		if !defined && !nativeProviderKeys[name] {
			continue
		}
		for _, k := range []struct {
			key string
			set bool
		}{
			{"base_url", p.BaseURL != ""},
			{"responses_path", p.ResponsesPath != ""},
			{"extra_headers", len(p.ExtraHeaders) > 0},
			{"api_key_env", p.APIKeyEnv != ""},
			{"permission_mode", p.PermissionMode != ""},
		} {
			if k.set {
				return fmt.Errorf("providers.%s.%s is set in %s, but the provider is built in or defined in %s, and a project file cannot set this key on such a provider", name, k.key, projPath, userName)
			}
		}
	}
	return nil
}

// rejectSharedPlugins fails when a plugin name is in both layers, so a project cannot shadow a user plugin.
func rejectSharedPlugins(user, proj *Config, userPath, projPath string) error {
	for _, u := range user.Plugins {
		for _, p := range proj.Plugins {
			if u.Name != "" && u.Name == p.Name {
				return fmt.Errorf("plugins: plugin %q is in both %s and %s", u.Name, userPath, projPath)
			}
		}
	}
	return nil
}

// mergeAndValidate merges and validates.
func mergeAndValidate(base, over *Config) (*Config, error) {
	out := merge(base, over)
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
