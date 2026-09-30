package config

import "testing"

func TestAccountRoutingRequiresExplicitSupportedProvider(t *testing.T) {
	for _, tc := range []struct {
		name      string
		key       string
		provider  Provider
		wantError bool
	}{
		{name: "codex responses", key: "codex", provider: Provider{Type: TypeOpenAI, BaseURL: "https://example.test", AccountRouting: &AccountRouting{Vendor: "codex", ProxyURLEnv: "HTTPS_PROXY", Protocol: "boxes-v1"}}},
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
