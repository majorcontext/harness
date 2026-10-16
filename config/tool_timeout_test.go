package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestToolTimeoutS(t *testing.T) {
	const server = `"mcp_servers": {"weather": {"url": "https://weather.example/mcp"%s}}`
	const plugin = `"plugins": [{"name": "p", "command": ["p"]%s}]`
	tests := []struct {
		name    string
		body    string
		want    time.Duration
		wantErr string
	}{
		{name: "mcp server absent", body: strings.Replace(server, "%s", "", 1), want: 120 * time.Second},
		{name: "mcp server set", body: strings.Replace(server, "%s", `, "tool_timeout_s": 7`, 1), want: 7 * time.Second},
		{name: "mcp server zero", body: strings.Replace(server, "%s", `, "tool_timeout_s": 0`, 1), wantErr: "mcp_servers.weather: tool_timeout_s must be positive (got 0)"},
		{name: "mcp server negative", body: strings.Replace(server, "%s", `, "tool_timeout_s": -3`, 1), wantErr: "mcp_servers.weather: tool_timeout_s must be positive (got -3)"},
		{name: "plugin absent", body: strings.Replace(plugin, "%s", "", 1), want: 120 * time.Second},
		{name: "plugin set", body: strings.Replace(plugin, "%s", `, "tool_timeout_s": 9`, 1), want: 9 * time.Second},
		{name: "plugin zero", body: strings.Replace(plugin, "%s", `, "tool_timeout_s": 0`, 1), wantErr: "plugins[0] (p): tool_timeout_s must be positive (got 0)"},
		{name: "plugin negative", body: strings.Replace(plugin, "%s", `, "tool_timeout_s": -1`, 1), wantErr: "plugins[0] (p): tool_timeout_s must be positive (got -1)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.json")
			writeFile(t, p, "{"+tc.body+"}")
			c, err := Load(p)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := c.MCPServers["weather"].ToolTimeout()
			if len(c.Plugins) > 0 {
				got = c.Plugins[0].ToolTimeout()
			}
			if got != tc.want {
				t.Errorf("ToolTimeout = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestToolTimeoutSurvivesAMerge(t *testing.T) {
	five := 5
	base := &Config{MCPServers: map[string]MCPServerSpec{"fs": {Command: []string{"fs"}, ToolTimeoutS: &five}}}
	merged := merge(base, &Config{})
	if got := merged.MCPServers["fs"].ToolTimeout(); got != 5*time.Second {
		t.Errorf("merged ToolTimeout = %v, want 5s", got)
	}
}
