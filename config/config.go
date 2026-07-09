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

// TypeOpenAICompat selects the generic OpenAI-compatible chat-completions
// adapter (provider/openaicompat) for a Provider config entry — the wire
// format spoken by OpenRouter, Ollama, vLLM, and similar deployments. It is
// the only non-empty Provider.Type value Load accepts today.
const TypeOpenAICompat = "openai-compat"

// nativeProviderKeys are the only providers map keys allowed an empty Type
// with no further defaulting: the built-in adapters cmd/harness's registry
// wires directly by name (provider/anthropic.Family and
// provider/openai.Family). A missing or typo'd Type on any other key must
// not silently produce no adapter at startup — see nativeDefaultProviders
// for the one key ("openrouter") that gets real built-in field defaults
// instead of a bare pass, and validateProviders for the loud failure every
// other key gets.
var nativeProviderKeys = map[string]bool{
	"anthropic": true,
	"openai":    true,
}

// nativeDefaultProviders holds the built-in field values for providers map
// keys that have a zero-config default outside this package — today just
// "openrouter" (cmd/harness's ensureDefaultOpenRouter registers it with
// these same values when the providers map has no "openrouter" key at
// all). When the key *is* present, applyProviderDefaults fills in whatever
// fields the entry leaves empty from here before validateProviders runs, so
// {"openrouter": {"api_key_env": "X"}} is a complete, valid entry: type and
// base_url are inherited, only api_key_env is overridden. This is what
// keeps the unrepresentable-bad-state property (see validateProviders)
// from overcorrecting into requiring a full entry for the one key that has
// a sensible built-in default to begin with — a typo'd or unsupported type
// still fails loudly, but a same-name key with only one field set to
// override does not.
var nativeDefaultProviders = map[string]Provider{
	"openrouter": {
		Type:      TypeOpenAICompat,
		BaseURL:   "https://openrouter.ai/api/v1",
		APIKeyEnv: "OPENROUTER_API_KEY",
	},
}

