package main

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/config"
)

func TestAccountRoutingForDefersMissingProxyEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")
	cfg := &config.Config{Providers: map[string]config.Provider{
		"codex": {
			Type:    config.TypeOpenAI,
			BaseURL: "https://chatgpt.com/backend-api/codex",
			AccountRouting: &config.AccountRouting{
				Vendor:      "codex",
				ProxyURLEnv: "HTTPS_PROXY",
				Protocol:    "boxes-v1",
			},
		},
	}}
	routes, err := accountRoutingFor(cfg)
	if err != nil {
		t.Fatalf("unselected account route failed during startup: %v", err)
	}
	if routes["codex"].ProxyURLEnv != "HTTPS_PROXY" {
		t.Fatalf("proxy URL environment name = %q", routes["codex"].ProxyURLEnv)
	}
	if _, err := routes["codex"].ProxyURLResolver(); err == nil || strings.Contains(err.Error(), "proxy.invalid") {
		t.Fatalf("lazy proxy environment error = %v", err)
	}
}
