package config

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
)

// TypeOpenAICompat selects the generic OpenAI-compatible chat-completions adapter for a Provider
// config entry: the wire format of OpenRouter, Ollama, vLLM, and similar deployments.
const TypeOpenAICompat = "openai-compat"

// TypeOpenAI selects the native OpenAI Responses API adapter for a Provider config entry under any
// providers map key.
const TypeOpenAI = "openai"

// TypeClaudeCodeCLI selects the Claude Code CLI backend, which runs the `claude` binary as a child process and needs no BaseURL.
const TypeClaudeCodeCLI = "claude-code-cli"

// nativeProviderKeys are the only providers map keys allowed an empty Type with no further
// defaulting: the built-in adapters that the runtime wires by name. Each maps to the variable that holds
// its API key when the entry sets no api_key_env.
var nativeProviderKeys = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY",
	"openai":    "OPENAI_API_KEY",
}

// NativeProviders returns the names of the built-in providers, sorted.
func NativeProviders() []string { return slices.Sorted(maps.Keys(nativeProviderKeys)) }

// NativeKeyEnv returns the variable that holds the API key of the built-in provider name, and "" for any other provider.
func NativeKeyEnv(name string) string { return nativeProviderKeys[name] }

// WithProxyBase returns a copy of c in which every request goes to base: the native providers and each
// providers entry that sets no BaseURL get it. A Claude Code CLI entry keeps no base URL. An empty base
// returns c unchanged.
func (c Config) WithProxyBase(base string) Config {
	if base == "" {
		return c
	}
	providers := maps.Clone(c.Providers)
	if providers == nil {
		providers = map[string]Provider{}
	}
	for name := range nativeProviderKeys {
		if _, ok := providers[name]; !ok {
			providers[name] = Provider{}
		}
	}
	for name, p := range providers {
		if p.BaseURL == "" && p.Type != TypeClaudeCodeCLI {
			p.BaseURL = base
			providers[name] = p
		}
	}
	c.Providers = providers
	return c
}

