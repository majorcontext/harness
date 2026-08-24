package main

import (
	"testing"
	"time"

	"github.com/majorcontext/harness/config"
)

func TestBuildMCPManagerEmpty(t *testing.T) {
	mgr := buildMCPManager(nil)
	if mgr != nil {
		t.Errorf("buildMCPManager(nil) = %v, want nil", mgr)
	}
	if reg := mcpRegistry(mgr); reg != nil {
		t.Errorf("mcpRegistry(nil manager) = %v, want a true nil interface", reg)
	}
	// A nil manager must be safe to close.
	closeMCPManager(mgr)
}

func TestBuildMCPManagerConvertsSpecs(t *testing.T) {
	mgr := buildMCPManager(map[string]config.MCPServerSpec{
		"fs": {Command: []string{"mcp-fs"}, Env: []string{"A=1"}, Dir: "/tmp"},
		"weather": {URL: "https://weather.example/mcp", Headers: map[string]string{
			"Authorization": "Bearer tok",
		}},
	})
	if mgr == nil {
		t.Fatal("buildMCPManager returned nil for a non-empty servers map")
	}
	if reg := mcpRegistry(mgr); reg == nil {
		t.Fatal("mcpRegistry(non-nil manager) returned a nil interface")
	}
	// Construction alone must touch neither network nor disk (connecting
	// happens lazily on first use — see engine.MCPManager); Close before any
	// use must still be clean.
	closeMCPManager(mgr)
}

// TestMCPServerConfigConnectTimeout is invariant 1's round-trip test:
// connect_timeout_s threads through to engine.MCPServerConfig.ConnectTimeout,
// and an absent (zero) value stays zero rather than picking some non-zero
// default here — the engine itself, not this conversion, owns the
// zero-means-default-15s policy (see engine.defaultMCPConnectTimeout,
// already covered by TestMCPManagerConnectTimeoutFailsOpen).
func TestMCPServerConfigConnectTimeout(t *testing.T) {
	got := mcpServerConfig(config.MCPServerSpec{URL: "https://x", ConnectTimeoutS: 5})
	if got.ConnectTimeout != 5*time.Second {
		t.Errorf("ConnectTimeout = %v, want 5s", got.ConnectTimeout)
	}

	got = mcpServerConfig(config.MCPServerSpec{URL: "https://x"})
	if got.ConnectTimeout != 0 {
		t.Errorf("ConnectTimeout = %v, want 0 (absent, engine applies its own default)", got.ConnectTimeout)
	}
}
