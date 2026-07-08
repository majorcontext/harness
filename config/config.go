// Package config loads the harness CLI configuration: a single JSON file
// (plus an optional per-project override) parsed in one flat pass with the
// standard library only. Nothing here touches the network or spawns
// processes — config loading sits on the startup path, so it is at most two
// file reads (the user config, plus the project override when present).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/majorcontext/harness/message"
)

// DefaultModel is the hard fallback when neither a flag nor config names one.
const DefaultModel = "anthropic/claude-fable-5"

// Config is the parsed harness configuration. The zero value is valid and
// represents "no configuration": every method degrades to built-in defaults.
type Config struct {
	// Model is the default model ref ("provider/model") or an alias key.
	Model string `json:"model,omitempty"`
	// Aliases maps short names ("fast", "smart") to model refs. Resolution is
	// one level only — an alias target is never itself looked up as an alias.
	Aliases map[string]string `json:"aliases,omitempty"`
	// SessionDir is where session logs live. A leading "~/" is expanded
	// against $HOME at load time.
	SessionDir string `json:"session_dir,omitempty"`
	// Providers configures each provider family by name (e.g. "anthropic").
	Providers map[string]Provider `json:"providers,omitempty"`
}

// Provider is per-family provider configuration.
type Provider struct {
	// APIKeyEnv names the environment variable to read the API key from.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// BaseURL overrides the provider's default API base URL when non-empty.
	BaseURL string `json:"base_url,omitempty"`
}

// Load reads a single config file. A missing file yields a zero-value Config
// and a nil error (config is optional). Malformed JSON or an unknown field
// (json.Decoder with DisallowUnknownFields, so typos surface) yields an error
// naming the path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	c.SessionDir = expandHome(c.SessionDir)
	return &c, nil
}

// Path resolves the effective user config path: $HARNESS_CONFIG if set,
// otherwise ~/.harness/config.json.
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

// LoadProject loads the user config (from Path) and, if <dir>/.harness.json
// exists, merges it on top. Merge rules, applied field by field (no
// reflection):
//
//   - Model, SessionDir: a non-empty project value overrides the user value.
//   - Aliases, Providers: maps are merged key by key — project keys are added
//     and override user keys of the same name. Within a Provider, a non-empty
//     project field (APIKeyEnv, BaseURL) overrides the user field.
//
// A missing project file is not an error; the user config is returned as-is
// (Load yields a zero-value Config for a missing file, and merging a zero
// override is a no-op).
func LoadProject(dir string) (*Config, error) {
	user, err := Load(Path())
	if err != nil {
		return nil, err
	}
	proj, err := Load(filepath.Join(dir, ".harness.json"))
	if err != nil {
		return nil, err
	}
	return merge(user, proj), nil
}

// merge returns base overlaid with the non-zero fields of over. The result
// never aliases either input's maps: fresh maps are always built, even when
// over contributes no entries, so mutating the merged config cannot corrupt
// the inputs.
func merge(base, over *Config) *Config {
	out := *base // copy scalar fields; maps are rebuilt below
	out.Aliases = nil
	out.Providers = nil
	if over.Model != "" {
		out.Model = over.Model
	}
	if over.SessionDir != "" {
		out.SessionDir = over.SessionDir
	}
	if n := len(base.Aliases) + len(over.Aliases); n > 0 {
		m := make(map[string]string, n)
		for k, v := range base.Aliases {
			m[k] = v
		}
		for k, v := range over.Aliases {
			m[k] = v
		}
		out.Aliases = m
	}
	if n := len(base.Providers) + len(over.Providers); n > 0 {
		m := make(map[string]Provider, n)
		for k, v := range base.Providers {
			m[k] = v
		}
		for k, v := range over.Providers {
			if ex, ok := m[k]; ok {
				if v.APIKeyEnv != "" {
					ex.APIKeyEnv = v.APIKeyEnv
				}
				if v.BaseURL != "" {
					ex.BaseURL = v.BaseURL
				}
				m[k] = ex
			} else {
				m[k] = v
			}
		}
		out.Providers = m
	}
	return &out
}

// ResolveModel turns a model string into a ModelRef. An empty string falls
// back to the config's Model, then to DefaultModel. The result is looked up
// once in Aliases (one level, no recursion) and then parsed. A bare name that
// is neither a known alias nor a valid "provider/model" ref is an error.
func (c *Config) ResolveModel(s string) (message.ModelRef, error) {
	if s == "" && c != nil {
		s = c.Model
	}
	if s == "" {
		s = DefaultModel
	}
	if c != nil {
		if target, ok := c.Aliases[s]; ok {
			s = target
		}
	}
	return message.ParseModelRef(s)
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
