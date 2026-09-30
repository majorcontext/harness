package config

import "testing"

func TestAccountRoutingRequiresExplicitSupportedProvider(t *testing.T) {
	for _, tc := range []struct {
		name      string
		key       string
		provider  Provider
		wantError bool
	}{
		{name: "codex responses", key: "codex", provider: Provider{Type: TypeOpenAI, BaseURL: "https://chatgpt.com/backend-api/codex", AccountRouting: &AccountRouting{Vendor: "codex", ProxyURLEnv: "HTTPS_PROXY", Protocol: "boxes-v1"}}},
		{name: "codex explicit default port", key: "codex", provider: Provider{Type: TypeOpenAI, BaseURL: "https://chatgpt.com:443/backend-api/codex", AccountRouting: &AccountRouting{Vendor: "codex", ProxyURLEnv: "HTTPS_PROXY", Protocol: "boxes-v1"}}},
		{name: "codex HTTP origin rejected", key: "codex", provider: Provider{Type: TypeOpenAI, BaseURL: "http://chatgpt.com/backend-api/codex", AccountRouting: &AccountRouting{Vendor: "codex", ProxyURLEnv: "HTTPS_PROXY", Protocol: "boxes-v1"}}, wantError: true},
		{name: "codex wrong origin rejected", key: "codex", provider: Provider{Type: TypeOpenAI, BaseURL: "https://api.openai.com/v1", AccountRouting: &AccountRouting{Vendor: "codex", ProxyURLEnv: "HTTPS_PROXY", Protocol: "boxes-v1"}}, wantError: true},
		{name: "codex non-default port rejected", key: "codex", provider: Provider{Type: TypeOpenAI, BaseURL: "https://chatgpt.com:8443/backend-api/codex", AccountRouting: &AccountRouting{Vendor: "codex", ProxyURLEnv: "HTTPS_PROXY", Protocol: "boxes-v1"}}, wantError: true},
		{name: "claude cli", key: "claude-code", provider: Provider{Type: TypeClaudeCodeCLI, AccountRouting: &AccountRouting{Vendor: "claude", ProxyURLEnv: "HTTPS_PROXY", Protocol: "boxes-v1"}}},
		{name: "openai adapter is not implicit", key: "custom", provider: Provider{Type: TypeOpenAI, BaseURL: "https://example.test", AccountRouting: &AccountRouting{Vendor: "codex", ProxyURLEnv: "HTTPS_PROXY", Protocol: "boxes-v1"}}, wantError: true},
		{name: "unknown protocol", key: "claude-code", provider: Provider{Type: TypeClaudeCodeCLI, AccountRouting: &AccountRouting{Vendor: "claude", ProxyURLEnv: "HTTPS_PROXY", Protocol: "v2"}}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProviders(map[string]Provider{tc.key: tc.provider})
			if (err != nil) != tc.wantError {
				t.Fatalf("validateProviders error = %v, wantError %v", err, tc.wantError)
			}
		})
	}
}
