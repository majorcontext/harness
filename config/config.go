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
	"github.com/majorcontext/harness/process"
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
	// MCPServers declares named MCP servers the engine connects to (lazily,
	// on first use) and registers as namespaced tools (mcp__<server>__<tool>,
	// the Claude Code convention — see package engine). Keyed by server name;
	// a nil (omitted) value configures no MCP servers. In the project-config
	// merge, keys merge like Providers/Aliases (new keys from either layer
	// are kept) but a same-name project entry replaces the user entry
	// wholesale (see merge) — MCPServerSpec's Command/Env/Headers slices
	// make field-by-field merging (as Provider gets) more confusing than
	// useful here.
	MCPServers map[string]MCPServerSpec `json:"mcp_servers,omitempty"`
	// Processes declares named dev/support processes the engine can
	// manage (start/stop/restart/status/logs) via the "process" session
	// tool and the server's /process endpoints (see package engine's
	// ProcessManager). Keyed by process name; a nil (omitted) value
	// configures no processes. Merge rules mirror MCPServers: keys merge,
	// but a same-name project entry replaces the user entry wholesale.
	Processes map[string]ProcessSpec `json:"processes,omitempty"`
	// ContextWindowTokens sets engine.Config.ContextWindowTokens for every
	// session this process creates: the model's context window size, in
	// tokens. Zero (omitted, the default) disables automatic compaction
	// entirely — see docs/design/context-compaction.md and issue #62 layer
	// 3. Opt-in: the engine has no built-in per-model table.
	ContextWindowTokens int `json:"context_window_tokens,omitempty"`
	// CompactionThreshold sets engine.Config.CompactionThreshold: the
	// fraction of ContextWindowTokens at which automatic compaction
	// triggers. Zero (omitted) defaults to 0.8 (see the engine).
	CompactionThreshold float64 `json:"compaction_threshold,omitempty"`
	// CompactionKeepTurns sets engine.Config.CompactionKeepTurns: how many
	// of the most recent turns automatic compaction always keeps verbatim.
	// Zero (omitted) defaults to 2 (see the engine); the effective value
	// can never go below 1.
	CompactionKeepTurns int `json:"compaction_keep_turns,omitempty"`
	// SessionSync selects the durability mechanism for attested session-store
	// writes (durable enqueue, session-create persist). "fsync" (default)
	// fsyncs the log file and, on first creation, its directory — correct for
	// local POSIX filesystems. "volume" skips both fsync round-trips: for
	// stores on continuously-synced network volumes whose own commit layer is
	// the documented durability boundary, where fsync adds no durability and
	// some FUSE/9p transports deadlock permanently on it (fsync(dirfd)
	// especially). With "volume", an attestation means the write(2) completed
	// and durability is delegated to the volume layer.
	SessionSync string `json:"session_sync,omitempty"`
}

// validSessionSync are the only accepted config values for SessionSync — see
// its doc comment. Checked in Load (a per-layer field: SessionSync overrides
// wholesale in merge, like Model/SessionDir, so an invalid value in either
// layer must fail regardless of which one ends up winning).
var validSessionSync = map[string]bool{"": true, "fsync": true, "volume": true}

// validateSessionSync fails loudly on an unrecognized session_sync value —
// same "cannot possibly be wired" philosophy as validatePlugins/
// validateMCPServers: a typo (e.g. "volumes") must not silently fall back to
// the default fsync behavior, since the whole point of "volume" mode is to
// avoid a real deadlock hazard on certain network-volume backends (see
// SessionSync's doc comment) — a silently-ignored typo would leave that
// hazard live.
func validateSessionSync(v string) error {
	if !validSessionSync[v] {
		return fmt.Errorf("session_sync: unknown value %q; valid values: \"\" (default), \"fsync\", \"volume\"", v)
	}
	return nil
}

