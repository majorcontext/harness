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
	// Instructions, when set to false, disables project-instruction
	// (AGENTS.md) injection into the system prompt. A nil value (the field
	// omitted) leaves injection enabled — a *bool so "unset" and "false" are
	// distinguishable across the project-config merge.
	Instructions *bool `json:"instructions,omitempty"`
	// InstructionsPath overrides the auto-discovered AGENTS.md with a specific
	// file to load instead of walking up from the working directory.
	InstructionsPath string `json:"instructions_path,omitempty"`
	// SkillsDirs lists directories scanned for Agent Skills (agentskills.io).
	// A nil (omitted) value leaves the engine default in place: use
	// <WorkDir>/.agents/skills when it exists. In the project-config merge a
	// non-empty project value replaces the user value entirely.
	SkillsDirs []string `json:"skills_dirs,omitempty"`
	// GoalEvaluatorModel names the model ref (or alias) used to evaluate goal
	// completion for `harness run --goal` and the server's goal endpoints.
	// There is no default — goal use requires this field to be set. Resolve it
	// with ResolveModel so aliases apply.
	GoalEvaluatorModel string `json:"goal_evaluator_model,omitempty"`
	// Plugins lists the plugin processes to wire into every session's
	// engine.Config.Hooks (see package plugin). Order matters: sync hooks
	// chain across plugins in this order, each seeing the previous plugin's
	// mutations. A nil (omitted) value disables plugins entirely. In the
	// project-config merge a non-empty project value replaces the user
	// value entirely (arrays override, like SkillsDirs).
	Plugins []PluginSpec `json:"plugins,omitempty"`
	// PluginHTTPHeaders are stamped on every plugin's outbound HTTP traffic
	// (plugin.Options.HTTPHeaders, e.g. workspace attribution). Maps merge
	// key by key in the project-config merge, like Aliases and Providers.
	PluginHTTPHeaders map[string]string `json:"plugin_http_headers,omitempty"`
}

// PluginSpec configures one plugin process, loaded verbatim into a
// plugin.Spec (Command, Env, Dir, Config) once its manifest is available
// (cached at install/probe time, keyed by binary hash — see `harness plugin
// probe` and package plugin's Probe/Host). Name and Command are required: a
// plugin with neither identifies nothing to spawn nor a manifest cache key,
// so Load rejects it rather than silently skipping it at session time.
type PluginSpec struct {
	// Name must match the plugin's own manifest name; it is also the cache
	// key and the config-order identity used for chaining.
	Name string `json:"name"`
	// Command is the argv used to spawn the plugin process (Command[0] is
	// resolved via PATH like any exec, exactly as plugin.Spec.Command).
	Command []string `json:"command"`
	// Env is appended to the harness environment when the plugin is spawned.
	Env []string `json:"env,omitempty"`
	// Dir is the plugin process's working directory.
	Dir string `json:"dir,omitempty"`
	// Config is this plugin's own config block, passed verbatim in
	// InitializeParams for the plugin to interpret however it likes.
	Config json.RawMessage `json:"config,omitempty"`
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
	if err := validatePlugins(c.Plugins); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	return &c, nil
}

// validatePlugins fails loudly on a plugin spec that cannot possibly be
// wired: a plugin needs a name (the manifest identity, cache key, and chain
// order) and a non-empty command (something to spawn). A silently-skipped
// malformed entry would run without the plugin its author expected — same
// philosophy as the AGENTS.md/skill loaders.
func validatePlugins(plugins []PluginSpec) error {
	seen := make(map[string]bool, len(plugins))
	for i, p := range plugins {
		if p.Name == "" {
			return fmt.Errorf("plugins[%d]: name is required", i)
		}
		if len(p.Command) == 0 {
			return fmt.Errorf("plugins[%d] (%s): command is required", i, p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("plugins[%d]: duplicate plugin name %q", i, p.Name)
		}
		seen[p.Name] = true
	}
	return nil
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
//   - Model, SessionDir, InstructionsPath, GoalEvaluatorModel: a non-empty
//     project value overrides the user value. Instructions (*bool): a non-nil
//     project value overrides.
//   - SkillsDirs: a non-empty project slice replaces the user slice entirely
//     (arrays override, they do not concatenate); an empty/omitted project
//     value inherits the user value.
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
	if over.Instructions != nil {
		out.Instructions = over.Instructions
	}
	if over.InstructionsPath != "" {
		out.InstructionsPath = over.InstructionsPath
	}
	if over.GoalEvaluatorModel != "" {
		out.GoalEvaluatorModel = over.GoalEvaluatorModel
	}
	// Arrays override wholesale: a non-empty project value replaces the user
	// value entirely; otherwise inherit. Copy so the merged config never
	// aliases either input's slice.
	src := out.SkillsDirs
	if len(over.SkillsDirs) > 0 {
		src = over.SkillsDirs
	}
	if len(src) > 0 {
		out.SkillsDirs = append([]string(nil), src...)
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
	// Plugins override wholesale, like SkillsDirs: a non-empty project list
	// replaces the user list entirely (config order is significant — the
	// sync-hook chain runs in this order — so merging entry-by-entry would
	// silently reorder or interleave two unrelated plugin lists).
	pSrc := out.Plugins
	if len(over.Plugins) > 0 {
		pSrc = over.Plugins
	}
	if len(pSrc) > 0 {
		out.Plugins = append([]PluginSpec(nil), pSrc...)
	} else {
		out.Plugins = nil
	}
	if n := len(base.PluginHTTPHeaders) + len(over.PluginHTTPHeaders); n > 0 {
		m := make(map[string]string, n)
		for k, v := range base.PluginHTTPHeaders {
			m[k] = v
		}
		for k, v := range over.PluginHTTPHeaders {
			m[k] = v
		}
		out.PluginHTTPHeaders = m
	} else {
		out.PluginHTTPHeaders = nil
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