// applyProviderDefaults fills empty fields of any nativeDefaultProviders
// entry present in providers from the built-in default, in place. It must
// run on the fully merged config, after layering user and project files
// together and before validateProviders — a per-layer entry (e.g. a
// project override naming only api_key_env) is not itself a complete
// entry, but the merged result must be.
func applyProviderDefaults(providers map[string]Provider) {
	for name, def := range nativeDefaultProviders {
		p, ok := providers[name]
		if !ok {
			continue // wholly absent: cmd/harness's own default registration handles this case
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

// Provider is per-family provider configuration.
type Provider struct {
	// Type selects the adapter to build for this entry. Empty (the
	// default) means a built-in native adapter — anthropic or openai — is
	// expected, wired directly by name in cmd/harness's registry; only the
	// "anthropic" and "openai" providers map keys may leave Type empty
	// (see nativeProviderKeys). TypeOpenAICompat ("openai-compat") builds a
	// generic provider/openaicompat client instead: the providers map key
	// becomes the new provider family, routed by the first segment of a
	// "provider/model" ref exactly like any built-in family. Any other
	// value, or an empty value on any other key, fails Load loudly — a
	// typo'd or missing type must not silently produce no adapter at
	// startup.
	Type string `json:"type,omitempty"`
	// APIKeyEnv names the environment variable to read the API key from.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// BaseURL overrides the provider's default API base URL when non-empty.
	// Required when Type is TypeOpenAICompat — there is no built-in default
	// base URL for an arbitrary compat entry (the one exception, the
	// built-in "openrouter" entry, is supplied by cmd/harness, not here).
	BaseURL string `json:"base_url,omitempty"`
	// Family overrides the ProviderData tag / wire-quirk key the
	// openaicompat adapter uses (some deployments need family-specific
	// transcoding quirks). Only meaningful when Type is TypeOpenAICompat;
	// defaults to the providers map key when empty.
	Family string `json:"family,omitempty"`
	// ExtraHeaders are sent verbatim on every request by the openaicompat
	// adapter, e.g. OpenRouter's HTTP-Referer/X-Title attribution headers.
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`
}

// Load reads a single config file. A missing file yields a zero-value Config
// and a nil error (config is optional). Malformed JSON or an unknown field
// (json.Decoder with DisallowUnknownFields, so typos surface) yields an error
// naming the path.
//
// Load does not validate providers: a single file is only ever one layer of
// the final config (see LoadProject), and a layer may legitimately be
// incomplete on its own — a project override naming only a provider's
// api_key_env is not a complete providers entry by itself, but the merged
// result must be. Provider validation therefore runs once, on the merged
// config; see validateProviders and mergeAndValidate. Plugins are not
// merged field by field (a non-empty project list replaces the user list
// wholesale — see merge), so a per-file plugin is already the whole entry
// and is validated here.
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

// validateProviders fails loudly on a providers entry that cannot possibly
// be wired: an unrecognized Type (a typo must not silently produce no
// adapter at startup — same philosophy as validatePlugins), an empty Type
// on a key that is neither native (nativeProviderKeys) nor native-default
// (nativeDefaultProviders), or a TypeOpenAICompat entry missing the BaseURL
// it has no built-in default for. Callers must run applyProviderDefaults on
// the same (fully merged) providers map first, so a nativeDefaultProviders
// key that only overrides one field is validated as the complete entry it
// becomes after defaulting, not the partial one a single config layer
// wrote.
func validateProviders(providers map[string]Provider) error {
	for name, p := range providers {
		switch p.Type {
		case "":
			if !nativeProviderKeys[name] {
				return fmt.Errorf("providers.%s: type is required (empty type is only valid for the built-in %q/%q entries); valid types: \"\" (native anthropic/openai override), %q", name, "anthropic", "openai", TypeOpenAICompat)
			}
			// Legacy/native provider entry (anthropic or openai); no
			// further validation here.
		case TypeOpenAICompat:
			if p.BaseURL == "" {
				return fmt.Errorf("providers.%s: base_url is required for type %q", name, TypeOpenAICompat)
			}
		default:
			return fmt.Errorf("providers.%s: unknown type %q", name, p.Type)
		}
	}
	return nil
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
//
// Providers are validated once, here, after the two layers are merged and
// applyProviderDefaults has filled in any nativeDefaultProviders field an
// entry left empty — never per file (see Load) — so a project override
// naming only a provider's api_key_env is validated as the complete entry
// it becomes once merged with the user layer (or with a native default),
// not rejected as incomplete on its own.
func LoadProject(dir string) (*Config, error) {
	user, err := Load(Path())
	if err != nil {
		return nil, err
	}
	proj, err := Load(filepath.Join(dir, ".harness.json"))
	if err != nil {
		return nil, err
	}
	return mergeAndValidate(user, proj)
}

// mergeAndValidate merges over onto base (see merge), applies built-in
// field defaults to any nativeDefaultProviders entry present in the result
// (see applyProviderDefaults), and validates the merged providers map (see
// validateProviders). This is the single point where a providers entry is
// judged complete or incomplete: always after layering, never per file.
func mergeAndValidate(base, over *Config) (*Config, error) {
	out := merge(base, over)
	applyProviderDefaults(out.Providers)
	if err := validateProviders(out.Providers); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return out, nil
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
				if v.Type != "" {
					ex.Type = v.Type
				}
				if v.APIKeyEnv != "" {
					ex.APIKeyEnv = v.APIKeyEnv
				}
				if v.BaseURL != "" {
					ex.BaseURL = v.BaseURL
				}
				if v.Family != "" {
					ex.Family = v.Family
				}
				if n := len(ex.ExtraHeaders) + len(v.ExtraHeaders); n > 0 {
					hm := make(map[string]string, n)
					for hk, hv := range ex.ExtraHeaders {
						hm[hk] = hv
					}
					for hk, hv := range v.ExtraHeaders {
						hm[hk] = hv
					}
					ex.ExtraHeaders = hm
				}
				m[k] = ex
			} else {
				if len(v.ExtraHeaders) > 0 {
					hm := make(map[string]string, len(v.ExtraHeaders))
					for hk, hv := range v.ExtraHeaders {
						hm[hk] = hv
					}
					v.ExtraHeaders = hm
				}
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