// ProcessSpec configures one managed process (package engine's
// ProcessManager). Command is required (a non-empty argv); Dir, when set,
// is resolved against the engine's working directory. At most one of
// ReadyRegex/ReadyPort/ReadyHTTP may be set (see validateProcesses); each
// gates Start the same way — blocking until it matches — and
// ReadyTimeoutS bounds how long that block waits; <= 0 defaults to 60
// seconds (applied by the engine, not here — see engine.ProcessManager,
// mirroring MCPServerSpec's ConnectTimeout default pattern).
type ProcessSpec struct {
	// Command is the argv of the process; Command[0] is resolved via PATH
	// like any exec.
	Command []string `json:"command,omitempty"`
	// Dir is the process's working directory, resolved against the
	// engine's WorkDir when relative.
	Dir string `json:"dir,omitempty"`
	// Env is appended to the harness environment when the process is
	// spawned.
	Env []string `json:"env,omitempty"`
	// Ports lists TCP ports this process is expected to listen on — pure
	// declarative metadata (each entry must be in 1-65535; see
	// validateProcesses) surfaced to Status/GET /process, the process
	// tool's list/status output, and the ambient status block. Harness
	// never allocates, binds to, or enforces these.
	Ports []int `json:"ports,omitempty"`
	// ReadyRegex, when non-empty, must be a valid RE2 pattern (regexp.Compile).
	// A combined stdout+stderr log line matching it marks the process ready.
	ReadyRegex string `json:"ready_regex,omitempty"`
	// ReadyPort, when set, is a TCP port (1-65535): Start's ready gate
	// blocks until a plain TCP dial to 127.0.0.1:<port> succeeds, instead
	// of matching a log line. Unambiguous where ReadyRegex can match the
	// wrong task's output in a multiplexed log — see
	// docs/design/managed-processes.md.
	ReadyPort int `json:"ready_port,omitempty"`
	// ReadyHTTP, when set, is a URL: Start's ready gate blocks until a GET
	// to it returns any non-5xx status.
	ReadyHTTP string `json:"ready_http,omitempty"`
	// ReadyTimeoutS bounds Start's blocking wait for whichever ready gate
	// is configured, in seconds; <= 0 means the engine's default (60s).
	ReadyTimeoutS int `json:"ready_timeout_s,omitempty"`
}

