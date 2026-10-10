package config

import (
	"fmt"
	"slices"
)

// TypeOpenAICompat selects the generic OpenAI-compatible chat-completions adapter
// (internal/provider/openaicompat) for a Provider config entry — the wire format spoken by OpenRouter,
// Ollama, vLLM, and similar deployments.
const TypeOpenAICompat = "openai-compat"

// TypeOpenAI selects the native OpenAI Responses API adapter (internal/provider/openai) for a Provider
// config entry under ANY providers map key.
const TypeOpenAI = "openai"

// TypeClaudeCodeCLI selects the Claude Code CLI backend, which runs the `claude` binary as a child process and needs no BaseURL.
const TypeClaudeCodeCLI = "claude-code-cli"

// nativeProviderKeys are the only providers map keys allowed an empty Type with no further
// defaulting: the built-in adapters cmd/harness's registry wires directly by name
// (internal/provider/anthropic.Family and internal/provider/openai.Family).
var nativeProviderKeys = map[string]bool{
	"anthropic": true,
	"openai":    true,
}

// Provider is per-family provider configuration.
type Provider struct {
	// Type selects the adapter to build for this entry.
	Type string `json:"type,omitempty"`
	// APIKeyEnv names the environment variable to read the API key from.
	APIKeyEnv string `json:"api_key_env,omitempty"`
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
	// OmitResponseParams names Responses params to leave off the wire; each is in OmitResponseParamValues.
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
	// PermissionMode is the --permission-mode of the CLI, one of ClaudeCodePermissionModeValues.
	PermissionMode string `json:"permission_mode,omitempty"`
	// SessionMirror runs the CLI with --session-mirror under a scratch CLAUDE_CONFIG_DIR, so the
	// session state holds the transcript and another host can resume it.
	SessionMirror bool `json:"session_mirror,omitempty"`
}

// ClaudeCodePermissionModeValues returns the accepted --permission-mode values, in a fresh slice.
func ClaudeCodePermissionModeValues() []string {
	return []string{
		"default",
		"acceptEdits",
		"bypassPermissions",
		"plan",
	}
}

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

// OmitResponseParamValues returns every value validateOmitResponseParams accepts, in a fresh slice.
func OmitResponseParamValues() []string {
	return []string{
		OmitResponseParamMaxOutputTokens,
		OmitResponseParamTemperature,
		OmitResponseParamTopP,
		OmitResponseParamMetadata,
	}
}

// validateProviders rejects an unknown Type, an empty Type on a key that is not native or defaulted, and a missing BaseURL.
func validateProviders(providers map[string]Provider) error {
	for name, p := range providers {
		switch p.Type {
		case "":
			if !nativeProviderKeys[name] {
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
		if p.PermissionMode != "" && !slices.Contains(ClaudeCodePermissionModeValues(), p.PermissionMode) {
			return fmt.Errorf("providers.%s: unknown permission_mode %q (valid values: %q)", name, p.PermissionMode, ClaudeCodePermissionModeValues())
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
// adapter (internal/provider/openai) — the one adapter that reads ResponsesPath.
func buildsResponsesAdapter(name string, p Provider) bool {
	if p.Type == TypeOpenAI {
		return true
	}
	return p.Type == "" && name == "openai"
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
		if !slices.Contains(OmitResponseParamValues(), param) {
			return fmt.Errorf("providers.%s: unknown omit_response_params entry %q (valid values: %q)", name, param, OmitResponseParamValues())
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
	if !slices.Contains(CacheTTLValues(), p.CacheTTL) {
		return fmt.Errorf("providers.%s: unknown cache_ttl %q (valid values: %q, %q)", name, p.CacheTTL, CacheTTL5m, CacheTTL1h)
	}
	return nil
}

// CacheTTLValues returns every non-empty cache_ttl validateCacheTTL accepts, in a fresh slice.
func CacheTTLValues() []string {
	return []string{CacheTTL5m, CacheTTL1h}
}
