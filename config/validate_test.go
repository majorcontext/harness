package config

import (
	"maps"
	"reflect"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	cc := map[string]Provider{"cc": {Type: TypeClaudeCodeCLI, ExtraArgs: []string{"--append-system-prompt"}}}
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "the zero config"},
		{name: "an openrouter entry that names only api_key_env", cfg: Config{Providers: map[string]Provider{"openrouter": {APIKeyEnv: "X"}}}},
		{name: "an openai entry without base_url", cfg: Config{Providers: map[string]Provider{"work": {Type: TypeOpenAI}}}, want: "base_url is required"},
		{name: "an unknown provider type", cfg: Config{Providers: map[string]Provider{"work": {Type: "bogus"}}}, want: "unknown type"},
		{name: "a plugin without command", cfg: Config{Plugins: []PluginSpec{{Name: "p"}}}, want: "plugins[0] (p): command is required"},
		{name: "an MCP server with no transport", cfg: Config{MCPServers: map[string]MCPServerSpec{"m": {}}}, want: "mcp_servers.m: exactly one of"},
		{name: "a process with two ready gates", cfg: Config{Processes: map[string]ProcessSpec{"web": {Command: []string{"x"}, ReadyPort: 80, ReadyRegex: "up"}}}, want: "at most one of"},
		{name: "an unknown session_sync", cfg: Config{SessionSync: "bogus"}, want: "session_sync"},
		{name: "an unknown mcp_tool_loading", cfg: Config{MCPToolLoading: "bogus"}, want: "mcp_tool_loading"},
		{name: "a negative event_sink flush_ms", cfg: Config{EventSink: &EventSinkSpec{URL: "https://e.test", FlushMS: -1}}, want: "flush_ms"},
		{name: "an append prompt that extra_args replaces", cfg: Config{AppendSystemPrompt: []string{"fact"}, Providers: cc}, want: "conflicts with append_system_prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := maps.Clone(tc.cfg.Providers)
			err := tc.cfg.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.want != "" && err == nil:
				t.Fatalf("Validate() got nil, want error containing %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.want)
			}
			if !reflect.DeepEqual(tc.cfg.Providers, before) {
				t.Errorf("Validate changed Providers to %+v, want %+v", tc.cfg.Providers, before)
			}
		})
	}
}