// MCPServerSpec configures one MCP server (package mcp's client, wired by
// package engine). Exactly one of Command (a stdio server: argv, env,
// working directory) or URL (a Streamable HTTP server: endpoint, headers)
// must be set — validateMCPServers rejects an entry with neither or both,
// the same "cannot possibly be wired" philosophy as validatePlugins.
type MCPServerSpec struct {
	// Command is the argv of a stdio MCP server process; Command[0] is
	// resolved via PATH like any exec.
	Command []string `json:"command,omitempty"`
	// Env is appended to the harness environment when the stdio server is
	// spawned.
	Env []string `json:"env,omitempty"`
	// Dir is the stdio server process's working directory.
	Dir string `json:"dir,omitempty"`
	// URL is a Streamable HTTP MCP server's endpoint.
	URL string `json:"url,omitempty"`
	// Headers are static headers sent on every request to a Streamable
	// HTTP server, e.g. {"Authorization": "Bearer <token>"}.
	Headers map[string]string `json:"headers,omitempty"`
	// ConnectTimeoutS bounds this server's Initialize+ListAllTools for its
	// FIRST connect attempt (and each background retry attempt after a
	// failure — see engine.MCPManager), in seconds; 0/absent (the default)
	// leaves engine.MCPServerConfig.ConnectTimeout at zero, which the
	// engine itself then defaults to 15s (defaultMCPConnectTimeout). A
	// negative value cannot possibly be wired and is rejected loudly by
	// validateMCPServers. Named and typed like ProcessSpec.ReadyTimeoutS,
	// the existing duration-ish config-field convention (a plain
	// integer-seconds field, not a time.Duration/string, since JSON has no
	// duration literal).
	ConnectTimeoutS int `json:"connect_timeout_s,omitempty"`
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

// EnsureProviderDefaults fills empty fields of any nativeDefaultProviders
// entry present in providers (in place) from the built-in default — the
// same defaulting mergeAndValidate applies to every *Config LoadProject
// returns, exported here so a caller that builds providers by some other
// route (a hand-built *Config in a test, an embedder that skips
// LoadProject) can apply the identical guarantee itself.
//
// It is idempotent: every field it sets is only set when empty, so calling
// it twice, or calling it on a map LoadProject already defaulted, is a
// no-op the second time. That idempotence is what makes it safe to use
// defensively — e.g. cmd/harness's registry() calls this on cfg.Providers
// before building provider clients, so a minimal {"openrouter": {...}}
// entry resolves to the same adapter whether or not the *Config in hand
// ever passed through LoadProject. Without that call, registry() would
// instead silently depend on its caller already having run this — an
// init-order dependency that fails by producing no adapter at all rather
// than an error, exactly the failure mode this package's provider
// validation otherwise refuses to allow (see validateProviders).
func EnsureProviderDefaults(providers map[string]Provider) {
	applyProviderDefaults(providers)
}

// applyProviderDefaults is EnsureProviderDefaults' unexported
// implementation, shared by mergeAndValidate (the load-path choke point)
// and EnsureProviderDefaults (the defensive entry point for callers that
// bypass it). It must run on the fully merged config, after layering user
// and project files together and before validateProviders — a per-layer
// entry (e.g. a project override naming only api_key_env) is not itself a
// complete entry, but the merged result must be.
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
	if err := validateMCPServers(c.MCPServers); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	if err := validateProcesses(c.Processes); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	if err := validateSessionSync(c.SessionSync); err != nil {
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

// validateMCPServers fails loudly on an MCP server entry that cannot
// possibly be wired: exactly one of Command (stdio) or URL (Streamable
// HTTP) must be set, and the map key naming it must be non-empty (it is
// both the tool-namespace segment — mcp__<name>__<tool> — and, like a
// plugin's Name, the identity a caller would use to refer to the server).
// A silently-skipped malformed entry would run without the server its
// author expected — same philosophy as validatePlugins.
//
// The server name also must not contain "__" or start with "mcp__": the
// namespaced tool name engine/mcp.go builds is mcp__<server>__<tool>, and
// that encoding is only uniquely decodable back into (server, tool) if
// "__" cannot occur inside server. Without this check, server "a__b" tool
// "c" and server "a" tool "b__c" both produce the identical namespaced
// name mcp__a__b__c — two unrelated servers' tools silently colliding and
// becoming indistinguishable at call time. Rejecting "mcp__" as a prefix
// closes the same hole from the other end (server "mcp__weather" would
// itself already contain "__" and be caught by that check, but a bare
// "mcp" server with an empty-string-shaped remainder is worth naming
// explicitly since "mcp__" is the literal namespace prefix a config
// author could plausibly type by mistake).
func validateMCPServers(servers map[string]MCPServerSpec) error {
	for name, s := range servers {
		if name == "" {
			return fmt.Errorf("mcp_servers: server name is required (empty key)")
		}
		if strings.Contains(name, "__") {
			return fmt.Errorf("mcp_servers.%s: server name must not contain \"__\" (the namespaced tool name mcp__<server>__<tool> would not be uniquely decodable)", name)
		}
		if strings.HasPrefix(name, "mcp__") {
			return fmt.Errorf("mcp_servers.%s: server name must not start with \"mcp__\" (reserved for the tool-namespace prefix)", name)
		}
		hasCommand := len(s.Command) > 0
		hasURL := s.URL != ""
		switch {
		case !hasCommand && !hasURL:
			return fmt.Errorf("mcp_servers.%s: exactly one of command (stdio) or url (streamable HTTP) is required", name)
		case hasCommand && hasURL:
			return fmt.Errorf("mcp_servers.%s: command and url are mutually exclusive", name)
		}
		if s.ConnectTimeoutS < 0 {
			return fmt.Errorf("mcp_servers.%s: connect_timeout_s must not be negative (got %d)", name, s.ConnectTimeoutS)
		}
	}
	return nil
}

// validateProcesses fails loudly on a process entry that cannot possibly be
// wired: the map key naming it must be non-empty (it is the identity a
// caller uses to start/stop/restart/status/logs it — same "cannot possibly
// be wired" philosophy as validateMCPServers/validatePlugins). Everything
// else (Command, Ports, and the ready gates) is validated by
// process.ValidateDef itself — called directly, not reimplemented, so a
// config-file process entry and the process tool's runtime `declare`
// action are rejected with byte-for-byte identical error text (see that
// function's doc comment). An invalid entry would otherwise only fail the
// first time a session actually starts the process, far from the config
// that caused it.
func validateProcesses(processes map[string]ProcessSpec) error {
	for name, p := range processes {
		if name == "" {
			return fmt.Errorf("processes: process name is required (empty key)")
		}
		def := process.Def{
			Command:    p.Command,
			Ports:      p.Ports,
			ReadyRegex: p.ReadyRegex,
			ReadyPort:  p.ReadyPort,
			ReadyHTTP:  p.ReadyHTTP,
		}
		if err := process.ValidateDef(def); err != nil {
			return fmt.Errorf("processes.%s: %w", name, err)
		}
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
//   - Model, SessionDir, InstructionsPath, GoalEvaluatorModel, SessionSync: a
//     non-empty project value overrides the user value. Instructions (*bool):
//     a non-nil project value overrides.
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
	cfg, _, err := LoadProjectWithInfo(dir)
	return cfg, err
}

// LoadInfo describes which config file LoadProjectWithInfo actually found
// (if any) and summarizes the resulting merged config, for the one boot-
// time observability log line `harness serve`/`harness run` emit (see
// AGENTS.md's startup-config-observability rule). It carries no
// behavior — Path is purely which file to report to an operator, never
// re-parsed or re-read.
type LoadInfo struct {
	// Path is the effective config file path to report: the project
	// override (<dir>/.harness.json) when it exists, otherwise the user
	// config path (Path()) when it exists, otherwise empty — meaning no
	// config file was found at all. This is deliberately a single path
	// even though up to two files may have been merged: the project
	// override is what a misnamed-file operator error almost always
	// means (a typo'd .harness.json silently loading as empty), so it is
	// the path worth naming when present.
	Path string
	// Processes, MCPServers, and Plugins are the merged config's declared
	// counts — the "how much did this actually load" half of the log
	// line, printed alongside Path so a config file that loaded but
	// declares nothing (e.g. a typo inside a key, or an empty object) is
	// still visibly distinguishable from a rich one.
	Processes  int
	MCPServers int
	Plugins    int
	// SessionSync is the merged config's SessionSync value, verbatim ("",
	// "fsync", or "volume") — carried here so the boot log line (cmd/harness's
	// configlog.go) can call out non-default durability behavior without a
	// separate config read.
	SessionSync string
}

// LoadProjectWithInfo is LoadProject plus the LoadInfo an operator-facing
// boot log needs. It performs exactly the same file reads and merge/
// validate pass as LoadProject (this is the only implementation; that
// function is now a thin wrapper) — checking existence costs one extra
// os.Stat per file, negligible against the startup budget's "at most two
// file reads" (a Stat is not the read the budget is about).
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

// fileExists reports whether path names a file (or anything else) that
// os.Stat can see — used only to distinguish "this config layer was
// absent" from "this config layer parsed to a zero value" for LoadInfo;
// Load already treats a missing file as an empty layer, so this never
// changes what gets loaded, only what gets reported.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
	if over.ContextWindowTokens != 0 {
		out.ContextWindowTokens = over.ContextWindowTokens
	}
	if over.CompactionThreshold != 0 {
		out.CompactionThreshold = over.CompactionThreshold
	}
	if over.CompactionKeepTurns != 0 {
		out.CompactionKeepTurns = over.CompactionKeepTurns
	}
	if over.SessionSync != "" {
		out.SessionSync = over.SessionSync
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
			// Deep-copy ExtraHeaders here too: a base-only key (never
			// touched by the loop below, e.g. no matching over.Providers
			// entry) would otherwise leave m[k] aliasing base's map, so
			// mutating the merged config's headers would corrupt base's.
			if len(v.ExtraHeaders) > 0 {
				hm := make(map[string]string, len(v.ExtraHeaders))
				for hk, hv := range v.ExtraHeaders {
					hm[hk] = hv
				}
				v.ExtraHeaders = hm
			}
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
	if n := len(base.MCPServers) + len(over.MCPServers); n > 0 {
		m := make(map[string]MCPServerSpec, n)
		for k, v := range base.MCPServers {
			m[k] = copyMCPServerSpec(v)
		}
		for k, v := range over.MCPServers {
			// A same-name project entry replaces the user entry wholesale
			// (see the MCPServers field doc) rather than merging field by
			// field.
			m[k] = copyMCPServerSpec(v)
		}
		out.MCPServers = m
	} else {
		out.MCPServers = nil
	}
	if n := len(base.Processes) + len(over.Processes); n > 0 {
		m := make(map[string]ProcessSpec, n)
		for k, v := range base.Processes {
			m[k] = copyProcessSpec(v)
		}
		for k, v := range over.Processes {
			// A same-name project entry replaces the user entry wholesale
			// (see the Processes field doc), same as MCPServers.
			m[k] = copyProcessSpec(v)
		}
		out.Processes = m
	} else {
		out.Processes = nil
	}
	return &out
}

// copyProcessSpec deep-copies s's slice fields so a merged config never
// aliases either input layer's — mutating the merged result must not be
// able to corrupt base or over.
func copyProcessSpec(s ProcessSpec) ProcessSpec {
	if len(s.Command) > 0 {
		s.Command = append([]string(nil), s.Command...)
	}
	if len(s.Env) > 0 {
		s.Env = append([]string(nil), s.Env...)
	}
	if len(s.Ports) > 0 {
		s.Ports = append([]int(nil), s.Ports...)
	}
	return s
}

// copyMCPServerSpec deep-copies s's slice/map fields so a merged config
// never aliases either input layer's — mutating the merged result must not
// be able to corrupt base or over.
func copyMCPServerSpec(s MCPServerSpec) MCPServerSpec {
	if len(s.Command) > 0 {
		s.Command = append([]string(nil), s.Command...)
	}
	if len(s.Env) > 0 {
		s.Env = append([]string(nil), s.Env...)
	}
	if len(s.Headers) > 0 {
		hm := make(map[string]string, len(s.Headers))
		for k, v := range s.Headers {
			hm[k] = v
		}
		s.Headers = hm
	}
	return s
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
