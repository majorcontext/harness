package main

import (
	"testing"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
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

// TestMCPToolLoadingByServer proves the per-server tool_loading override
// reaches engine.Config, which is where the session-side policy lives (see
// engine.Config.MCPToolLoadingByServer for why it rides Config rather than
// the shared manager).
func TestMCPToolLoadingByServer(t *testing.T) {
	got := mcpToolLoadingByServer(map[string]config.MCPServerSpec{
		"lazy":    {URL: "http://x", ToolLoading: "lazy"},
		"eager":   {URL: "http://y", ToolLoading: "eager"},
		"inherit": {URL: "http://z"},
	})
	if len(got) != 2 {
		t.Fatalf("got %d overrides %v, want 2 (the inheriting server contributes none)", len(got), got)
	}
	if got["lazy"] != engine.MCPToolLoadingLazy || got["eager"] != engine.MCPToolLoadingEager {
		t.Fatalf("overrides = %v, want lazy/eager mapped verbatim", got)
	}
	if _, ok := got["inherit"]; ok {
		t.Fatal("a server with no tool_loading produced an override entry")
	}
}

// TestMCPToolLoadingByServerNilWhenUnused keeps a config that never mentions
// tool_loading producing exactly the zero-value engine Config it did before
// deferral existed.
func TestMCPToolLoadingByServerNilWhenUnused(t *testing.T) {
	if got := mcpToolLoadingByServer(map[string]config.MCPServerSpec{"a": {URL: "http://x"}}); got != nil {
		t.Fatalf("overrides = %v, want nil", got)
	}
	if got := mcpToolLoadingByServer(nil); got != nil {
		t.Fatalf("overrides = %v, want nil", got)
	}
}
