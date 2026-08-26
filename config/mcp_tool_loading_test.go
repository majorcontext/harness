package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMCPToolLoadingAcceptedValues(t *testing.T) {
	for _, v := range []string{"", "eager", "auto", "lazy"} {
		t.Run(fmt.Sprintf("%q", v), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.json")
			body := `{}`
			if v != "" {
				body = fmt.Sprintf(`{"mcp_tool_loading": %q}`, v)
			}
			writeFile(t, p, body)
			c, err := Load(p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.MCPToolLoading != v {
				t.Errorf("MCPToolLoading = %q, want %q", c.MCPToolLoading, v)
			}
		})
	}
}

func TestLoadMCPToolLoadingRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, `{"mcp_tool_loading": "lazily"}`)
	if _, err := Load(p); err == nil {
		t.Fatal("Load accepted an unrecognized mcp_tool_loading value")
	} else if !strings.Contains(err.Error(), "mcp_tool_loading") {
		t.Errorf("error %q does not name mcp_tool_loading", err.Error())
	}
}

// TestLoadMCPToolLoadingThresholdRejectsNegative guards the always-defer
// bug: len(catalog) > -1 holds even for an empty catalog, so a stray minus
// sign would silently defer everything under "auto".
func TestLoadMCPToolLoadingThresholdRejectsNegative(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, `{"mcp_tool_loading": "auto", "mcp_tool_loading_threshold": -1}`)
	if _, err := Load(p); err == nil {
		t.Fatal("Load accepted a negative mcp_tool_loading_threshold")
	} else if !strings.Contains(err.Error(), "mcp_tool_loading_threshold") {
		t.Errorf("error %q does not name mcp_tool_loading_threshold", err.Error())
	}
}

func TestLoadMCPServerToolLoading(t *testing.T) {
	t.Run("accepted values", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.json")
		writeFile(t, p, `{"mcp_servers": {"a": {"url": "http://x", "tool_loading": "lazy"},
		                                  "b": {"url": "http://y", "tool_loading": "eager"},
		                                  "c": {"url": "http://z"}}}`)
		c, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		for name, want := range map[string]string{"a": "lazy", "b": "eager", "c": ""} {
			if got := c.MCPServers[name].ToolLoading; got != want {
				t.Errorf("mcp_servers.%s.tool_loading = %q, want %q", name, got, want)
			}
		}
	})

	// "auto" is global-only: the threshold it selects measures whole-catalog
	// pressure, which is not a property of one server.
	t.Run("rejects auto", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.json")
		writeFile(t, p, `{"mcp_servers": {"a": {"url": "http://x", "tool_loading": "auto"}}}`)
		if _, err := Load(p); err == nil {
			t.Fatal("Load accepted per-server tool_loading auto")
		} else if !strings.Contains(err.Error(), "global-only") {
			t.Errorf("error %q does not explain that auto is global-only", err.Error())
		}
	})

	t.Run("rejects garbage", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.json")
		writeFile(t, p, `{"mcp_servers": {"a": {"url": "http://x", "tool_loading": "lazyish"}}}`)
		if _, err := Load(p); err == nil {
			t.Fatal("Load accepted an unrecognized per-server tool_loading value")
		} else if !strings.Contains(err.Error(), "tool_loading") {
			t.Errorf("error %q does not name tool_loading", err.Error())
		}
	})
}

func TestMergeMCPToolLoading(t *testing.T) {
	t.Run("empty project inherits user", func(t *testing.T) {
		base := &Config{MCPToolLoading: "lazy", MCPToolLoadingThreshold: 7}
		got := merge(base, &Config{})
		if got.MCPToolLoading != "lazy" || got.MCPToolLoadingThreshold != 7 {
			t.Errorf("merged = (%q, %d), want inherited (\"lazy\", 7)", got.MCPToolLoading, got.MCPToolLoadingThreshold)
		}
	})
	t.Run("non-empty project overrides", func(t *testing.T) {
		base := &Config{MCPToolLoading: "lazy", MCPToolLoadingThreshold: 7}
		got := merge(base, &Config{MCPToolLoading: "auto", MCPToolLoadingThreshold: 30})
		if got.MCPToolLoading != "auto" || got.MCPToolLoadingThreshold != 30 {
			t.Errorf("merged = (%q, %d), want project (\"auto\", 30)", got.MCPToolLoading, got.MCPToolLoadingThreshold)
		}
	})
	// A same-name project entry replaces the user entry wholesale, so the
	// per-server override travels with it and never aliases either layer.
	t.Run("per-server override survives the merge", func(t *testing.T) {
		base := &Config{MCPServers: map[string]MCPServerSpec{"a": {URL: "http://x", ToolLoading: "lazy"}}}
		got := merge(base, &Config{})
		if got.MCPServers["a"].ToolLoading != "lazy" {
			t.Errorf("tool_loading = %q, want inherited \"lazy\"", got.MCPServers["a"].ToolLoading)
		}
	})
}
