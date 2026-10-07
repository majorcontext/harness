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
		{name: "a sync block with an owner epoch and a token file", cfg: Config{OwnerEpoch: 3, Sync: &SyncSpec{URL: "https://b.test/sync", TokenFile: "/run/t"}}},
		{name: "an owner epoch of zero with no sync block"},
		{name: "a negative owner_epoch", cfg: Config{OwnerEpoch: -1}, want: "owner_epoch"},
		{name: "a sync block with no token_file", cfg: Config{Sync: &SyncSpec{URL: "https://b.test/sync"}}, want: "token_file"},
		{name: "a sync url that does not parse", cfg: Config{Sync: &SyncSpec{URL: "http://[::1", TokenFile: "/run/t"}}, want: "sync: url is not valid"},
		{name: "a sync url with an unsupported scheme", cfg: Config{Sync: &SyncSpec{URL: "ftp://b.test/sync", TokenFile: "/run/t"}}, want: "http or https"},
		{name: "a sync url with no host", cfg: Config{Sync: &SyncSpec{URL: "https:///sync", TokenFile: "/run/t"}}, want: "host is required"},
		{name: "a sync url with userinfo", cfg: Config{Sync: &SyncSpec{URL: "https://u:p@b.test/sync", TokenFile: "/run/t"}}, want: "userinfo"},
		{name: "a negative max_task_depth", cfg: Config{MaxTaskDepth: -1}, want: "max_task_depth"},
		{name: "a negative max_concurrent_tasks", cfg: Config{MaxConcurrentTasks: -1}, want: "max_concurrent_tasks"},
		{name: "a negative max_tree_tokens", cfg: Config{MaxTreeTokens: -1}, want: "max_tree_tokens"},
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