// Provider is per-family provider configuration.
type Provider struct {
	// Type selects the adapter to build for this entry.
	Type string `json:"type,omitempty"`
	// APIKeyEnv names the environment variable to read the API key from.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// Auth names the credential source of the entry instead of an API key: AuthChatGPT, the ChatGPT subscription login of `harness login codex`. Valid only on an entry that builds the OpenAI Responses adapter, and not with APIKeyEnv.
	Auth string `json:"auth,omitempty"`
	// BaseURL overrides the provider's default API base URL when non-empty.
	BaseURL string `json:"base_url,omitempty"`
	// Family overrides the ProviderData tag / wire-quirk key the openaicompat adapter uses (some
	// deployments need family-specific transcoding quirks).
	Family string `json:"family,omitempty"`
	// ExtraHeaders are sent on every request by the openaicompat, anthropic, and openai adapters.
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`
	// NoPromptCacheKey omits prompt_cache_key from every request. Valid only on a TypeOpenAICompat entry.
	NoPromptCacheKey bool `json:"no_prompt_cache_key,omitempty"`
	// ResponsesPath overrides the Responses request path, "/v1/responses" when empty.
	ResponsesPath string `json:"responses_path,omitempty"`
	// OmitResponseParams names Responses params to leave off the wire; each is an OmitResponseParam constant.
	OmitResponseParams []string `json:"omit_response_params,omitempty"`
	// SanitizeToolSchemas rewrites tool schemas through an allowlist before a Responses request.
	SanitizeToolSchemas bool `json:"sanitize_tool_schemas,omitempty"`
	// UseWebSocketTransport sends the Responses calls over one pooled wss:// connection for each session.
	UseWebSocketTransport bool `json:"use_websocket_transport,omitempty"`
	// CacheTTL selects the Anthropic prompt-cache lifetime: "5m" or "1h".
	CacheTTL string `json:"cache_ttl,omitempty"`

	// BinaryPath is the Claude Code CLI executable, resolved through PATH.
	BinaryPath string `json:"binary_path,omitempty"`
	// ExtraArgs are appended after the flags that the Claude Code backend constructs.
	ExtraArgs []string `json:"extra_args,omitempty"`
	// PermissionMode is the --permission-mode of the CLI, one of default, acceptEdits, bypassPermissions, or plan.
	PermissionMode string `json:"permission_mode,omitempty"`
	// SessionMirror runs the CLI with --session-mirror under a scratch CLAUDE_CONFIG_DIR, so the
	// session state holds the transcript and another host can resume it.
	SessionMirror bool `json:"session_mirror,omitempty"`

	keyless bool
}

// Keyless reports that only the project file defines the entry, so it reads no API key. Only MergeProject sets it.
func (p Provider) Keyless() bool { return p.keyless }

// claudeCodePermissionModes returns the accepted --permission-mode values.
func claudeCodePermissionModes() []string {
	return []string{
		"default",
		"acceptEdits",
		"bypassPermissions",
		"plan",
	}
}

// UsesChatGPTLogin says that the entry signs in with the ChatGPT login of `harness login codex`.
func (p Provider) UsesChatGPTLogin() bool { return p.Auth == AuthChatGPT }

// UsesClaudeCLI says that the entry runs the Claude Code CLI.
func (p Provider) UsesClaudeCLI() bool { return p.Type == TypeClaudeCodeCLI }

// ClaudeCLI returns the executable of the Claude Code CLI that the config uses: the entry of the default model when it runs the CLI, else the first such entry in name order. ok is false when no entry runs the CLI.
func (c *Config) ClaudeCLI() (bin string, ok bool) {
	if ref, _, _ := strings.Cut(c.ResolveModel(""), "/"); c.Providers[ref].UsesClaudeCLI() {
		return c.Providers[ref].CLIBinary(), true
	}
	for _, name := range slices.Sorted(maps.Keys(c.Providers)) {
		if p := c.Providers[name]; p.UsesClaudeCLI() {
			return p.CLIBinary(), true
		}
	}
	return "", false
}

// CLIBinary is the executable of a claude-code-cli entry: BinaryPath, else claude, resolved through PATH.
func (p Provider) CLIBinary() string {
	if p.BinaryPath != "" {
		return p.BinaryPath
	}
	return "claude"
}

// AuthChatGPT is the value of Provider.Auth for the ChatGPT subscription login of `harness login codex`.
const AuthChatGPT = "chatgpt"

// Cache TTL values accepted by Provider.CacheTTL.
const (
	CacheTTL5m = "5m"
	CacheTTL1h = "1h"
)

// OmitResponseParam* name the optional OpenAI Responses request params the native openai adapter
// can conditionally omit on Provider.OmitResponseParams.
const (
	OmitResponseParamMaxOutputTokens = "max_output_tokens"
	OmitResponseParamTemperature     = "temperature"
	OmitResponseParamTopP            = "top_p"
	OmitResponseParamMetadata        = "metadata"
)

// omitResponseParams returns every value that Provider.OmitResponseParams accepts.
func omitResponseParams() []string {
	return []string{
		OmitResponseParamMaxOutputTokens,
		OmitResponseParamTemperature,
		OmitResponseParamTopP,
		OmitResponseParamMetadata,
	}
}

// validateProviders rejects an unknown Type, an empty Type on a key that is not native, and a missing BaseURL.
func validateProviders(providers map[string]Provider) error {
	for name, p := range providers {
		switch p.Type {
		case "":
			if _, ok := nativeProviderKeys[name]; !ok {
				return fmt.Errorf("providers.%s: type is required (empty type is only valid for the built-in %q/%q entries); valid types: \"\" (native anthropic/openai override), %q, %q, %q", name, "anthropic", "openai", TypeOpenAICompat, TypeOpenAI, TypeClaudeCodeCLI)
			}
			// Legacy/native provider entry (anthropic or openai); no further validation here.
		case TypeOpenAICompat, TypeOpenAI:
			if p.BaseURL == "" {
				return fmt.Errorf("providers.%s: base_url is required for type %q", name, p.Type)
			}
		case TypeClaudeCodeCLI:
			// A child process needs no base_url.
			// never dials an HTTP endpoint — see TypeClaudeCodeCLI's own doc comment.
		default:
			return fmt.Errorf("providers.%s: unknown type %q (valid types: \"\" (native anthropic/openai override), %q, %q, %q)", name, p.Type, TypeOpenAICompat, TypeOpenAI, TypeClaudeCodeCLI)
		}
		if err := validateCacheTTL(name, p); err != nil {
			return err
		}
		if p.NoPromptCacheKey && p.Type != TypeOpenAICompat {
			return fmt.Errorf("providers.%s: no_prompt_cache_key is only valid on a %q entry (only the openaicompat adapter reads it)", name, TypeOpenAICompat)
		}
		if err := validateAuth(name, p); err != nil {
			return err
		}
		if err := validateResponsesPath(name, p); err != nil {
			return err
		}
		if err := validateOmitResponseParams(name, p); err != nil {
			return err
		}
		if err := validateSanitizeToolSchemas(name, p); err != nil {
			return err
		}
		if err := validateUseWebSocketTransport(name, p); err != nil {
			return err
		}
		if err := validateClaudeCodeFields(name, p); err != nil {
			return err
		}
	}
	return nil
}

// validateClaudeCodeFields rejects a Claude Code field on an entry of another type, and an unknown
// PermissionMode.
func validateClaudeCodeFields(name string, p Provider) error {
	if p.Type == TypeClaudeCodeCLI {
		if p.PermissionMode != "" && !slices.Contains(claudeCodePermissionModes(), p.PermissionMode) {
			return fmt.Errorf("providers.%s: unknown permission_mode %q (valid values: %q)", name, p.PermissionMode, claudeCodePermissionModes())
		}
		return nil
	}
	if p.BinaryPath != "" {
		return fmt.Errorf("providers.%s: binary_path is only valid on a %q entry (only the Claude Code CLI backend spawns a process)", name, TypeClaudeCodeCLI)
	}
	if len(p.ExtraArgs) > 0 {
		return fmt.Errorf("providers.%s: extra_args is only valid on a %q entry", name, TypeClaudeCodeCLI)
	}
	if p.PermissionMode != "" {
		return fmt.Errorf("providers.%s: permission_mode is only valid on a %q entry", name, TypeClaudeCodeCLI)
	}
	if p.SessionMirror {
		return fmt.Errorf("providers.%s: session_mirror is only valid on a %q entry", name, TypeClaudeCodeCLI)
	}
	return nil
}

// buildsResponsesAdapter reports whether a providers entry builds the native OpenAI Responses
// adapter, the one adapter that reads ResponsesPath.
func buildsResponsesAdapter(name string, p Provider) bool {
	if p.Type == TypeOpenAI {
		return true
	}
	return p.Type == "" && name == "openai"
}

// validateAuth rejects an unknown auth, auth on an entry that does not build the Responses adapter,
// auth with api_key_env, and a base_url that would send the login over plain HTTP to a remote host.
func validateAuth(name string, p Provider) error {
	if p.Auth == "" {
		return nil
	}
	switch {
	case p.Auth != AuthChatGPT:
		return fmt.Errorf("providers.%s: unknown auth %q (valid value: %q)", name, p.Auth, AuthChatGPT)
	case !buildsResponsesAdapter(name, p):
		return fmt.Errorf("providers.%s: auth is only valid on an entry that builds the OpenAI Responses adapter (map key %q with no type, or any key with type %q); no other adapter reads it", name, "openai", TypeOpenAI)
	case p.APIKeyEnv != "":
		return fmt.Errorf("providers.%s: auth and api_key_env are mutually exclusive", name)
	}
	if err := validateSecureURL(p.BaseURL); err != nil {
		return fmt.Errorf("providers.%s: base_url with auth %q: %w", name, p.Auth, err)
	}
	return nil
}

// validateSecureURL accepts an empty URL, an https URL, and an http URL on a loopback host.
func validateSecureURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return errors.New("the URL must be absolute and carry no user information")
	}
	if u.Scheme == "https" {
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); u.Scheme == "http" && (ip != nil && ip.IsLoopback() || u.Hostname() == "localhost") {
		return nil
	}
	return errors.New("the URL must be https, or http on a loopback host")
}

// validateResponsesPath rejects responses_path on an entry that does not build the Responses adapter.
func validateResponsesPath(name string, p Provider) error {
	if p.ResponsesPath == "" {
		return nil
	}
	if !buildsResponsesAdapter(name, p) {
		return fmt.Errorf("providers.%s: responses_path is only valid on an entry that builds the OpenAI Responses adapter (map key %q with no type, or any key with type %q); no other adapter reads it", name, "openai", TypeOpenAI)
	}
	return nil
}

// validateUseWebSocketTransport rejects use_websocket_transport on an entry that does not build the Responses adapter.
func validateUseWebSocketTransport(name string, p Provider) error {
	if !p.UseWebSocketTransport {
		return nil
	}
	if !buildsResponsesAdapter(name, p) {
		return fmt.Errorf("providers.%s: use_websocket_transport is only valid on an entry that builds the OpenAI Responses adapter (map key %q with no type, or any key with type %q); no other adapter reads it", name, "openai", TypeOpenAI)
	}
	return nil
}

// validateOmitResponseParams rejects an unknown name, and any value on an entry that does not build the Responses adapter.
func validateOmitResponseParams(name string, p Provider) error {
	if len(p.OmitResponseParams) == 0 {
		return nil
	}
	if !buildsResponsesAdapter(name, p) {
		return fmt.Errorf("providers.%s: omit_response_params is only valid on an entry that builds the OpenAI Responses adapter (map key %q with no type, or any key with type %q); no other adapter reads it", name, "openai", TypeOpenAI)
	}
	for _, param := range p.OmitResponseParams {
		if !slices.Contains(omitResponseParams(), param) {
			return fmt.Errorf("providers.%s: unknown omit_response_params entry %q (valid values: %q)", name, param, omitResponseParams())
		}
	}
	return nil
}

// validateSanitizeToolSchemas rejects sanitize_tool_schemas on an entry that does not build the Responses adapter.
func validateSanitizeToolSchemas(name string, p Provider) error {
	if !p.SanitizeToolSchemas {
		return nil
	}
	if !buildsResponsesAdapter(name, p) {
		return fmt.Errorf("providers.%s: sanitize_tool_schemas is only valid on an entry that builds the OpenAI Responses adapter (map key %q with no type, or any key with type %q); no other adapter reads it", name, "openai", TypeOpenAI)
	}
	return nil
}

// validateCacheTTL rejects an unknown cache_ttl, and any value outside the native anthropic entry.
func validateCacheTTL(name string, p Provider) error {
	if p.CacheTTL == "" {
		return nil
	}
	if name != "anthropic" || p.Type != "" {
		return fmt.Errorf("providers.%s: cache_ttl is only valid on the native %q provider (map key %q with no type); only the anthropic adapter sets cache_control", name, "anthropic", "anthropic")
	}
	if !slices.Contains(cacheTTLs(), p.CacheTTL) {
		return fmt.Errorf("providers.%s: unknown cache_ttl %q (valid values: %q, %q)", name, p.CacheTTL, CacheTTL5m, CacheTTL1h)
	}
	return nil
}

// cacheTTLs returns every non-empty value that Provider.CacheTTL accepts.
func cacheTTLs() []string {
	return []string{CacheTTL5m, CacheTTL1h}
}
