package main

import (
	"testing"

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
